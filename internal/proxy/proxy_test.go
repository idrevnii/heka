package proxy

import (
	"bufio"
	"context"
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

	"github.com/idrevnii/heka/internal/config"
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

func newHandler(t *testing.T, target string, keyIn config.KeyIn, keys ...string) (*Handler, *keypool.Pool) {
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
		Name:   "test",
		Target: u,
		KeyIn:  keyIn,
		Pool:   pool,
		Params: Params{
			MaxRetries:    3,
			MaxDisables:   1,
			MaxBodyBuffer: 1 << 20,
			CooldownOn:    []int{429},
			DisableOn:     []int{401, 402, 403},
		},
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
	h, _ := newHandler(t, up.srv.URL, config.KeyIn{Header: "X-Api-Key"}, key1)

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

	h, _ := newHandler(t, up.srv.URL, config.KeyIn{Header: "Authorization", Prefix: "Bearer "}, key1)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/models", nil))
	if got := up.requests[0].Header.Get("Authorization"); got != "Bearer "+key1 {
		t.Fatalf("prefixed header = %q", got)
	}

	h, _ = newHandler(t, up.srv.URL, config.KeyIn{Query: "key"}, key1)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1beta/models?alt=json", nil))
	q := up.requests[1].URL.Query()
	if q.Get("key") != key1 || q.Get("alt") != "json" {
		t.Fatalf("query injection: %s", up.requests[1].URL)
	}
}

func TestQueryKeyReplacesClientValues(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.WriteHeader(http.StatusOK)
	})
	h, _ := newHandler(t, up.srv.URL, config.KeyIn{Query: "key"}, key1)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(
		"GET", "/v1beta/models?key=client&zeta=%2B1&k%65y=other&alpha=2", nil))

	got := up.requests[0].URL
	want := "zeta=%2B1&alpha=2&key=" + key1
	if got.RawQuery != want {
		t.Fatalf("RawQuery = %q, want %q", got.RawQuery, want)
	}
	if values := got.Query()["key"]; len(values) != 1 || values[0] != key1 {
		t.Fatalf("key values = %q, want only provider key", values)
	}
}

func TestBaseURLQueryIsPreserved(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.WriteHeader(http.StatusOK)
	})
	target := up.srv.URL + "/root?api-version=2025-01-01&fixed=%2B1"
	h, _ := newHandler(t, target, config.KeyIn{Query: "key"}, key1)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(
		"GET", "/models?zeta=%2B2&alpha=3", nil))

	got := up.requests[0].URL
	if got.Path != "/root/models" {
		t.Fatalf("Path = %q, want %q", got.Path, "/root/models")
	}
	want := "api-version=2025-01-01&fixed=%2B1&zeta=%2B2&alpha=3&key=" + key1
	if got.RawQuery != want {
		t.Fatalf("RawQuery = %q, want %q", got.RawQuery, want)
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
	h, pool := newHandler(t, up.srv.URL, config.KeyIn{Header: "X-Api-Key"}, key1, key2)

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
	h, _ := newHandler(t, up.srv.URL, config.KeyIn{Header: "X-Api-Key"}, key1, key2)

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
	h, _ := newHandler(t, up.srv.URL, config.KeyIn{Header: "X-Api-Key"},
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
	h, pool := newHandler(t, up.srv.URL, config.KeyIn{Header: "X-Api-Key"}, key1, key2)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/x", strings.NewReader("{}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if snap := pool.Snapshot(); snap[0].State != "disabled" {
		t.Fatalf("key1 state = %q, want disabled", snap[0].State)
	}
}

// A request-level fault makes every key look invalid. The disable budget
// has to stop that from walking the rotation and emptying the pool.
func TestInvalidKeyDisableCapped(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	keys := []string{key1, key2, "key-c-cccccccccc", "key-d-dddddddddd"}
	h, pool := newHandler(t, up.srv.URL, config.KeyIn{Header: "X-Api-Key"}, keys...)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/x", strings.NewReader("{}")))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the upstream 401 passed through", rec.Code)
	}
	// One key is spent proving the fault is not key-specific; the second
	// 401 hits the cap and ends the loop.
	if up.count() != 2 {
		t.Fatalf("attempts = %d, want 2", up.count())
	}
	var disabled int
	for _, k := range pool.Snapshot() {
		if k.State == "disabled" {
			disabled++
		}
	}
	if disabled != 1 {
		t.Fatalf("disabled = %d of %d, want 1", disabled, len(keys))
	}
}

