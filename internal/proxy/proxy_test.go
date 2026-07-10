package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/idrevnii/heka/internal/keypool"
)

type upstream struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []string
	respond  func(w http.ResponseWriter, r *http.Request, n int)
	srv      *httptest.Server
}

func newUpstream(t *testing.T, respond func(w http.ResponseWriter, r *http.Request, n int)) *upstream {
	t.Helper()
	u := &upstream{respond: respond}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.requests = append(u.requests, r.Clone(r.Context()))
		u.bodies = append(u.bodies, string(body))
		n := len(u.requests)
		u.mu.Unlock()
		u.respond(w, r, n)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

func newHandler(t *testing.T, target string, keyIn KeyIn, keys ...string) (*Handler, *keypool.Pool) {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	var pool *keypool.Pool
	if len(keys) > 0 {
		pool = keypool.New(keypool.Config{CooldownBase: time.Minute, CooldownMax: time.Hour}, keys)
	}
	return &Handler{
		Name:      "test",
		Target:    u,
		KeyIn:     keyIn,
		Pool:      pool,
		Params:    Params{MaxRetries: 3, MaxBodyBuffer: 1 << 20},
		Transport: http.DefaultTransport,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, pool
}

const (
	key1 = "key-one-aaaaaaaa"
	key2 = "key-two-bbbbbbbb"
)

func TestPassthrough(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"ok":true}`)
	})
	h, _ := newHandler(t, up.srv.URL, KeyIn{Header: "X-Api-Key"}, key1)

	req := httptest.NewRequest("POST", "/v1/messages?beta=true", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Authorization", "Bearer gateway-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("X-Upstream") != "yes" || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("response not passed through: %v %s", rec.Header(), rec.Body)
	}
	got := up.requests[0]
	if got.URL.Path != "/v1/messages" || got.URL.Query().Get("beta") != "true" {
		t.Fatalf("upstream URL = %s", got.URL)
	}
	if got.Header.Get("X-Api-Key") != key1 {
		t.Fatalf("key not injected: %q", got.Header.Get("X-Api-Key"))
	}
	if got.Header.Get("Authorization") != "" {
		t.Fatal("gateway token leaked to upstream")
	}
	if got.Header.Get("Anthropic-Version") != "2023-06-01" || got.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("client headers lost: %v", got.Header)
	}
	if up.bodies[0] != `{"model":"m"}` {
		t.Fatalf("body = %q", up.bodies[0])
	}
}

func TestKeyPrefixAndQueryInjection(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.WriteHeader(http.StatusOK)
	})

	h, _ := newHandler(t, up.srv.URL, KeyIn{Header: "Authorization", Prefix: "Bearer "}, key1)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/models", nil))
	if got := up.requests[0].Header.Get("Authorization"); got != "Bearer "+key1 {
		t.Fatalf("prefixed header = %q", got)
	}

	h, _ = newHandler(t, up.srv.URL, KeyIn{Query: "key"}, key1)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1beta/models?alt=json", nil))
	q := up.requests[1].URL.Query()
	if q.Get("key") != key1 || q.Get("alt") != "json" {
		t.Fatalf("query injection: %s", up.requests[1].URL)
	}
}

func TestRotationOn429(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if r.Header.Get("X-Api-Key") == key1 {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	h, pool := newHandler(t, up.srv.URL, KeyIn{Header: "X-Api-Key"}, key1, key2)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader("{}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after rotation", rec.Code)
	}
	if up.count() != 2 {
		t.Fatalf("upstream requests = %d, want 2", up.count())
	}
	snap := pool.Snapshot()
	if snap[0].State != "cooldown" {
		t.Fatalf("key1 state = %q, want cooldown", snap[0].State)
	}
	if until := time.Until(*snap[0].CooldownUntil); until < 110*time.Second || until > 130*time.Second {
		t.Fatalf("Retry-After not honored: %v", until)
	}

	// Next request goes straight to key2 without touching key1.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/messages", strings.NewReader("{}")))
	if up.count() != 3 || up.requests[2].Header.Get("X-Api-Key") != key2 {
		t.Fatalf("cooling key was used again")
	}
}

func TestAllKeysExhaustedReturnsLastResponse(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":"rate limited"}`)
	})
	h, _ := newHandler(t, up.srv.URL, KeyIn{Header: "X-Api-Key"}, key1, key2)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader("{}")))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want provider's 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "rate limited") {
		t.Fatalf("provider body lost: %s", rec.Body)
	}
	if up.count() != 2 {
		t.Fatalf("attempts = %d, want one per key", up.count())
	}
}

