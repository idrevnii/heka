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
}

// RotationParams is a fully resolved rotation policy.
type RotationParams struct {
	CooldownBase  time.Duration
	CooldownMax   time.Duration
	MaxRetries    int
	MaxBodyBuffer int64
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
	data, err := expandEnv(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: config is empty", path)
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func expandEnv(data []byte) ([]byte, error) {
	var missing []string
	out := envRe.ReplaceAllFunc(data, func(m []byte) []byte {
		name := string(envRe.FindSubmatch(m)[1])
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
			return m
		}
		return []byte(v)
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("undefined environment variables: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// DefaultRotation returns the built-in rotation defaults with the global
// section applied.
func (c *Config) DefaultRotation() RotationParams {
	p := RotationParams{
		CooldownBase:  60 * time.Second,
		CooldownMax:   time.Hour,
		MaxRetries:    3,
		MaxBodyBuffer: 10 << 20,
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
	if r := c.Rotation; r != nil && r.MaxRetries != nil && *r.MaxRetries < 0 {
		return errors.New("rotation.max_retries must be >= 0")
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
		u, err := url.Parse(p.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%s: base_url %q must be an absolute http(s) URL", where, p.BaseURL)
		}
		switch {
		case p.KeyIn.Header != "" && p.KeyIn.Query != "":
			return fmt.Errorf("%s: key_in: header and query are mutually exclusive", where)
		case p.KeyIn.Header == "" && p.KeyIn.Query == "":
			return fmt.Errorf("%s: key_in: one of header or query is required", where)
		case p.KeyIn.Prefix != "" && p.KeyIn.Header == "":
			return fmt.Errorf("%s: key_in: prefix requires header", where)
		}
		if len(p.Keys) == 0 {
			return fmt.Errorf("%s: at least one key is required", where)
		}
		for i, k := range p.Keys {
			if strings.TrimSpace(k) == "" {
				return fmt.Errorf("%s: keys[%d] is empty", where, i)
			}
		}
		if p.Rotation != nil && p.Rotation.MaxRetries != nil && *p.Rotation.MaxRetries < 0 {
			return fmt.Errorf("%s: rotation.max_retries must be >= 0", where)
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
			u, err := url.Parse(s.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("%s: url %q must be an absolute http(s) URL", where, s.URL)
			}
		}
	}
	return nil
}
