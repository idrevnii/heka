// Package config loads and validates the heka YAML configuration.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen    string               `yaml:"listen"`
	Auth      Auth                 `yaml:"auth"`
	Rotation  *Rotation            `yaml:"rotation"`
	Providers map[string]*Provider `yaml:"providers"`
	Sidecar   map[string]*Sidecar  `yaml:"sidecar"`
	History   *History             `yaml:"history"`
	Dashboard *Dashboard           `yaml:"dashboard"`
}

// History configures the in-memory request/error ring buffers used by the
// dashboard. All fields are optional; unset ones fall back to the defaults
// documented on HistoryParams.
type History struct {
	Size     *int  `yaml:"size"`
	Errors   *int  `yaml:"errors"`
	MaxBytes *Size `yaml:"max_bytes"`
}

// HistoryParams is a fully resolved history policy.
type HistoryParams struct {
	Size     int
	Errors   int
	MaxBytes int64
}

// Dashboard configures the embedded web dashboard.
type Dashboard struct {
	Enabled    *bool     `yaml:"enabled"`
	ConfigEdit *bool     `yaml:"config_edit"`
	Watch      *Duration `yaml:"watch"`
}

// DashboardParams is a fully resolved dashboard policy.
type DashboardParams struct {
	Enabled    bool
	ConfigEdit bool
	Watch      time.Duration
}

// Capture configures opt-in request/response body capture for the
// dashboard's request history. Disabled by default: bodies may contain
// secrets or user data, so this is a deliberate per-route decision.
type Capture struct {
	Body      *bool `yaml:"body"`
	MaxBytes  *Size `yaml:"max_bytes"`
	Streaming *bool `yaml:"streaming"`
}

// CaptureParams is a fully resolved capture policy.
type CaptureParams struct {
	Body      bool
	MaxBytes  int64
	Streaming bool
}

// DefaultHistory returns the built-in history defaults with the top-level
// section applied.
func (c *Config) DefaultHistory() HistoryParams {
	p := HistoryParams{Size: 500, Errors: 200, MaxBytes: 32 << 20}
	if h := c.History; h != nil {
		if h.Size != nil {
			p.Size = *h.Size
		}
		if h.Errors != nil {
			p.Errors = *h.Errors
		}
		if h.MaxBytes != nil {
			p.MaxBytes = int64(*h.MaxBytes)
		}
	}
	return p
}

// DefaultDashboard returns the built-in dashboard defaults with the
// top-level section applied.
func (c *Config) DefaultDashboard() DashboardParams {
	p := DashboardParams{Enabled: true, ConfigEdit: true, Watch: 10 * time.Second}
	if d := c.Dashboard; d != nil {
		if d.Enabled != nil {
			p.Enabled = *d.Enabled
		}
		if d.ConfigEdit != nil {
			p.ConfigEdit = *d.ConfigEdit
		}
		if d.Watch != nil {
			p.Watch = time.Duration(*d.Watch)
		}
	}
	return p
}

// defaultCapture is the built-in, always-off capture policy.
func defaultCapture() CaptureParams {
	return CaptureParams{Body: false, MaxBytes: 64 << 10, Streaming: false}
}

func (c *Capture) apply(p *CaptureParams) {
	if c == nil {
		return
	}
	if c.Body != nil {
		p.Body = *c.Body
	}
	if c.MaxBytes != nil {
		p.MaxBytes = int64(*c.MaxBytes)
	}
	if c.Streaming != nil {
		p.Streaming = *c.Streaming
	}
}

// Params resolves capture for this specific provider or sidecar. Names can
// overlap between the two sections, so callers pass the section itself.
func (c *Capture) Params() CaptureParams {
	p := defaultCapture()
	c.apply(&p)
	return p
}

// Auth holds both credentials heka knows about: the gateway tokens that API
// clients (SDKs, curl) send as their key, and the single dashboard login the
// browser signs in with. They are deliberately separate — a token is what a
// machine can send on every request, a password is what a human types once.
type Auth struct {
	Tokens []string `yaml:"tokens"`
	// User and PasswordHash gate the dashboard. PasswordHash is a bcrypt
	// hash ("heka -hash" prints one); the plaintext password is never
	// stored. Leaving both empty turns the dashboard off.
	User         string `yaml:"user"`
	PasswordHash string `yaml:"password_hash"`
}

