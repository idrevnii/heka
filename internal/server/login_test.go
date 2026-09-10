package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// dashServer is newServer plus a stub dashboard handler, so the tests below
// can tell "auth let the request through" (200 from the stub) from "auth
// rejected it" (401 from the server).
func dashServer(t *testing.T) *Server {
	t.Helper()
	srv, _, _ := newServer(t)
	srv.SetDashboard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	return srv
}

func login(t *testing.T, srv *Server, user, password string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"user":%q,"password":%q}`, user, password)
	req := httptest.NewRequest("POST", "/dashboard/api/login", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// sessionOf returns the session cookie a recorded response set, or nil.
func sessionOf(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	return nil
}

func getAPI(srv *Server, c *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/dashboard/api/overview", nil)
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestDashboardAPIRequiresSession(t *testing.T) {
	srv := dashServer(t)
	if rec := getAPI(srv, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no cookie = %d, want 401", rec.Code)
	}
	if rec := getAPI(srv, &http.Cookie{Name: sessionCookie, Value: "made-up"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged cookie = %d, want 401", rec.Code)
	}
	// The gateway token is for API clients — it must not open the dashboard.
	req := httptest.NewRequest("GET", "/dashboard/api/overview", nil)
	req.Header.Set("Authorization", "Bearer gw-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("gateway token on dashboard = %d, want 401", rec.Code)
	}
}

func TestLoginAndLogout(t *testing.T) {
	srv := dashServer(t)

	if rec := login(t, srv, "admin", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d, want 401", rec.Code)
	}
	if rec := login(t, srv, "nobody", testPassword); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong user = %d, want 401", rec.Code)
	}

	rec := login(t, srv, "admin", testPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d: %s", rec.Code, rec.Body)
	}
	c := sessionOf(rec)
	if c == nil {
		t.Fatal("login set no session cookie")
	}
	if !c.HttpOnly || c.Path != "/dashboard" {
		t.Fatalf("cookie = %+v, want HttpOnly and Path=/dashboard", c)
	}
	if got := getAPI(srv, c); got.Code != http.StatusOK {
		t.Fatalf("api with session = %d, want 200", got.Code)
	}

	out := httptest.NewRequest("POST", "/dashboard/api/logout", nil)
	out.AddCookie(c)
	outRec := httptest.NewRecorder()
	srv.ServeHTTP(outRec, out)
	if outRec.Code != http.StatusOK {
		t.Fatalf("logout = %d", outRec.Code)
	}
	if got := getAPI(srv, c); got.Code != http.StatusUnauthorized {
		t.Fatalf("api after logout = %d, want 401", got.Code)
	}
}

// The static shell must stay reachable without a session: it is the sign-in
// form itself.
func TestDashboardShellIsPublic(t *testing.T) {
	srv := dashServer(t)
	req := httptest.NewRequest("GET", "/dashboard/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard shell = %d, want 200", rec.Code)
	}
}

// Without a configured login the dashboard is off, not open.
func TestDashboardOffWithoutCredentials(t *testing.T) {
	srv := dashServer(t)
	srv.Swap(&State{Tokens: []string{"gw-token"}})
	req := httptest.NewRequest("GET", "/dashboard/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("dashboard without credentials = %d, want 404", rec.Code)
	}
}

// Rotating the password is how you evict whoever is already signed in.
func TestPasswordChangeEndsSessions(t *testing.T) {
	srv := dashServer(t)
	c := sessionOf(login(t, srv, "admin", testPassword))
	if c == nil {
		t.Fatal("login set no session cookie")
	}

	// A reload that leaves the credentials alone keeps the session.
	srv.Swap(&State{Tokens: []string{"gw-token"}, User: "admin", PasswordHash: testHash})
	if rec := getAPI(srv, c); rec.Code != http.StatusOK {
		t.Fatalf("session after plain reload = %d, want 200", rec.Code)
	}

	// Swap only compares the hash, so any different string stands in for
	// "the password was rotated".
	srv.Swap(&State{Tokens: []string{"gw-token"}, User: "admin", PasswordHash: testHash + "-rotated"})
	if rec := getAPI(srv, c); rec.Code != http.StatusUnauthorized {
		t.Fatalf("session after password change = %d, want 401", rec.Code)
	}
}

type swapOnRead struct {
	io.Reader
	swap func()
}

func (r *swapOnRead) Read(p []byte) (int, error) {
	if r.swap != nil {
		r.swap()
		r.swap = nil
	}
	return r.Reader.Read(p)
}

func TestLoginCannotSurviveConcurrentPasswordRotation(t *testing.T) {
	srv := dashServer(t)
	body := &swapOnRead{
		Reader: strings.NewReader(fmt.Sprintf(`{"user":"admin","password":%q}`, testPassword)),
		swap: func() {
			srv.Swap(&State{User: "admin", PasswordHash: testHash + "-rotated"})
		},
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("POST", "/dashboard/api/login", body))
	if rec.Code != http.StatusUnauthorized || sessionOf(rec) != nil {
		t.Fatalf("old password minted a session after rotation: %d", rec.Code)
	}
}

func TestDashboardRejectsCrossOriginMutations(t *testing.T) {
	srv := dashServer(t)
	cookie := sessionOf(login(t, srv, "admin", testPassword))
	for _, path := range []string{"/dashboard/api/login", "/dashboard/api/logout", "/dashboard/api/status/reset"} {
		for _, origin := range []string{"https://other.example", "null", "https://example.com"} {
			r := httptest.NewRequest("POST", "http://example.com"+path, strings.NewReader(`{}`))
			r.Header.Set("Origin", origin)
			r.AddCookie(cookie)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, r)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s origin=%s returned %d", path, origin, rec.Code)
			}
		}
	}
	if rec := getAPI(srv, cookie); rec.Code != http.StatusOK {
		t.Fatal("cross-origin logout removed the valid session")
	}
	r := httptest.NewRequest("POST", "http://example.com/dashboard/api/status/reset", nil)
	r.Header.Set("Origin", "https://example.com")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-origin request behind TLS proxy rejected: %d", rec.Code)
	}
}

func TestLoginConcurrencyIsBounded(t *testing.T) {
	srv := dashServer(t)
	srv.loginMu.Lock()
	defer srv.loginMu.Unlock()
	if rec := login(t, srv, "admin", testPassword); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("busy password verifier returned %d", rec.Code)
	}
}