// The error text a disabled key carries is peeked out of the upstream
// response — which must still reach the client intact.
func TestInvalidKeyRecordsUpstreamError(t *testing.T) {
	const body = `{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, body)
	})
	h, pool := newHandler(t, up.srv.URL, config.KeyIn{Header: "X-Api-Key"}, key1)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/x", strings.NewReader("{}")))

	if rec.Body.String() != body {
		t.Fatalf("forwarded body = %q, want the upstream body unchanged", rec.Body.String())
	}
	e := pool.Snapshot()[0].LastError
	if e == nil || e.Status != http.StatusUnauthorized || e.Reason != "invalid_key" {
		t.Fatalf("last error = %+v", e)
	}
	if e.Message != body {
		t.Fatalf("recorded message = %q, want %q", e.Message, body)
	}
}

// Check is the manual "is this key actually alive?" probe: it must reach the
// upstream with that one key, ask for a single token, and — the whole reason
// it exists — bench a key the provider rejects even though heka's own
// cooldown had already expired and called it active.
func TestCheckProbesOneKey(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			io.WriteString(w, `{"choices":[]}`)
			return
		}
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"rate limited"}`)
	})
	h, pool := newHandler(t, up.srv.URL, config.KeyIn{Header: "Authorization", Prefix: "Bearer "}, key1, key2)

	status, msg, err := h.Check(context.Background(), 0, key1, "deepseek-v4-flash", "/v1/chat/completions")
	if err != nil || status != http.StatusOK || msg != `{"choices":[]}` {
		t.Fatalf("check = (%d, %q, %v)", status, msg, err)
	}
	if got := up.requests[0].URL.Path; got != "/v1/chat/completions" {
		t.Fatalf("path = %q", got)
	}
	if got := up.requests[0].Header.Get("Authorization"); got != "Bearer "+key1 {
		t.Fatalf("authorization = %q", got)
	}
	var sent struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
	}
	if err := json.Unmarshal([]byte(up.bodies[0]), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Model != "deepseek-v4-flash" || sent.MaxTokens != 1 {
		t.Fatalf("sent body = %s", up.bodies[0])
	}
	if st := pool.Snapshot()[0]; st.State != "active" || st.Successes != 1 {
		t.Fatalf("key 0 after a passing check = %+v", st)
	}

	if status, _, err = h.Check(context.Background(), 1, key2, "deepseek-v4-flash", "/v1/chat/completions"); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status)
	}
	st := pool.Snapshot()[1]
	if st.State != "cooldown" || st.LastError == nil || st.LastError.Reason != "rate_limited" {
		t.Fatalf("key 1 after a failing check = %+v, last error %+v", st, st.LastError)
	}
}

// A rate limit is a temporary state, but why it happened is still worth
// keeping — quota exhausted and per-minute throttling look identical
// otherwise.
func TestRateLimitRecordsUpstreamError(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, "rate limit exceeded")
	})
	h, pool := newHandler(t, up.srv.URL, config.KeyIn{Header: "X-Api-Key"}, key1)
	h.Params.MaxRetries = 0

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/x", strings.NewReader("{}")))

	if rec.Body.String() != "rate limit exceeded" {
		t.Fatalf("forwarded body = %q", rec.Body.String())
	}
	e := pool.Snapshot()[0].LastError
	if e == nil || e.Reason != "rate_limited" || e.Status != http.StatusTooManyRequests ||
		e.Message != "rate limit exceeded" {
		t.Fatalf("last error = %+v", e)
	}
}

// An error body longer than the peek budget must not truncate what the
// client receives.
func TestErrorPeekDoesNotTruncateResponse(t *testing.T) {
	big := strings.Repeat("e", errPeekMax*2)
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, big)
	})
	h, pool := newHandler(t, up.srv.URL, config.KeyIn{Header: "X-Api-Key"}, key1)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/x", strings.NewReader("{}")))

	if rec.Body.String() != big {
		t.Fatalf("forwarded body length = %d, want %d", rec.Body.Len(), len(big))
	}
	if e := pool.Snapshot()[0].LastError; e == nil || len(e.Message) != errPeekMax {
		t.Fatalf("recorded message length = %d, want %d", len(e.Message), errPeekMax)
	}
}