// DefaultDashboardUser is the account name assumed when a password_hash is
// configured without a user.
const DefaultDashboardUser = "admin"

// DashboardLogin reports whether a dashboard login is configured at all.
func (a Auth) DashboardLogin() bool {
	return a.User != "" && a.PasswordHash != ""
}

// validateLogin rejects a half-configured login (one field without the
// other) and a password_hash that isn't bcrypt — both are silent lockouts
// otherwise, discovered only when someone tries to sign in.
func (a Auth) validateLogin() error {
	if a.User == "" && a.PasswordHash == "" {
		return nil
	}
	if a.User == "" || a.PasswordHash == "" {
		return errors.New("auth.user and auth.password_hash must be set together")
	}
	if _, err := bcrypt.Cost([]byte(a.PasswordHash)); err != nil {
		return fmt.Errorf("auth.password_hash is not a bcrypt hash: %w", err)
	}
	return nil
}

// Rotation holds optional overrides; nil fields fall back to the level above.
type Rotation struct {
	CooldownBase  *Duration `yaml:"cooldown_base"`
	CooldownMax   *Duration `yaml:"cooldown_max"`
	MaxRetries    *int      `yaml:"max_retries"`
	MaxDisables   *int      `yaml:"max_disables"`
	MaxBodyBuffer *Size     `yaml:"max_body_buffer"`
	CooldownOn    []int     `yaml:"cooldown_on"`
	DisableOn     []int     `yaml:"disable_on"`
	Affinity      *Affinity `yaml:"affinity"`
}

// RotationParams is a fully resolved rotation policy.
type RotationParams struct {
	CooldownBase  time.Duration
	CooldownMax   time.Duration
	MaxRetries    int
	MaxDisables   int
	MaxBodyBuffer int64
	CooldownOn    []int
	DisableOn     []int
	Affinity      AffinityParams
}

// Affinity configures session stickiness: binding requests that share a
// cacheable prompt prefix to one key, so upstream prompt caches (which live
// per key) actually get hit instead of being rewritten on every turn.
type Affinity struct {
	Enabled *bool `yaml:"enabled"`
	// Header names an explicit affinity key supplied by the client; empty
	// means derive the binding from the request body only.
	Header *string `yaml:"header"`
}

// AffinityParams is a fully resolved affinity policy.
type AffinityParams struct {
	Enabled bool
	Header  string
}

type Provider struct {
	BaseURL  string    `yaml:"base_url"`
	KeyIn    KeyIn     `yaml:"key_in"`
	Keys     []string  `yaml:"keys"`
	Rotation *Rotation `yaml:"rotation"`
	Capture  *Capture  `yaml:"capture"`
	// CheckModel names the model a manual key check asks one token from;
	// empty means the provider offers no check.
	CheckModel string `yaml:"check_model"`
}

// KeyIn describes where the provider expects its API key: exactly one of
// Header or Query must be set; Prefix is prepended to the header value.
type KeyIn struct {
	Header string `yaml:"header"`
	Prefix string `yaml:"prefix"`
	Query  string `yaml:"query"`
}

type Sidecar struct {
	Route          string    `yaml:"route"`
	Command        []string  `yaml:"command"`
	Port           int       `yaml:"port"`
	URL            string    `yaml:"url"`
	HealthInterval *Duration `yaml:"health_interval"`
	// Optional static key injected into sidecar requests (for CLIProxyAPI
	// instances that require their own api-key). No rotation.
	KeyIn   *KeyIn   `yaml:"key_in"`
	Key     string   `yaml:"key"`
	Capture *Capture `yaml:"capture"`
}

type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"60s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	if v <= 0 {
		return fmt.Errorf("duration %q must be positive", s)
	}
	*d = Duration(v)
	return nil
}

type Size int64

var sizeRe = regexp.MustCompile(`^([0-9]+)\s*(B|KiB|MiB|GiB)?$`)

func (s *Size) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("size must be a scalar like \"10MiB\": %w", err)
	}
	m := sizeRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return fmt.Errorf("invalid size %q (expected e.g. \"10MiB\", \"512KiB\" or bytes)", raw)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid size %q: %w", raw, err)
	}
	var shift uint
	switch m[2] {
	case "KiB":
		shift = 10
	case "MiB":
		shift = 20
	case "GiB":
		shift = 30
	}
	if n > math.MaxInt64>>shift {
		return fmt.Errorf("size %q overflows int64", raw)
	}
	n <<= shift
	if n <= 0 {
		return fmt.Errorf("size %q must be positive", raw)
	}
	*s = Size(n)
	return nil
}

