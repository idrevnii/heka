// Package config loads and validates the heka YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen    string               `yaml:"listen"`
	Auth      Auth                 `yaml:"auth"`
	Rotation  *Rotation            `yaml:"rotation"`
	Providers map[string]*Provider `yaml:"providers"`
	Sidecar   map[string]*Sidecar  `yaml:"sidecar"`
}

type Auth struct {
	Tokens []string `yaml:"tokens"`
}

// Rotation holds optional overrides; nil fields fall back to the level above.
type Rotation struct {
	CooldownBase  *Duration `yaml:"cooldown_base"`
	CooldownMax   *Duration `yaml:"cooldown_max"`
	MaxRetries    *int      `yaml:"max_retries"`
	MaxBodyBuffer *Size     `yaml:"max_body_buffer"`
	CooldownOn    []int     `yaml:"cooldown_on"`
	DisableOn     []int     `yaml:"disable_on"`
}

// RotationParams is a fully resolved rotation policy.
type RotationParams struct {
	CooldownBase  time.Duration
	CooldownMax   time.Duration
	MaxRetries    int
	MaxBodyBuffer int64
	CooldownOn    []int
	DisableOn     []int
}

type Provider struct {
	BaseURL  string    `yaml:"base_url"`
	KeyIn    KeyIn     `yaml:"key_in"`
	Keys     []string  `yaml:"keys"`
	Rotation *Rotation `yaml:"rotation"`
}

// KeyIn describes where the provider expects its API key: exactly one of
// Header or Query must be set; Prefix is prepended to the header value.
type KeyIn struct {
	Header string `yaml:"header"`
	Prefix string `yaml:"prefix"`
	Query  string `yaml:"query"`
}

type Sidecar struct {
	Route   string   `yaml:"route"`
	Command []string `yaml:"command"`
	Port    int      `yaml:"port"`
	URL     string   `yaml:"url"`
	// Optional static key injected into sidecar requests (for CLIProxyAPI
	// instances that require their own api-key). No rotation.
	KeyIn *KeyIn `yaml:"key_in"`
	Key   string `yaml:"key"`
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
	switch m[2] {
	case "KiB":
		n <<= 10
	case "MiB":
		n <<= 20
	case "GiB":
		n <<= 30
	}
	if n <= 0 {
		return fmt.Errorf("size %q must be positive", raw)
	}
	*s = Size(n)
	return nil
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if doc.Kind == 0 {
		return nil, fmt.Errorf("%s: config is empty", path)
	}
	var missing []string
	expandEnvNode(&doc, &missing)
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s: undefined environment variables: %s",
			path, strings.Join(missing, ", "))
	}
	// Re-emit and strictly re-parse: the YAML emitter quotes substituted
	// values, so secrets stay opaque strings whatever characters they hold.
	data, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
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
		MaxBodyBuffer: 10 << 20,
		CooldownOn:    []int{429},
		DisableOn:     []int{401, 402, 403},
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
}

func (r *Rotation) validate(where string) error {
	if r == nil {
		return nil
	}
	if r.MaxRetries != nil && *r.MaxRetries < 0 {
		return fmt.Errorf("%s: max_retries must be >= 0", where)
	}
	for _, list := range [][]int{r.CooldownOn, r.DisableOn} {
		for _, code := range list {
			if code < 100 || code > 599 {
				return fmt.Errorf("%s: status code %d out of range", where, code)
			}
		}
	}
	return nil
}

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
	if err := c.Rotation.validate("rotation"); err != nil {
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
		if route == "healthz" || route == "status" {
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
	}
	return nil
}
