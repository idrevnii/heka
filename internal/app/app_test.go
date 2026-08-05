package app

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/idrevnii/heka/internal/config"
	"github.com/idrevnii/heka/internal/history"
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