// Load reads and parses the config file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw, path)
}

// Parse expands ${VAR} references, strictly decodes, and validates raw YAML
// config bytes. source is used only in error messages — a file path at
// startup, or e.g. "request body" when parsing config submitted through the
// dashboard's editor.
func Parse(raw []byte, source string) (*Config, error) {
	var doc yaml.Node
	yamlDec := yaml.NewDecoder(bytes.NewReader(raw))
	if err := yamlDec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	if doc.Kind == 0 {
		return nil, fmt.Errorf("%s: config is empty", source)
	}
	var extra yaml.Node
	if err := yamlDec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: config must contain exactly one YAML document", source)
	}
	var missing []string
	expandEnvNode(&doc, &missing)
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s: undefined environment variables: %s",
			source, strings.Join(missing, ", "))
	}
	// Re-emit and strictly re-parse: the YAML emitter quotes substituted
	// values, so secrets stay opaque strings whatever characters they hold.
	data, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	return &cfg, nil
}

// Version returns a short content-hash of raw config bytes, used as an
// optimistic-concurrency token by the dashboard's config editor.
func Version(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

// Save writes data to path atomically: it's written to a temp file in the
// same directory (so the following rename stays on one filesystem, which
// matters when path is a bind-mounted volume) and then renamed over path.
// When backup is true, path's previous content — if any — is preserved
// first as path+".bak-"+<RFC3339 timestamp>, and older backups beyond keep
// are pruned.
func Save(path string, data []byte, backup bool, keep int) error {
	dir := filepath.Dir(path)
	if backup {
		if err := saveBackup(path, keep); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(dir, ".heka-config-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func saveBackup(path string, keep int) error {
	cur, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // nothing to back up yet
	}
	if err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	bakPath := path + ".bak-" + stamp
	if err := os.WriteFile(bakPath, cur, 0o600); err != nil {
		return err
	}
	return pruneBackups(path, keep)
}

func pruneBackups(path string, keep int) error {
	if keep <= 0 {
		return nil
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return err
	}
	var matches []string
	for _, entry := range entries { // ReadDir sorts filenames chronologically by their suffix.
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), filepath.Base(path)+".bak-") {
			matches = append(matches, filepath.Join(filepath.Dir(path), entry.Name()))
		}
	}
	if len(matches) <= keep {
		return nil
	}
	for _, m := range matches[:len(matches)-keep] {
		os.Remove(m)
	}
	return nil
}

