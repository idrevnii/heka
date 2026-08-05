// Package server wires gateway auth, routing and the service endpoints.
package server

import (
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/idrevnii/heka/internal/keypool"
	"github.com/idrevnii/heka/internal/proxy"
)

// State is everything ServeHTTP needs to route and authorize one request.
// It is treated as immutable once built: a reload builds a new State and
// swaps it in atomically via Server.Swap, so a single request never sees a
// mix of old and new routes/tokens.
type State struct {
	Tokens        []string
	Routes        map[string]http.Handler
	Pools         map[string]*keypool.Pool
	SidecarStates map[string]func() string
}

type Server struct {
	state atomic.Pointer[State]
	dash  atomic.Pointer[http.Handler]
	log   *slog.Logger
}

func New(tokens []string, routes map[string]http.Handler, pools map[string]*keypool.Pool,
	sidecarStates map[string]func() string, log *slog.Logger) *Server {
	s := &Server{log: log}
	s.state.Store(&State{Tokens: tokens, Routes: routes, Pools: pools, SidecarStates: sidecarStates})
	return s
}

// Swap atomically replaces the state a request is routed/authorized
// against. Safe to call concurrently with ServeHTTP.
func (s *Server) Swap(st *State) {
	s.state.Store(st)
}

// SetDashboard installs (or, called with nil, removes) the handler serving
// everything under /dashboard. Safe to call concurrently with ServeHTTP.
func (s *Server) SetDashboard(h http.Handler) {
	if h == nil {
		s.dash.Store(nil)
		return
	}
	s.dash.Store(&h)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	st := s.state.Load()

	if r.URL.Path == "/healthz" {
		s.handleHealthz(st, w, r)
		return
	}
	if r.URL.Path == "/dashboard" || strings.HasPrefix(r.URL.Path, "/dashboard/") {
		s.serveDashboard(st, w, r)
		return
	}
	if !s.authorized(st, r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		proxy.WriteError(w, http.StatusUnauthorized, "heka: missing or invalid gateway token")
		return
	}
	switch r.URL.Path {
	case "/status":
		s.handleStatus(st, w, r)
		return
	case "/status/reset":
		s.handleReset(st, w, r)
		return
	}

	seg, rest := splitRoute(r.URL.EscapedPath())
	h, ok := st.Routes[seg]
	if !ok {
		proxy.WriteError(w, http.StatusNotFound, fmt.Sprintf("heka: unknown route %q", seg))
		return
	}
	// Shallow copy: only the URL changes; the proxy clones headers itself.
	r2 := new(http.Request)
	*r2 = *r
	u := *r.URL
	dec, err := url.PathUnescape(rest)
	if err != nil {
		dec = rest
	}
	u.Path = dec
	u.RawPath = ""
	if dec != rest {
		u.RawPath = rest
	}
	r2.URL = &u
	h.ServeHTTP(w, r2)
}

// serveDashboard authorizes and dispatches everything under /dashboard. In
// addition to the normal bearer-token check, GET requests may authenticate
// via a ?token= query parameter — browsers can't easily set custom headers
// for a page navigation — but that carve-out is strictly scoped to this
// prefix and to GET, so it never widens the proxy/status API's posture.
func (s *Server) serveDashboard(st *State, w http.ResponseWriter, r *http.Request) {
	dash := s.dash.Load()
	if dash == nil {
		proxy.WriteError(w, http.StatusNotFound, "heka: dashboard is disabled")
		return
	}
	ok := s.authorized(st, r)
	if !ok && r.Method == http.MethodGet {
		ok = matchToken(st.Tokens, r.URL.Query().Get("token"))
	}
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		proxy.WriteError(w, http.StatusUnauthorized, "heka: missing or invalid gateway token")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:")
	(*dash).ServeHTTP(w, r)
}

// splitRoute splits "/anthropic/v1/messages" into "anthropic" and "/v1/messages".
func splitRoute(escapedPath string) (seg, rest string) {
	seg, rest, _ = strings.Cut(strings.TrimPrefix(escapedPath, "/"), "/")
	return seg, "/" + rest
}

// authorized accepts the gateway token in any header a provider SDK would
// naturally use for its key; every candidate present is checked.
func (s *Server) authorized(st *State, r *http.Request) bool {
	var candidates []string
	if ah := r.Header.Get("Authorization"); len(ah) > 7 && strings.EqualFold(ah[:7], "bearer ") {
		candidates = append(candidates, strings.TrimSpace(ah[7:]))
	}
	for _, header := range []string{"X-Api-Key", "X-Goog-Api-Key"} {
		if v := r.Header.Get(header); v != "" {
			candidates = append(candidates, v)
		}
	}
	for _, c := range candidates {
		if matchToken(st.Tokens, c) {
			return true
		}
	}
	return false
}

// matchToken constant-time compares candidate against every configured
// token. An empty candidate never matches, even against an empty token.
func matchToken(tokens []string, candidate string) bool {
	if candidate == "" {
		return false
	}
	ok := false
	for _, t := range tokens {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(t)) == 1 {
			ok = true
		}
	}
	return ok
}

func sidecarSnapshot(st *State) map[string]string {
	if len(st.SidecarStates) == 0 {
		return nil
	}
	states := map[string]string{}
	for name, state := range st.SidecarStates {
		states[name] = state()
	}
	return states
}

func (s *Server) handleHealthz(st *State, w http.ResponseWriter, r *http.Request) {
	body := map[string]any{"status": "ok"}
	if states := sidecarSnapshot(st); states != nil {
		body["sidecars"] = states
	}
	proxy.WriteJSON(w, http.StatusOK, body)
}

func (s *Server) handleStatus(st *State, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		proxy.WriteError(w, http.StatusMethodNotAllowed, "heka: GET only")
		return
	}
	providers := map[string][]keypool.KeyStatus{}
	for name, pool := range st.Pools {
		providers[name] = pool.Snapshot()
	}
	body := map[string]any{"providers": providers}
	if states := sidecarSnapshot(st); states != nil {
		body["sidecars"] = states
	}
	proxy.WriteJSON(w, http.StatusOK, body)
}

func (s *Server) handleReset(st *State, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		proxy.WriteError(w, http.StatusMethodNotAllowed, "heka: POST only")
		return
	}
	var reset []string
	if name := r.URL.Query().Get("provider"); name != "" {
		pool, ok := st.Pools[name]
		if !ok {
			proxy.WriteError(w, http.StatusNotFound, fmt.Sprintf("heka: unknown provider %q", name))
			return
		}
		pool.Reset()
		reset = append(reset, name)
	} else {
		for name, pool := range st.Pools {
			pool.Reset()
			reset = append(reset, name)
		}
	}
	s.log.Info("key state reset", "providers", reset)
	proxy.WriteJSON(w, http.StatusOK, map[string]any{"reset": reset})
}
