package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/idrevnii/heka/internal/config"
	"github.com/idrevnii/heka/internal/history"
	"github.com/idrevnii/heka/internal/keypool"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "heka.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestApp(t *testing.T, content string) *App {
	t.Helper()
	path := writeConfig(t, content)
	a, err := New(Options{
		ConfigPath: path,
		Transport:  http.DefaultTransport,
		Log:        testLog(),
		History:    history.New(100, 20, 1<<20),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { a.Shutdown(2 * time.Second) })
	return a
}

func providerConfig(baseURL string, extra string) string {
	return fmt.Sprintf(`
auth:
  tokens: ["dev-token"]
providers:
  mock:
    base_url: %s
    key_in: { header: x-api-key }
    keys: ["key-one-aaaaaaaaaaaa"]
%s
`, baseURL, extra)
}

func TestApplyReusesUnchangedProviderPool(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	a := newTestApp(t, providerConfig(up.URL, ""))
	pool1 := a.providers["mock"].pool
	pool1.ReportSuccess(0)

	// Reapply the identical config: pool object identity and its counters
	// must survive.
	cfg, raw := readAndParse(t, a)
	if _, err := a.apply(cfg, raw); err != nil {
		t.Fatalf("apply: %v", err)
	}
	pool2 := a.providers["mock"].pool
	if pool1 != pool2 {
		t.Fatal("unchanged provider should reuse the same *keypool.Pool")
	}
	if pool2.Snapshot()[0].Successes != 1 {
		t.Fatalf("success counter lost across reload: %+v", pool2.Snapshot()[0])
	}
}

func TestSeedCannotOverwriteAnExistingConfig(t *testing.T) {
	seed := writeConfig(t, "seed")
	live := writeConfig(t, "live")
	if err := seedConfig(seed, live); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(live); err != nil || string(data) != "live" {
		t.Fatalf("seed overwrote live config: %q, %v", data, err)
	}
}

