// Package server wires gateway auth, routing and the service endpoints.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/idrevnii/heka/internal/keypool"
	"github.com/idrevnii/heka/internal/proxy"
)

type Server struct {
	tokens        []string
	routes        map[string]http.Handler
	pools         map[string]*keypool.Pool
	sidecarStates map[string]func() string
	log           *slog.Logger
}

func New(tokens []string, routes map[string]http.Handler, pools map[string]*keypool.Pool,
	sidecarStates map[string]func() string, log *slog.Logger) *Server {
	return &Server{tokens: tokens, routes: routes, pools: pools, sidecarStates: sidecarStates, log: log}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		s.handleHealthz(w, r)
		return
	}
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		proxy.WriteError(w, http.StatusUnauthorized, "heka: missing or invalid gateway token")
		return
	}
	switch r.URL.Path {
	case "/status":
		s.handleStatus(w, r)
		return
	case "/status/reset":
		s.handleReset(w, r)
		return
	}

	seg, rest := splitRoute(r.URL.EscapedPath())
	h, ok := s.routes[seg]
	if !ok {
		proxy.WriteError(w, http.StatusNotFound, fmt.Sprintf("heka: unknown route %q", seg))
		return
	}
	r2 := r.Clone(r.Context())
	dec, err := url.PathUnescape(rest)
	if err != nil {
		dec = rest
	}
	r2.URL.Path = dec
	r2.URL.RawPath = ""
	if dec != rest {
		r2.URL.RawPath = rest
	}
	h.ServeHTTP(w, r2)
}

// splitRoute splits "/anthropic/v1/messages" into "anthropic" and "/v1/messages".
func splitRoute(escapedPath string) (seg, rest string) {
	p := strings.TrimPrefix(escapedPath, "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i], p[i:]
	}
	return p, "/"
}

func (s *Server) authorized(r *http.Request) bool {
	token := ""
	if ah := r.Header.Get("Authorization"); len(ah) > 7 && strings.EqualFold(ah[:7], "bearer ") {
		token = strings.TrimSpace(ah[7:])
	}
	if token == "" {
		token = r.Header.Get("X-Api-Key")
	}
	if token == "" {
		return false
	}
	ok := false
	for _, t := range s.tokens {
		if subtle.ConstantTimeCompare([]byte(token), []byte(t)) == 1 {
			ok = true
		}
	}
	return ok
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{"status": "ok"}
	if len(s.sidecarStates) > 0 {
		states := map[string]string{}
		for name, state := range s.sidecarStates {
			states[name] = state()
		}
		body["sidecars"] = states
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		proxy.WriteError(w, http.StatusMethodNotAllowed, "heka: GET only")
		return
	}
	providers := map[string][]keypool.KeyStatus{}
	for name, pool := range s.pools {
		providers[name] = pool.Snapshot()
	}
	body := map[string]any{"providers": providers}
	if len(s.sidecarStates) > 0 {
		states := map[string]string{}
		for name, state := range s.sidecarStates {
			states[name] = state()
		}
		body["sidecars"] = states
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		proxy.WriteError(w, http.StatusMethodNotAllowed, "heka: POST only")
		return
	}
	var reset []string
	if name := r.URL.Query().Get("provider"); name != "" {
		pool, ok := s.pools[name]
		if !ok {
			proxy.WriteError(w, http.StatusNotFound, fmt.Sprintf("heka: unknown provider %q", name))
			return
		}
		pool.Reset()
		reset = append(reset, name)
	} else {
		for name, pool := range s.pools {
			pool.Reset()
			reset = append(reset, name)
		}
	}
	s.log.Info("key state reset", "providers", reset)
	writeJSON(w, http.StatusOK, map[string]any{"reset": reset})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(body)
}
