package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "heka.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const valid = `
auth:
  tokens: [ "${TEST_HEKA_TOKEN}" ]
rotation:
  cooldown_base: 30s
  max_body_buffer: 1MiB
providers:
  anthropic:
    base_url: https://api.anthropic.com
    key_in: { header: x-api-key }
    keys: [ "k-one", "k-two" ]
    rotation:
      max_retries: 5
  google:
    base_url: https://generativelanguage.googleapis.com
    key_in: { query: key }
    keys: [ "g-one" ]
sidecar:
  cliproxy:
    route: oauth
    command: [ "cli-proxy-api" ]
    port: 8317
    health_interval: 30s
`

func TestLoadValid(t *testing.T) {
	t.Setenv("TEST_HEKA_TOKEN", "secret-token")
	cfg, err := Load(write(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:8787" {
		t.Fatalf("default listen = %q", cfg.Listen)
	}
	if cfg.Auth.Tokens[0] != "secret-token" {
		t.Fatalf("env interpolation failed: %q", cfg.Auth.Tokens[0])
	}

	rot := cfg.RotationFor("anthropic")
	if rot.CooldownBase != 30*time.Second {
		t.Fatalf("global override lost: %v", rot.CooldownBase)
	}
	if rot.CooldownMax != time.Hour {
		t.Fatalf("built-in default lost: %v", rot.CooldownMax)
	}
	if rot.MaxRetries != 5 {
		t.Fatalf("provider override lost: %d", rot.MaxRetries)
	}
	if rot.MaxBodyBuffer != 1<<20 {
		t.Fatalf("size parsing: %d", rot.MaxBodyBuffer)
	}
	if got := cfg.RotationFor("google").MaxRetries; got != 3 {
		t.Fatalf("google max_retries = %d, want default 3", got)
	}
	if rot.MaxDisables != 1 {
		t.Fatalf("max_disables = %d, want default 1", rot.MaxDisables)
	}
	if cfg.Sidecar["cliproxy"].Route != "oauth" {
		t.Fatalf("sidecar route = %q", cfg.Sidecar["cliproxy"].Route)
	}
	if got := time.Duration(*cfg.Sidecar["cliproxy"].HealthInterval); got != 30*time.Second {
		t.Fatalf("sidecar health interval = %v", got)
	}
}

// Secrets are substituted at the YAML node level, so any characters — quotes,
// newlines, YAML syntax — stay inside the string value.
func TestEnvValueWithYAMLMetachars(t *testing.T) {
	nasty := `ab"cd', [x]: &y ` + "\nnewline"
	t.Setenv("TEST_NASTY_SECRET", nasty)
	cfg, err := Load(write(t, `
auth:
  tokens: [ "${TEST_NASTY_SECRET}" ]
providers:
  p:
    base_url: https://example.com
    key_in: { header: x-api-key }
    keys: [ "k-something-long" ]
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Auth.Tokens) != 1 || cfg.Auth.Tokens[0] != nasty {
		t.Fatalf("secret mangled: %q", cfg.Auth.Tokens)
	}
}

func TestUnquotedEnvValueRemainsString(t *testing.T) {
	for _, secret := range []string{"null", "true", "123", "[yaml]"} {
		t.Run(secret, func(t *testing.T) {
			t.Setenv("TEST_UNQUOTED_SECRET", secret)
			cfg, err := Load(write(t, `
auth:
  tokens:
    - ${TEST_UNQUOTED_SECRET}
providers:
  p:
    base_url: https://example.com
    key_in: { header: x-api-key }
    keys: [ "k-something-long" ]
`))
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Auth.Tokens[0]; got != secret {
				t.Fatalf("secret = %q, want %q", got, secret)
			}
		})
	}
}

// ${VAR} inside comments is inert; $${VAR} passes a literal ${VAR} through.
func TestEnvExpansionCommentsAndEscape(t *testing.T) {
	os.Unsetenv("TEST_COMMENTED_OUT_KEY")
	cfg, err := Load(write(t, `
auth:
  tokens: [ "t-something-long" ]
providers:
  p:
    base_url: https://example.com
    key_in: { header: x-api-key }
    # keys: [ "${TEST_COMMENTED_OUT_KEY}" ]
    keys: [ "$${NOT_AN_ENV_REF}" ]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Providers["p"].Keys[0]; got != "${NOT_AN_ENV_REF}" {
		t.Fatalf("escape: %q", got)
	}
}

func TestDashboardLoginValidation(t *testing.T) {
	const provider = `
providers:
  p: { base_url: "https://example.com", key_in: { header: h }, keys: [ "k" ] }
`
	h, err := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}

	// No login at all is valid: it just means no dashboard.
	if _, err := Load(write(t, `auth: { tokens: [ "t" ] }`+provider)); err != nil {
		t.Fatalf("no login configured: %v", err)
	}
	cfg, err := Load(write(t, `auth: { tokens: [ "t" ], user: admin, password_hash: "`+string(h)+`" }`+provider))
	if err != nil {
		t.Fatalf("full login: %v", err)
	}
	if !cfg.Auth.DashboardLogin() {
		t.Fatal("DashboardLogin() = false with user and hash set")
	}

	if _, err := Load(write(t, `auth: { tokens: [ "t" ], user: admin }`+provider)); err == nil {
		t.Fatal("user without password_hash accepted")
	}
	if _, err := Load(write(t, `auth: { tokens: [ "t" ], user: admin, password_hash: "plaintext" }`+provider)); err == nil {
		t.Fatal("non-bcrypt password_hash accepted")
	}
}