func TestPermanentKeyBlockSurvivesResetReloadAndRestart(t *testing.T) {
	const first, second = "key-one-aaaaaaaaaaaa", "key-two-bbbbbbbbbbbb"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != second {
			t.Error("blocked key reached the upstream")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	content := strings.Replace(providerConfig(up.URL, "    check_model: test-model"), `keys: ["`+first+`"]`, `keys: ["`+first+`", "`+second+`"]`, 1)
	a := newTestApp(t, content)
	pool := a.providers["mock"].pool
	id := pool.Snapshot()[0].ID
	pool.ReportSuccess(0)
	for range 2 { // Repeated submits are idempotent.
		if err := a.DisableStatusKey("mock", 0, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.ResetStatus(""); err != nil {
		t.Fatal(err)
	}
	if err := a.ResetStatusKey("mock", 0); err != nil {
		t.Fatal(err)
	}
	pool.ReportSuccess(0) // A response already in flight cannot re-enable it.
	if state := pool.Snapshot()[0]; state.State != "blocked" || state.Successes != 2 {
		t.Fatalf("reset or success lost block/history: %+v", state)
	}
	if _, err := a.CheckKey(context.Background(), "mock", 0); err == nil {
		t.Fatal("manual check accepted a blocked key")
	}
	if _, _, err := a.providers["mock"].handler.Check(context.Background(), 0, first, "test-model"); err == nil {
		t.Fatal("direct check accepted a blocked key")
	}
	for _, path := range []string{"/status/reset", "/status/reset?provider=mock", "/status/reset?provider=mock&key=0", "/mock/v1/chat/completions"} {
		r := httptest.NewRequest("POST", path, strings.NewReader(`{"messages":[]}`))
		r.Header.Set("Authorization", "Bearer dev-token")
		rec := httptest.NewRecorder()
		a.Server().ServeHTTP(rec, r)
		if rec.Code != 200 || pool.Snapshot()[0].State != "blocked" {
			t.Fatalf("%s: status=%d, block=%s", path, rec.Code, pool.Snapshot()[0].State)
		}
	}
	data, err := os.ReadFile(a.opts.ConfigPath + ".disabled-keys.json")
	if err != nil {
		t.Fatal(err)
	}
	var saved []string
	if err := json.Unmarshal(data, &saved); err != nil || len(saved) != 1 || saved[0] != id || strings.Contains(string(data), first) {
		t.Fatalf("expected exactly one fingerprint, no secret: %s", data)
	}

	// Reorder keys, then confirm that a stale index cannot disable its replacement.
	reordered := strings.Replace(content, `keys: ["`+first+`", "`+second+`"]`, `keys: ["`+second+`", "`+first+`"]`, 1)
	if _, err := a.ApplyBytes([]byte(reordered), ""); err != nil {
		t.Fatal(err)
	}
	if err := a.DisableStatusKey("mock", 0, id); !errors.Is(err, ErrKeyChanged) {
		t.Fatalf("stale index: %v", err)
	}
	for _, p := range []*keypool.Pool{pool, a.providers["mock"].pool} {
		if _, secret, ok := p.Acquire(nil); !ok || secret != second {
			t.Fatal("old or rebuilt pool selected a blocked key")
		}
	}

	restarted, err := New(a.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Shutdown(time.Second)
	if restarted.providers["mock"].pool.Snapshot()[1].State != "blocked" {
		t.Fatal("block lost on restart")
	}
	if err := restarted.DisableStatusKey("mock", 0, keypool.Fingerprint(second)); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/mock/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer dev-token")
	rec := httptest.NewRecorder()
	restarted.Server().ServeHTTP(rec, r)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("all blocked: status=%d, want 503", rec.Code)
	}
}

func TestPermanentKeyBlockWriteFailureAndCorruptFile(t *testing.T) {
	a := newTestApp(t, providerConfig("http://127.0.0.1:1", ""))
	path := a.opts.ConfigPath + ".disabled-keys.json"
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	id := a.providers["mock"].pool.Snapshot()[0].ID
	if err := a.DisableStatusKey("mock", 0, id); err == nil {
		t.Fatal("write failure reported success")
	}
	if a.providers["mock"].pool.Snapshot()[0].State != "active" {
		t.Fatal("failed persistence changed live state")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{`{`, `["not-a-fingerprint"]`} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if other, err := New(a.opts); err == nil {
			other.Shutdown(time.Second)
			t.Fatal("corrupt blocklist was silently ignored")
		}
	}
}

func TestApplyChangedKeysResetsPool(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	a := newTestApp(t, providerConfig(up.URL, ""))
	pool1 := a.providers["mock"].pool
	pool1.ReportSuccess(0)

	changed := fmt.Sprintf(`
auth:
  tokens: ["dev-token"]
providers:
  mock:
    base_url: %s
    key_in: { header: x-api-key }
    keys: ["different-key-aaaaaa"]
`, up.URL)
	cfg, raw := parseText(t, changed)
	if _, err := a.apply(cfg, raw); err != nil {
		t.Fatalf("apply: %v", err)
	}
	pool2 := a.providers["mock"].pool
	if pool1 == pool2 {
		t.Fatal("changed keys should produce a fresh *keypool.Pool")
	}
	if pool2.Snapshot()[0].Successes != 0 {
		t.Fatal("fresh pool should start with zero counters")
	}
}

func TestApplyUnchangedCaptureKeepsHandlerButOnlyPoolFPMatters(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	a := newTestApp(t, providerConfig(up.URL, ""))
	pool1 := a.providers["mock"].pool
	handler1 := a.providers["mock"].handler

	withCapture := providerConfig(up.URL, "    capture: { body: true, max_bytes: 10KiB }\n")
	cfg, raw := parseText(t, withCapture)
	if _, err := a.apply(cfg, raw); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if a.providers["mock"].pool != pool1 {
		t.Fatal("pool should be reused when only capture settings changed")
	}
	if a.providers["mock"].handler == handler1 {
		t.Fatal("handler should be rebuilt when capture settings changed")
	}
}

func TestApplyBytesInvalidConfigLeavesRunningStateAlone(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	a := newTestApp(t, providerConfig(up.URL, ""))
	before := a.StatusSnapshot()

	_, err := a.ApplyBytes([]byte("auth: {tokens: []}"), "")
	if err == nil {
		t.Fatal("expected a validation error")
	}
	var verr *ValidationError
	if !asValidationError(err, &verr) {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
	}

	after := a.StatusSnapshot()
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("running state changed after a rejected config:\nbefore=%v\nafter=%v", before, after)
	}
}

func TestApplyBytesConflict(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	a := newTestApp(t, providerConfig(up.URL, ""))
	_, err := a.ApplyBytes([]byte(providerConfig(up.URL, "")), "sha256:not-the-current-version")
	if err == nil {
		t.Fatal("expected a conflict error")
	}
	var cerr *ConflictError
	if !asConflictError(err, &cerr) {
		t.Fatalf("expected *ConflictError, got %T: %v", err, err)
	}
}

func TestApplyBytesChecksDiskVersion(t *testing.T) {
	initial := providerConfig("http://127.0.0.1:1", "")
	a := newTestApp(t, initial)
	version := a.version
	external := initial + "\n# changed outside the dashboard\n"
	if err := os.WriteFile(a.opts.ConfigPath, []byte(external), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := a.ApplyBytes([]byte(initial+"\n# stale edit\n"), version)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("external edit overwritten: %v", err)
	}
	if _, err := a.ApplyBytes([]byte(initial+"\n# merged edit\n"), config.Version([]byte(external))); err != nil {
		t.Fatalf("could not save against the version returned by ReadConfig: %v", err)
	}
}

func TestConcurrentConfigSavesHaveOneWinner(t *testing.T) {
	initial := providerConfig("http://127.0.0.1:1", "")
	a := newTestApp(t, initial)
	version := a.version
	start := make(chan struct{})
	results := make(chan error, 12)
	var wg sync.WaitGroup
	for i := range cap(results) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := a.ApplyBytes([]byte(initial+fmt.Sprintf("\n# edit %d\n", i)), version)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		var conflict *ConflictError
		if err == nil {
			winners++
		} else if !errors.As(err, &conflict) {
			t.Errorf("unexpected save error: %v", err)
		}
	}
	_, diskVersion, _, err := a.ReadConfig()
	if err != nil || winners != 1 || diskVersion != a.version {
		t.Fatalf("winners=%d, disk=%s, applied=%s, error=%v", winners, diskVersion, a.version, err)
	}
}

func TestReloadAppliesRotationAndSidecarCapture(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer up.Close()
	initial := providerConfig(up.URL, "    capture: {body: false}\n") + fmt.Sprintf(`
rotation: {max_disables: 1, max_body_buffer: 1KiB}
sidecar:
  mock:
    route: oauth
    url: %s
    capture: {body: true}
`, up.URL)
	a := newTestApp(t, initial)
	waitForHealthy(t, a, "mock")
	r := httptest.NewRequest("POST", "/oauth/test", strings.NewReader(strings.Repeat("x", 1500)))
	r.Header.Set("Authorization", "Bearer dev-token")
	a.Server().ServeHTTP(httptest.NewRecorder(), r)
	records := a.opts.History.List(history.Filter{})
	record, _ := a.opts.History.Get(records[0].ID)
	if record.ReqBody == nil || !record.ReqTrunc {
		t.Error("sidecar did not capture with its own policy and initial buffer limit")
	}
	oldSidecar := a.sidecars["mock"]
	oldPool := a.providers["mock"].pool
	changed := strings.ReplaceAll(initial, "max_disables: 1", "max_disables: 0")
	_, err := a.ApplyBytes([]byte(changed), a.version)
	if err != nil {
		t.Fatal(err)
	}
	if a.providers["mock"].handler.Params.MaxDisables != 0 || a.providers["mock"].pool != oldPool {
		t.Error("new disable budget was not applied while preserving the pool")
	}
	changed = strings.ReplaceAll(changed, "max_body_buffer: 1KiB", "max_body_buffer: 2KiB")
	result, err := a.ApplyBytes([]byte(changed), a.version)
	if err != nil {
		t.Fatal(err)
	}
	if a.sidecars["mock"].sup != oldSidecar.sup {
		t.Error("sidecar buffer change must keep its supervisor")
	}
	r = httptest.NewRequest("POST", "/oauth/test", strings.NewReader(strings.Repeat("x", 1500)))
	r.Header.Set("Authorization", "Bearer dev-token")
	a.Server().ServeHTTP(httptest.NewRecorder(), r)
	records = a.opts.History.List(history.Filter{})
	record, _ = a.opts.History.Get(records[0].ID)
	if len(record.ReqBody) != 1500 || record.ReqTrunc {
		t.Error("sidecar did not apply the updated buffer size")
	}
	if !strings.Contains(fmt.Sprint(result.Changed.Updated), "sidecar:mock") {
		t.Errorf("sidecar update not reported: %+v", result.Changed)
	}
}

func TestDashboardDisabledOnReload(t *testing.T) {
	initial := providerConfig("http://127.0.0.1:1", "")
	a := newTestApp(t, initial)
	a.SetDashboard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("disabled dashboard handler was reached")
	}))
	// A syntactically valid hash is sufficient; this test never verifies a password.
	changed := strings.Replace(initial, "  tokens:", "  password_hash: '$2a$04$abcdefghijklmnopqrstuuABCDEFGHIJKLMNOPQRSTUVWXYZabcde'\n  tokens:", 1)
	changed += "\ndashboard: {enabled: false}\n"
	if _, err := a.ApplyBytes([]byte(changed), a.version); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/dashboard/", "/dashboard/api/login", "/dashboard/api/config"} {
		rec := httptest.NewRecorder()
		a.Server().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s returned %d, want disabled dashboard", path, rec.Code)
		}
	}
}

