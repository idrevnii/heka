package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseMatchesLoad(t *testing.T) {
	t.Setenv("TEST_HEKA_TOKEN", "secret-token")
	path := write(t, valid)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	viaLoad, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	viaParse, err := Parse(raw, path)
	if err != nil {
		t.Fatal(err)
	}
	if viaLoad.Auth.Tokens[0] != viaParse.Auth.Tokens[0] {
		t.Fatalf("Load and Parse disagree: %q vs %q", viaLoad.Auth.Tokens[0], viaParse.Auth.Tokens[0])
	}
	if len(viaLoad.Providers) != len(viaParse.Providers) {
		t.Fatal("Load and Parse disagree on provider count")
	}
}

func TestParseErrorsUseGivenSource(t *testing.T) {
	_, err := Parse([]byte("auth: {tokens: []}"), "request body")
	if err == nil || !strings.Contains(err.Error(), "request body") {
		t.Fatalf("error should reference the given source: %v", err)
	}
}

func TestVersionStable(t *testing.T) {
	a := Version([]byte("hello"))
	b := Version([]byte("hello"))
	c := Version([]byte("world"))
	if a != b {
		t.Fatal("Version should be deterministic")
	}
	if a == c {
		t.Fatal("Version should differ for different content")
	}
	if !strings.HasPrefix(a, "sha256:") {
		t.Fatalf("Version format: %q", a)
	}
}

func TestSaveAtomicWithBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "heka.yaml")

	if err := Save(path, []byte("v1"), true, 10); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "v1" {
		t.Fatalf("content = %q", got)
	}
	// First save: nothing existed yet, so no backup should be made.
	matches, _ := filepath.Glob(path + ".bak-*")
	if len(matches) != 0 {
		t.Fatalf("unexpected backups after first save: %v", matches)
	}

	if err := Save(path, []byte("v2"), true, 10); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != "v2" {
		t.Fatalf("content = %q", got)
	}
	matches, _ = filepath.Glob(path + ".bak-*")
	if len(matches) != 1 {
		t.Fatalf("expected 1 backup of v1, got %v", matches)
	}
	bak, _ := os.ReadFile(matches[0])
	if string(bak) != "v1" {
		t.Fatalf("backup content = %q, want v1", bak)
	}

	// No leftover temp files.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".heka-config-") {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}

func TestSavePruneBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "heka.yaml")
	for i := 0; i < 5; i++ {
		if err := Save(path, []byte{byte('a' + i)}, true, 2); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond) // ensure distinct timestamps
	}
	matches, _ := filepath.Glob(path + ".bak-*")
	if len(matches) > 2 {
		t.Fatalf("backups not pruned: %v", matches)
	}
}

func TestDashboardRouteReserved(t *testing.T) {
	t.Setenv("TEST_HEKA_TOKEN", "secret-token")
	bad := `
auth:
  tokens: [ "${TEST_HEKA_TOKEN}" ]
providers:
  dashboard:
    base_url: https://example.com
    key_in: { header: x-api-key }
    keys: [ "k" ]
`
	_, err := Load(write(t, bad))
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected reserved-route error, got %v", err)
	}
}

func TestHistoryAndDashboardDefaults(t *testing.T) {
	t.Setenv("TEST_HEKA_TOKEN", "secret-token")
	cfg, err := Load(write(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	hp := cfg.DefaultHistory()
	if hp.Size != 500 || hp.Errors != 200 || hp.MaxBytes != 32<<20 {
		t.Fatalf("history defaults = %+v", hp)
	}
	dp := cfg.DefaultDashboard()
	if !dp.Enabled || !dp.ConfigEdit || dp.Watch != 10*time.Second {
		t.Fatalf("dashboard defaults = %+v", dp)
	}
}

func TestCaptureDefaultsOff(t *testing.T) {
	t.Setenv("TEST_HEKA_TOKEN", "secret-token")
	cfg, err := Load(write(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	capp := cfg.CaptureFor("anthropic")
	if capp.Body {
		t.Fatal("capture.body must default to false")
	}
}

func TestCaptureOptIn(t *testing.T) {
	t.Setenv("TEST_HEKA_TOKEN", "secret-token")
	doc := `
auth:
  tokens: [ "${TEST_HEKA_TOKEN}" ]
providers:
  anthropic:
    base_url: https://api.anthropic.com
    key_in: { header: x-api-key }
    keys: [ "k-one" ]
    capture: { body: true, max_bytes: 128KiB }
`
	cfg, err := Load(write(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	capp := cfg.CaptureFor("anthropic")
	if !capp.Body {
		t.Fatal("capture.body should be true")
	}
	if capp.MaxBytes != 128<<10 {
		t.Fatalf("capture.max_bytes = %d", capp.MaxBytes)
	}
}

func TestCaptureMaxBytesBounded(t *testing.T) {
	t.Setenv("TEST_HEKA_TOKEN", "secret-token")
	doc := `
auth:
  tokens: [ "${TEST_HEKA_TOKEN}" ]
providers:
  anthropic:
    base_url: https://api.anthropic.com
    key_in: { header: x-api-key }
    keys: [ "k-one" ]
    capture: { body: true, max_bytes: 100MiB }
`
	_, err := Load(write(t, doc))
	if err == nil || !strings.Contains(err.Error(), "max_bytes") {
		t.Fatalf("expected max_bytes bound error, got %v", err)
	}
}

func TestHistorySizeBounds(t *testing.T) {
	t.Setenv("TEST_HEKA_TOKEN", "secret-token")
	doc := valid + "\nhistory:\n  size: 0\n"
	_, err := Load(write(t, doc))
	if err == nil || !strings.Contains(err.Error(), "history") {
		t.Fatalf("expected history bound error, got %v", err)
	}
}