// max_disables: 0 is a deliberate "never burn a key on a 401" override.
func TestInvalidKeyDisablesNone(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	h, pool := newHandler(t, up.srv.URL, config.KeyIn{Header: "X-Api-Key"}, key1, key2)
	h.Params.MaxDisables = 0

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/x", strings.NewReader("{}")))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
	if up.count() != 1 {
		t.Fatalf("attempts = %d, want 1", up.count())
	}
	for i, k := range pool.Snapshot() {
		if k.State != "active" {
			t.Fatalf("key %d state = %q, want active", i, k.State)
		}
	}
}

// The cap must not interfere with cooldowns: 429 keeps sweeping the pool.
func TestRateLimitedNotCapped(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	h, _ := newHandler(t, up.srv.URL, config.KeyIn{Header: "X-Api-Key"},
		key1, key2, "key-c-cccccccccc", "key-d-dddddddddd")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/x", strings.NewReader("{}")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want rotation past the rate-limited keys", rec.Code)
	}
	if up.count() != 3 {
		t.Fatalf("attempts = %d, want 3", up.count())
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
	h, pool := newHandler(t, up.srv.URL, config.KeyIn{Header: "X-Api-Key"}, key1, key2)

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
	h, _ := newHandler(t, "http://127.0.0.1:1", config.KeyIn{Header: "X-Api-Key"}, key1, key2)
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
	h, pool := newHandler(t, "http://127.0.0.1:1", config.KeyIn{Header: "X-Api-Key"}, key1)
	pool.ReportInvalid(0, keypool.ErrorDetail{})
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
	h, _ := newHandler(t, up.srv.URL, config.KeyIn{Header: "X-Api-Key"}, key1, key2)
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
	h, _ := newHandler(t, up.srv.URL, config.KeyIn{Header: "X-Api-Key"}, key1)
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

// TestQueryByteForBytePassthrough: injecting a query key must not reorder or
// re-encode the client's own parameters.
func TestQueryByteForBytePassthrough(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.WriteHeader(http.StatusOK)
	})
	h, _ := newHandler(t, up.srv.URL, config.KeyIn{Query: "key"}, key1)
	h.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("GET", "/v1beta/models?zeta=%2B1&alpha=2", nil))
	want := "zeta=%2B1&alpha=2&key=" + key1
	if got := up.requests[0].URL.RawQuery; got != want {
		t.Fatalf("RawQuery = %q, want %q", got, want)
	}
}

// TestStaticKeyForSidecarUpstream: a pool-less handler with StaticKey must
// authenticate to the upstream while the gateway token is stripped.
func TestStaticKeyForSidecarUpstream(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.WriteHeader(http.StatusOK)
	})
	h, _ := newHandler(t, up.srv.URL, config.KeyIn{Header: "Authorization", Prefix: "Bearer "})
	h.StaticKey = "cliproxy-secret-key"

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"m":1}`))
	req.Header.Set("X-Api-Key", "gateway-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got := up.requests[0]
	if got.Header.Get("Authorization") != "Bearer cliproxy-secret-key" {
		t.Fatalf("static key not injected: %q", got.Header.Get("Authorization"))
	}
	if got.Header.Get("X-Api-Key") != "" {
		t.Fatal("gateway token leaked to sidecar upstream")
	}
	if up.bodies[0] != `{"m":1}` {
		t.Fatalf("body = %q (unbuffered path must still deliver it)", up.bodies[0])
	}
}

// TestClientCancelNoRotation: a client disconnect must not burn through the
// key pool or count as key failures.
func TestClientCancelNoRotation(t *testing.T) {
	started := make(chan struct{})
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			close(started)
		}
		<-r.Context().Done()
	})
	h, pool := newHandler(t, up.srv.URL, config.KeyIn{Header: "X-Api-Key"}, key1, key2)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	req := httptest.NewRequest("POST", "/x", strings.NewReader("{}")).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if up.count() != 1 {
		t.Fatalf("attempts = %d, want 1 (no rotation on client cancel)", up.count())
	}
	for i, ks := range pool.Snapshot() {
		if ks.Failures != 0 {
			t.Fatalf("key %d failures = %d, want 0", i, ks.Failures)
		}
	}
}