func TestWatchIntervalChangesOnReload(t *testing.T) {
	initial := providerConfig("http://127.0.0.1:1", "") + "\ndashboard: {watch: 1h}\n"
	a := newTestApp(t, initial)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { a.Watch(ctx, time.Hour); close(done) }()
	defer func() { cancel(); <-done }()
	changed := strings.Replace(initial, "watch: 1h", "watch: 1s", 1)
	if _, err := a.ApplyBytes([]byte(changed), a.version); err != nil {
		t.Fatal(err)
	}
	changed = strings.Replace(changed, "key-one-aaaaaaaaaaaa", "replacement-key", 1)
	if err := os.WriteFile(a.opts.ConfigPath, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		applied := a.cfg.Providers["mock"].Keys[0] == "replacement-key"
		a.mu.Unlock()
		if applied {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("watcher kept the old one-hour interval")
}

func TestSidecarReloadReusesUnchangedSupervisor(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	cfgText := fmt.Sprintf(`
auth:
  tokens: ["dev-token"]
sidecar:
  side:
    route: side
    url: %s
`, up.URL)
	a := newTestApp(t, cfgText)
	waitForHealthy(t, a, "side")
	sup1 := a.sidecars["side"].sup

	cfg, raw := parseText(t, cfgText)
	if _, err := a.apply(cfg, raw); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if a.sidecars["side"].sup != sup1 {
		t.Fatal("unchanged sidecar should keep the same supervisor")
	}
}

func TestSidecarReloadReplacesChangedSupervisor(t *testing.T) {
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer up1.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer up2.Close()

	cfgText := fmt.Sprintf(`
auth:
  tokens: ["dev-token"]
sidecar:
  side:
    route: side
    url: %s
`, up1.URL)
	a := newTestApp(t, cfgText)
	waitForHealthy(t, a, "side")
	sup1 := a.sidecars["side"].sup

	changed := fmt.Sprintf(`
auth:
  tokens: ["dev-token"]
sidecar:
  side:
    route: side
    url: %s
`, up2.URL)
	cfg, raw := parseText(t, changed)
	if _, err := a.apply(cfg, raw); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if a.sidecars["side"].sup == sup1 {
		t.Fatal("changed sidecar url should produce a fresh supervisor")
	}
	waitForHealthy(t, a, "side")
}

func TestSidecarRemovedIsRetiredAndNoProcessLeaksOnShutdown(t *testing.T) {
	cfgText := `
auth:
  tokens: ["dev-token"]
sidecar:
  side:
    route: side
    command: ["sleep", "300"]
    port: 1
`
	path := writeConfig(t, cfgText)
	a, err := New(Options{
		ConfigPath: path,
		Transport:  http.DefaultTransport,
		Log:        testLog(),
		History:    history.New(100, 20, 1<<20),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Remove the sidecar entirely.
	empty := `
auth:
  tokens: ["dev-token"]
providers:
  noop:
    base_url: http://127.0.0.1:1
    key_in: { header: x-api-key }
    keys: ["k"]
`
	cfg, raw := parseText(t, empty)
	if _, err := a.apply(cfg, raw); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(a.sidecars) != 0 {
		t.Fatal("removed sidecar should be gone from the live set")
	}

	// Shutdown must return well within its timeout: the retired supervisor
	// (still in its grace-delay goroutine) and the process it spawned must
	// both be torn down, not leaked past process exit.
	done := make(chan struct{})
	go func() {
		a.Shutdown(10 * time.Second)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(9 * time.Second):
		t.Fatal("Shutdown did not return in time — likely a leaked sidecar process/goroutine")
	}
}

func TestListenFrozenAcrossReload(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer up.Close()

	a := newTestApp(t, "listen: \"127.0.0.1:19999\"\n"+providerConfig(up.URL, ""))
	addr := a.Addr()
	if addr != "127.0.0.1:19999" {
		t.Fatalf("addr = %q", addr)
	}

	changed := "listen: \"127.0.0.1:20000\"\n" + providerConfig(up.URL, "")
	cfg, raw := parseText(t, changed)
	res, err := a.apply(cfg, raw)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if a.Addr() != addr {
		t.Fatalf("Addr() changed across reload: %q -> %q", addr, a.Addr())
	}
	if len(res.RestartRequired) == 0 || res.RestartRequired[0] != "listen" {
		t.Fatalf("expected restart_required=[listen], got %v", res.RestartRequired)
	}
}

// --- helpers -----------------------------------------------------------

func waitForHealthy(t *testing.T, a *App, name string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if a.sidecars[name].sup.Healthy() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("sidecar %q never became healthy", name)
}

// readAndParse re-reads the App's own config file and parses it, giving a
// (*config.Config, []byte) pair to feed straight back into a.apply — used
// to test re-applying an identical config.
func readAndParse(t *testing.T, a *App) (*config.Config, []byte) {
	t.Helper()
	data, err := os.ReadFile(a.opts.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	return parseText(t, string(data))
}

func parseText(t *testing.T, text string) (*config.Config, []byte) {
	t.Helper()
	raw := []byte(text)
	cfg, err := config.Parse(raw, "test")
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	return cfg, raw
}

func asValidationError(err error, target **ValidationError) bool {
	return errors.As(err, target)
}

func asConflictError(err error, target **ConflictError) bool {
	return errors.As(err, target)
}