func TestRotationTriggerOverrides(t *testing.T) {
	cfg, err := Load(write(t, `
auth: { tokens: [ "t" ] }
rotation:
  disable_on: [ 401 ]
providers:
  p:
    base_url: https://example.com
    key_in: { header: h }
    keys: [ "k" ]
    rotation:
      cooldown_on: [ 429, 529 ]
      disable_on: []
`))
	if err != nil {
		t.Fatal(err)
	}
	rot := cfg.RotationFor("p")
	if len(rot.CooldownOn) != 2 || rot.CooldownOn[1] != 529 {
		t.Fatalf("cooldown_on = %v", rot.CooldownOn)
	}
	// Empty list is a deliberate "never disable" override.
	if len(rot.DisableOn) != 0 {
		t.Fatalf("disable_on = %v, want empty override", rot.DisableOn)
	}
	if got := cfg.DefaultRotation().DisableOn; len(got) != 1 || got[0] != 401 {
		t.Fatalf("global disable_on = %v", got)
	}
}

func TestAffinityOverrides(t *testing.T) {
	bare, err := Load(write(t, `
auth: { tokens: [ "t" ] }
providers:
  p: { base_url: "https://example.com", key_in: { header: h }, keys: [ "k" ] }
`))
	if err != nil {
		t.Fatal(err)
	}
	// On by default, so pools benefit from prompt-cache stickiness without
	// anyone having to know the option exists.
	if got := bare.RotationFor("p").Affinity; !got.Enabled || got.Header != "X-Heka-Affinity" {
		t.Fatalf("default affinity = %+v", got)
	}

	cfg, err := Load(write(t, `
auth: { tokens: [ "t" ] }
rotation:
  affinity:
    header: X-Session
providers:
  chat:
    base_url: https://example.com
    key_in: { header: h }
    keys: [ "k" ]
  search:
    base_url: https://example.com
    key_in: { header: h }
    keys: [ "k" ]
    rotation:
      affinity: { enabled: false }
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.RotationFor("chat").Affinity; !got.Enabled || got.Header != "X-Session" {
		t.Fatalf("chat affinity = %+v", got)
	}
	if got := cfg.RotationFor("search").Affinity; got.Enabled {
		t.Fatalf("search affinity = %+v, want disabled", got)
	}

	loadErr(t, `
auth: { tokens: [ "t" ] }
rotation:
  affinity: { header: "not a header" }
providers:
  p: { base_url: "https://example.com", key_in: { header: h }, keys: [ "k" ] }
`, "not a valid header name")
}

func TestSidecarKeyValidation(t *testing.T) {
	loadErr(t, `
auth: { tokens: [ "t" ] }
sidecar:
  s:
    url: http://127.0.0.1:8317
    key: secret
`, "key requires key_in")

	cfg, err := Load(write(t, `
auth: { tokens: [ "t" ] }
sidecar:
  s:
    url: http://127.0.0.1:8317
    key_in: { header: Authorization, prefix: "Bearer " }
    key: secret
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sidecar["s"].KeyIn.Header != "Authorization" || cfg.Sidecar["s"].Key != "secret" {
		t.Fatalf("sidecar key config lost: %+v", cfg.Sidecar["s"])
	}
}

func TestMissingEnvVar(t *testing.T) {
	os.Unsetenv("TEST_HEKA_TOKEN_MISSING")
	_, err := Load(write(t, `
auth:
  tokens: [ "${TEST_HEKA_TOKEN_MISSING}" ]
providers:
  p:
    base_url: https://example.com
    key_in: { header: x-api-key }
    keys: [ "k" ]
`))
	if err == nil || !strings.Contains(err.Error(), "TEST_HEKA_TOKEN_MISSING") {
		t.Fatalf("want undefined env var error, got %v", err)
	}
}

func loadErr(t *testing.T, yaml, wantSubstr string) {
	t.Helper()
	_, err := Load(write(t, yaml))
	if err == nil || !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("want error containing %q, got %v", wantSubstr, err)
	}
}

func TestValidation(t *testing.T) {
	loadErr(t, `
providers:
  p:
    base_url: https://example.com
    key_in: { header: h }
    keys: [ "k" ]
`, "auth.tokens")

	loadErr(t, `
auth: { tokens: [ "t" ] }
`, "no providers")

	loadErr(t, `
auth: { tokens: [ "t" ] }
providers:
  p:
    base_url: example.com
    key_in: { header: h }
    keys: [ "k" ]
`, "absolute http(s) URL")

	loadErr(t, `
auth: { tokens: [ "t" ] }
providers:
  p:
    base_url: https://example.com
    key_in: { header: h, query: q }
    keys: [ "k" ]
`, "mutually exclusive")

	loadErr(t, `
auth: { tokens: [ "t" ] }
providers:
  p:
    base_url: https://example.com
    key_in: { header: h }
    keys: []
`, "at least one key")

	loadErr(t, `
auth: { tokens: [ "t" ] }
providers:
  status:
    base_url: https://example.com
    key_in: { header: h }
    keys: [ "k" ]
`, "reserved")

	// A sidecar route colliding with a provider name.
	loadErr(t, `
auth: { tokens: [ "t" ] }
providers:
  p:
    base_url: https://example.com
    key_in: { header: h }
    keys: [ "k" ]
sidecar:
  s:
    route: p
    url: http://127.0.0.1:9000
`, "already used")

	loadErr(t, `
auth: { tokens: [ "t" ] }
sidecar:
  s:
    command: [ "bin" ]
    url: http://127.0.0.1:9000
`, "mutually exclusive")

	loadErr(t, `
auth: { tokens: [ "t" ] }
sidecar:
  s:
    command: [ "bin" ]
`, "port")

	// Typos in field names must not pass silently.
	loadErr(t, `
auth: { tokens: [ "t" ] }
providers:
  p:
    baseurl: https://example.com
    key_in: { header: h }
    keys: [ "k" ]
`, "baseurl")
}

func TestSidecarRouteDefaultsToName(t *testing.T) {
	cfg, err := Load(write(t, `
auth: { tokens: [ "t" ] }
sidecar:
  oauth:
    url: http://127.0.0.1:8317
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sidecar["oauth"].Route != "oauth" {
		t.Fatalf("route = %q, want name fallback", cfg.Sidecar["oauth"].Route)
	}
}

func TestSizeParsing(t *testing.T) {
	for raw, want := range map[string]int64{
		`"1024"`: 1024, `512KiB`: 512 << 10, `10MiB`: 10 << 20, `1GiB`: 1 << 30,
	} {
		cfg, err := Load(write(t, `
auth: { tokens: [ "t" ] }
rotation: { max_body_buffer: `+raw+` }
providers:
  p:
    base_url: https://example.com
    key_in: { header: h }
    keys: [ "k" ]
`))
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if got := cfg.DefaultRotation().MaxBodyBuffer; got != want {
			t.Fatalf("%s = %d, want %d", raw, got, want)
		}
	}
	loadErr(t, `
auth: { tokens: [ "t" ] }
rotation: { max_body_buffer: 10MB }
providers:
  p:
    base_url: https://example.com
    key_in: { header: h }
    keys: [ "k" ]
`, "invalid size")
}