// envRe matches ${VAR} plus the $${VAR} escape for a literal ${VAR}.
var envRe = regexp.MustCompile(`\$?\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// expandEnvNode expands ${VAR} in scalar values only — comments and mapping
// keys are never touched, so commented-out secrets can't fail the load.
func expandEnvNode(n *yaml.Node, missing *[]string) {
	if n.Kind == yaml.ScalarNode {
		expanded := envRe.ReplaceAllStringFunc(n.Value, func(m string) string {
			if strings.HasPrefix(m, "$$") {
				return m[1:]
			}
			name := m[2 : len(m)-1]
			v, ok := os.LookupEnv(name)
			if !ok {
				*missing = append(*missing, name)
				return m
			}
			return v
		})
		if expanded != n.Value {
			n.Value = expanded
			// Environment values are opaque strings. In particular, a plain
			// value such as `null`, `true`, or `123` must not be retyped when
			// the expanded document is parsed again.
			n.Tag = "!!str"
		}
		return
	}
	children := n.Content
	if n.Kind == yaml.MappingNode {
		// Values live at odd indexes; keys are left alone.
		for i := 1; i < len(children); i += 2 {
			expandEnvNode(children[i], missing)
		}
		return
	}
	for _, c := range children {
		expandEnvNode(c, missing)
	}
}

// DefaultRotation returns the built-in rotation defaults with the global
// section applied.
func (c *Config) DefaultRotation() RotationParams {
	p := RotationParams{
		CooldownBase:  60 * time.Second,
		CooldownMax:   time.Hour,
		MaxRetries:    3,
		MaxDisables:   1,
		MaxBodyBuffer: 10 << 20,
		CooldownOn:    []int{429},
		DisableOn:     []int{401, 402, 403},
		Affinity:      AffinityParams{Enabled: true, Header: "X-Heka-Affinity"},
	}
	c.Rotation.apply(&p)
	return p
}

// RotationFor resolves the effective rotation policy for a provider:
// built-in defaults ← global rotation ← provider rotation.
func (c *Config) RotationFor(name string) RotationParams {
	p := c.DefaultRotation()
	if prov, ok := c.Providers[name]; ok {
		prov.Rotation.apply(&p)
	}
	return p
}

func (r *Rotation) apply(p *RotationParams) {
	if r == nil {
		return
	}
	if r.CooldownBase != nil {
		p.CooldownBase = time.Duration(*r.CooldownBase)
	}
	if r.CooldownMax != nil {
		p.CooldownMax = time.Duration(*r.CooldownMax)
	}
	if r.MaxRetries != nil {
		p.MaxRetries = *r.MaxRetries
	}
	if r.MaxDisables != nil {
		p.MaxDisables = *r.MaxDisables
	}
	if r.MaxBodyBuffer != nil {
		p.MaxBodyBuffer = int64(*r.MaxBodyBuffer)
	}
	// A present-but-empty list is a deliberate "never" override.
	if r.CooldownOn != nil {
		p.CooldownOn = r.CooldownOn
	}
	if r.DisableOn != nil {
		p.DisableOn = r.DisableOn
	}
	if a := r.Affinity; a != nil {
		if a.Enabled != nil {
			p.Affinity.Enabled = *a.Enabled
		}
		if a.Header != nil {
			p.Affinity.Header = *a.Header
		}
	}
}

func (r *Rotation) validate(where string) error {
	if r == nil {
		return nil
	}
	if r.MaxRetries != nil && *r.MaxRetries < 0 {
		return fmt.Errorf("%s: max_retries must be >= 0", where)
	}
	if r.MaxDisables != nil && *r.MaxDisables < 0 {
		return fmt.Errorf("%s: max_disables must be >= 0", where)
	}
	if r.MaxBodyBuffer != nil && int64(*r.MaxBodyBuffer) == math.MaxInt64 {
		return fmt.Errorf("%s: max_body_buffer must be < %d", where, int64(math.MaxInt64))
	}
	for _, list := range [][]int{r.CooldownOn, r.DisableOn} {
		for _, code := range list {
			if code < 100 || code > 599 {
				return fmt.Errorf("%s: status code %d out of range", where, code)
			}
		}
	}
	if a := r.Affinity; a != nil && a.Header != nil && *a.Header != "" {
		if !headerRe.MatchString(*a.Header) {
			return fmt.Errorf("%s: affinity.header %q is not a valid header name", where, *a.Header)
		}
	}
	return nil
}

// headerRe matches an HTTP field name (RFC 9110 token).
var headerRe = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_` + "`" + `|~-]+$`)

func validateHTTPURL(raw, where string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s: %q must be an absolute http(s) URL", where, raw)
	}
	return nil
}

func (k *KeyIn) validate(where string) error {
	switch {
	case k.Header != "" && k.Query != "":
		return fmt.Errorf("%s: header and query are mutually exclusive", where)
	case k.Header == "" && k.Query == "":
		return fmt.Errorf("%s: one of header or query is required", where)
	case k.Prefix != "" && k.Header == "":
		return fmt.Errorf("%s: prefix requires header", where)
	}
	if k.Header != "" && !headerRe.MatchString(k.Header) {
		return fmt.Errorf("%s: invalid HTTP header name %q", where, k.Header)
	}
	return nil
}

func (c *Capture) validate(where string) error {
	if c == nil {
		return nil
	}
	if c.MaxBytes != nil && int64(*c.MaxBytes) > 8<<20 {
		return fmt.Errorf("%s: max_bytes must be <= 8MiB", where)
	}
	return nil
}

func (h *History) validate(where string) error {
	if h == nil {
		return nil
	}
	if h.Size != nil && (*h.Size < 1 || *h.Size > 100000) {
		return fmt.Errorf("%s: size must be in 1..100000", where)
	}
	if h.Errors != nil && (*h.Errors < 1 || *h.Errors > 100000) {
		return fmt.Errorf("%s: errors must be in 1..100000", where)
	}
	if h.MaxBytes != nil && int64(*h.MaxBytes) < 1<<20 {
		return fmt.Errorf("%s: max_bytes must be >= 1MiB", where)
	}
	return nil
}

