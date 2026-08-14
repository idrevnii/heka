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
	"sync/atomic"

	"golang.org/x/crypto/bcrypt"

	"github.com/idrevnii/heka/internal/keypool"
	"github.com/idrevnii/heka/internal/proxy"
)

// State is everything ServeHTTP needs to route and authorize one request.
// It is treated as immutable once built: a reload builds a new State and
// swaps it in atomically via Server.Swap, so a single request never sees a
// mix of old and new routes/tokens.
type State struct {
	// Tokens authorize API clients on the proxy routes.
	Tokens []string
	// User and PasswordHash are the dashboard login; empty means no
	// dashboard. PasswordHash is bcrypt.
	User          string
	PasswordHash  string
	Routes        map[string]http.Handler
	Pools         map[string]*keypool.Pool
	SidecarStates map[string]func() string
}

type Server struct {
	state    atomic.Pointer[State]
	dash     atomic.Pointer[http.Handler]
	sessions *sessions
	log      *slog.Logger
}

// New builds a server around its initial state.
func New(st *State, log *slog.Logger) *Server {
	s := &Server{sessions: newSessions(), log: log}
	s.state.Store(st)
	return s
}

// Swap atomically replaces the state a request is routed/authorized
// against. Safe to call concurrently with ServeHTTP. Open sessions survive
// an ordinary reload but not a credential change — rotating the password is
// how you kick out whoever is already signed in.
func (s *Server) Swap(st *State) {
	if old := s.state.Load(); old.User != st.User || old.PasswordHash != st.PasswordHash {
		s.sessions.reset()
	}
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

// serveDashboard dispatches everything under /dashboard. The static shell
// (HTML/CSS/JS) carries no secrets and is served without authentication —
// it is just the login form until someone signs in. The JSON API under
// /dashboard/api/ is what actually needs protecting: it requires a session
// cookie handed out by POST /dashboard/api/login in exchange for the
// configured user and password.
func (s *Server) serveDashboard(st *State, w http.ResponseWriter, r *http.Request) {
	dash := s.dash.Load()
	if dash == nil {
		proxy.WriteError(w, http.StatusNotFound, "heka: dashboard is disabled")
		return
	}
	if st.User == "" || st.PasswordHash == "" {
		proxy.WriteError(w, http.StatusNotFound,
			"heka: dashboard is disabled — set auth.user and auth.password_hash to enable it")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// style-src needs 'unsafe-inline': the vendored CodeMirror editor injects
	// its highlighting/theme rules as inline <style> elements at runtime (a
	// StyleModule, not a stylesheet load) — standard for any JS-driven
	// editor. script-src stays 'self'-only; that's the directive that
	// actually matters against injection on a page that can act with the
	// signed-in session.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:")

	switch r.URL.Path {
	case "/dashboard/api/login":
		s.handleLogin(st, w, r)
		return
	case "/dashboard/api/logout":
		s.handleLogout(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/dashboard/api/") && !s.loggedIn(r) {
		proxy.WriteError(w, http.StatusUnauthorized, "heka: not signed in")
		return
	}
	(*dash).ServeHTTP(w, r)
}

// loggedIn reports whether the request carries a live session cookie.
func (s *Server) loggedIn(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	return err == nil && s.sessions.valid(c.Value)
}

func (s *Server) handleLogin(st *State, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		proxy.WriteError(w, http.StatusMethodNotAllowed, "heka: POST only")
		return
	}
	var body struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		proxy.WriteError(w, http.StatusBadRequest, "heka: invalid request body")
		return
	}
	// bcrypt is deliberately slow, so a wrong password costs the caller the
	// same ~100ms a right one does — that is the rate limit.
	userOK := subtle.ConstantTimeCompare([]byte(body.User), []byte(st.User)) == 1
	passOK := bcrypt.CompareHashAndPassword([]byte(st.PasswordHash), []byte(body.Password)) == nil
	if !userOK || !passOK {
		s.log.Warn("dashboard login failed", "user", body.User, "remote", r.RemoteAddr)
		proxy.WriteError(w, http.StatusUnauthorized, "heka: wrong user or password")
		return
	}

	id, err := s.sessions.create()
	if err != nil {
		proxy.WriteError(w, http.StatusInternalServerError, "heka: could not start a session")
		return
	}
	http.SetCookie(w, s.newSessionCookie(r, id, int(sessionTTL.Seconds())))
	s.log.Info("dashboard login", "user", body.User, "remote", r.RemoteAddr)
	proxy.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		proxy.WriteError(w, http.StatusMethodNotAllowed, "heka: POST only")
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.drop(c.Value)
	}
	http.SetCookie(w, s.newSessionCookie(r, "", -1))
	proxy.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// newSessionCookie scopes the cookie to /dashboard so it never rides along
// on a proxied API request, and marks it Secure whenever the browser
// reached us over TLS — directly or through a terminating reverse proxy.
func (s *Server) newSessionCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/dashboard",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
	}
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