func TestMaxRetriesBoundsAttempts(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	h, _ := newHandler(t, up.srv.URL, KeyIn{Header: "X-Api-Key"},
		"key-a-aaaaaaaaaa", "key-b-bbbbbbbbbb", "key-c-cccccccccc",
		"key-d-dddddddddd", "key-e-eeeeeeeeee", "key-f-ffffffffff")
	h.Params.MaxRetries = 2

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/x", strings.NewReader("{}")))
	if up.count() != 3 {
		t.Fatalf("attempts = %d, want max_retries+1 = 3", up.count())
	}
}

func TestInvalidKeyDisabled(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if r.Header.Get("X-Api-Key") == key1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	h, pool := newHandler(t, up.srv.URL, KeyIn{Header: "X-Api-Key"}, key1, key2)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/x", strings.NewReader("{}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if snap := pool.Snapshot(); snap[0].State != "disabled" {
		t.Fatalf("key1 state = %q, want disabled", snap[0].State)
	}
}

func TestRetryOn5xx(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	h, pool := newHandler(t, up.srv.URL, KeyIn{Header: "X-Api-Key"}, key1, key2)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/x", strings.NewReader("{}")))
	if rec.Code != http.StatusOK || up.count() != 2 {
		t.Fatalf("status = %d, attempts = %d", rec.Code, up.count())
	}
	// 5xx must not cool the key down.
	if snap := pool.Snapshot(); snap[0].State != "active" {
		t.Fatalf("key1 state = %q, want active", snap[0].State)
	}
}

func TestUnreachableUpstream(t *testing.T) {
	h, _ := newHandler(t, "http://127.0.0.1:1", KeyIn{Header: "X-Api-Key"}, key1, key2)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/x", strings.NewReader("{}")))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var body struct {
		Error struct{ Type, Message string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Type != "heka_gateway_error" {
		t.Fatalf("error body: %s", rec.Body)
	}
}

func TestAllKeysDisabledReturns503(t *testing.T) {
	h, pool := newHandler(t, "http://127.0.0.1:1", KeyIn{Header: "X-Api-Key"}, key1)
	pool.ReportInvalid(0)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/x", strings.NewReader("{}")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestOversizedBodySingleAttempt(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	h, _ := newHandler(t, up.srv.URL, KeyIn{Header: "X-Api-Key"}, key1, key2)
	h.Params.MaxBodyBuffer = 16

	big := strings.Repeat("x", 100)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/x", strings.NewReader(big)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want passthrough 429", rec.Code)
	}
	if up.count() != 1 {
		t.Fatalf("attempts = %d, want 1 (non-replayable body)", up.count())
	}
	if up.bodies[0] != big {
		t.Fatalf("oversized body corrupted: len %d", len(up.bodies[0]))
	}
}

// TestStreamingFlush proves SSE chunks reach the client before the upstream
// finishes the response.
func TestStreamingFlush(t *testing.T) {
	release := make(chan struct{})
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, "data: second\n\n")
	})
	h, _ := newHandler(t, up.srv.URL, KeyIn{Header: "X-Api-Key"}, key1)
	front := httptest.NewServer(h)
	defer front.Close()

	resp, err := front.Client().Get(front.URL + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)

	line, err := rd.ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("first chunk = %q, %v", line, err)
	}
	// First chunk arrived while the upstream is still blocked — the proxy
	// flushes as it copies.
	close(release)
	rest, _ := io.ReadAll(rd)
	if !strings.Contains(string(rest), "data: second") {
		t.Fatalf("rest = %q", rest)
	}
}
