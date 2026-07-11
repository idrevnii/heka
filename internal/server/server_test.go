package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/idrevnii/heka/internal/keypool"
)

func newServer(t *testing.T) (*Server, *keypool.Pool, *http.Request) {
	t.Helper()
	pool := keypool.New(keypool.Config{CooldownBase: time.Minute, CooldownMax: time.Hour},
		[]string{"key-one-aaaaaaaa"})
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Rest-Path", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	})
	srv := New(
		[]string{"gw-token"},
		map[string]http.Handler{"anthropic": echo},
		map[string]*keypool.Pool{"anthropic": pool},
		map[string]func() string{"cliproxy": func() string { return "healthy" }},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	return srv, pool, nil
}

func do(srv *Server, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestHealthzOpen(t *testing.T) {
	srv, _, _ := newServer(t)
	rec := do(srv, "GET", "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d", rec.Code)
	}
	var body struct {
		Status   string            `json:"status"`
		Sidecars map[string]string `json:"sidecars"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Status != "ok" || body.Sidecars["cliproxy"] != "healthy" {
		t.Fatalf("healthz body: %s", rec.Body)
	}
}

func TestAuthRequired(t *testing.T) {
	srv, _, _ := newServer(t)
	if rec := do(srv, "POST", "/anthropic/v1/messages", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", rec.Code)
	}
	if rec := do(srv, "POST", "/anthropic/v1/messages", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token = %d, want 401", rec.Code)
	}
	if rec := do(srv, "GET", "/status", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status without token = %d, want 401", rec.Code)
	}
}

func TestRoutingAndPrefixStrip(t *testing.T) {
	srv, _, _ := newServer(t)
	rec := do(srv, "POST", "/anthropic/v1/messages", "gw-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("routed = %d: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("X-Rest-Path"); got != "/v1/messages" {
		t.Fatalf("rest path = %q, want /v1/messages", got)
	}
	if rec := do(srv, "POST", "/nope/v1/x", "gw-token"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown route = %d, want 404", rec.Code)
	}
}

func TestXApiKeyAuth(t *testing.T) {
	srv, _, _ := newServer(t)
	req := httptest.NewRequest("POST", "/anthropic/v1/messages", nil)
	req.Header.Set("X-Api-Key", "gw-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("x-api-key auth = %d", rec.Code)
	}
}

// A valid token in one header must win even when an unrelated Bearer header
// rides along (SDKs and middlewares add their own Authorization).
func TestValidTokenNotShadowedByForeignBearer(t *testing.T) {
	srv, _, _ := newServer(t)
	req := httptest.NewRequest("POST", "/anthropic/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer some-unrelated-oauth-token")
	req.Header.Set("X-Api-Key", "gw-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("x-api-key shadowed by foreign Bearer: %d", rec.Code)
	}
}

func TestGoogHeaderAuth(t *testing.T) {
	srv, _, _ := newServer(t)
	req := httptest.NewRequest("POST", "/anthropic/v1/messages", nil)
	req.Header.Set("X-Goog-Api-Key", "gw-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("x-goog-api-key auth = %d", rec.Code)
	}
}

func TestStatusAndReset(t *testing.T) {
	srv, pool, _ := newServer(t)
	pool.ReportInvalid(0)

	rec := do(srv, "GET", "/status", "gw-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Providers map[string][]keypool.KeyStatus `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Providers["anthropic"][0].State != "disabled" {
		t.Fatalf("status body: %s", rec.Body)
	}

	if rec := do(srv, "GET", "/status/reset", "gw-token"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET reset = %d, want 405", rec.Code)
	}
	if rec := do(srv, "POST", "/status/reset?provider=nope", "gw-token"); rec.Code != http.StatusNotFound {
		t.Fatalf("reset unknown provider = %d, want 404", rec.Code)
	}
	if rec := do(srv, "POST", "/status/reset?provider=anthropic", "gw-token"); rec.Code != http.StatusOK {
		t.Fatalf("reset = %d", rec.Code)
	}
	if pool.Snapshot()[0].State != "active" {
		t.Fatal("reset did not clear disabled state")
	}
}