func (d *Dashboard) validate(where string) error {
	if d == nil {
		return nil
	}
	if d.Watch != nil && time.Duration(*d.Watch) < time.Second {
		return fmt.Errorf("%s: watch must be >= 1s", where)
	}
	return nil
}

var routeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func (c *Config) validate() error {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8787"
	}
	if len(c.Auth.Tokens) == 0 {
		return errors.New("auth.tokens: at least one gateway token is required")
	}
	for i, t := range c.Auth.Tokens {
		if strings.TrimSpace(t) == "" {
			return fmt.Errorf("auth.tokens[%d] is empty", i)
		}
	}
	// A password with no user next to it means the deployment only bothered
	// to set the secret — name the account for it instead of refusing to
	// boot over a missing username.
	if c.Auth.PasswordHash != "" && c.Auth.User == "" {
		c.Auth.User = DefaultDashboardUser
	}
	if err := c.Auth.validateLogin(); err != nil {
		return err
	}
	if err := c.Rotation.validate("rotation"); err != nil {
		return err
	}
	if err := c.History.validate("history"); err != nil {
		return err
	}
	if err := c.Dashboard.validate("dashboard"); err != nil {
		return err
	}
	if len(c.Providers) == 0 && len(c.Sidecar) == 0 {
		return errors.New("no providers or sidecars configured")
	}

	routes := map[string]string{}
	claim := func(route, owner string) error {
		if !routeRe.MatchString(route) {
			return fmt.Errorf("%s: route %q must match %s", owner, route, routeRe)
		}
		if route == "healthz" || route == "status" || route == "dashboard" {
			return fmt.Errorf("%s: route %q is reserved", owner, route)
		}
		if prev, dup := routes[route]; dup {
			return fmt.Errorf("%s: route %q already used by %s", owner, route, prev)
		}
		routes[route] = owner
		return nil
	}

	for name, p := range c.Providers {
		where := "providers." + name
		if p == nil {
			return fmt.Errorf("%s: empty section", where)
		}
		if err := claim(name, where); err != nil {
			return err
		}
		if err := validateHTTPURL(p.BaseURL, where+": base_url"); err != nil {
			return err
		}
		if err := p.KeyIn.validate(where + ": key_in"); err != nil {
			return err
		}
		if len(p.Keys) == 0 {
			return fmt.Errorf("%s: at least one key is required", where)
		}
		for i, k := range p.Keys {
			if strings.TrimSpace(k) == "" {
				return fmt.Errorf("%s: keys[%d] is empty", where, i)
			}
		}
		if err := p.Rotation.validate(where + ": rotation"); err != nil {
			return err
		}
		if err := p.Capture.validate(where + ": capture"); err != nil {
			return err
		}
	}

	for name, s := range c.Sidecar {
		where := "sidecar." + name
		if s == nil {
			return fmt.Errorf("%s: empty section", where)
		}
		if s.Route == "" {
			s.Route = name
		}
		if err := claim(s.Route, where); err != nil {
			return err
		}
		hasCmd, hasURL := len(s.Command) > 0, s.URL != ""
		switch {
		case hasCmd && hasURL:
			return fmt.Errorf("%s: command and url are mutually exclusive", where)
		case !hasCmd && !hasURL:
			return fmt.Errorf("%s: either command (+port) or url is required", where)
		case hasCmd && (s.Port < 1 || s.Port > 65535):
			return fmt.Errorf("%s: port must be in 1..65535", where)
		case hasURL:
			if err := validateHTTPURL(s.URL, where+": url"); err != nil {
				return err
			}
		}
		switch {
		case s.Key != "" && s.KeyIn == nil:
			return fmt.Errorf("%s: key requires key_in", where)
		case s.Key == "" && s.KeyIn != nil:
			return fmt.Errorf("%s: key_in requires key", where)
		case s.KeyIn != nil:
			if err := s.KeyIn.validate(where + ": key_in"); err != nil {
				return err
			}
		}
		if err := s.Capture.validate(where + ": capture"); err != nil {
			return err
		}
	}
	return nil
}
