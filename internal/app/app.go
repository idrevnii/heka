// Package app owns the live gateway built from a Config — provider key
// pools, sidecar supervisors, and the server routing table — and knows how
// to rebuild that world from a new Config and swap it in without dropping
// in-flight requests or leaking child processes.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/idrevnii/heka/internal/config"
	"github.com/idrevnii/heka/internal/history"
	"github.com/idrevnii/heka/internal/keypool"
	"github.com/idrevnii/heka/internal/proxy"
	"github.com/idrevnii/heka/internal/server"
	"github.com/idrevnii/heka/internal/sidecar"
)

// Options configures a new App. All fields are required except SeedPath.
type Options struct {
	ConfigPath string
	// SeedPath, if set, is copied to ConfigPath on first boot when
	// ConfigPath does not yet exist.
	SeedPath  string
	Transport http.RoundTripper
	Log       *slog.Logger
	History   *history.Store
}

// Changes summarizes what a reload added, removed or updated. Sidecar
// entries are prefixed "sidecar:" to disambiguate from provider names.
type Changes struct {
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	Updated []string `json:"updated,omitempty"`
}

// Result reports what happened after applying a config.
type Result struct {
	Version         string    `json:"version"`
	AppliedAt       time.Time `json:"applied_at"`
	RestartRequired []string  `json:"restart_required,omitempty"`
	Changed         Changes   `json:"changed"`
}

// ValidationError wraps a config parse/validate failure — the caller's
// fault, maps to an HTTP 400.
type ValidationError struct{ Err error }

func (e *ValidationError) Error() string { return e.Err.Error() }
func (e *ValidationError) Unwrap() error { return e.Err }

// ConflictError reports that the config on disk has moved on since the
// caller last read it — maps to an HTTP 409.
type ConflictError struct{ Current string }

func (e *ConflictError) Error() string {
	return fmt.Sprintf("config was modified concurrently (current version %s)", e.Current)
}

// PersistError reports that a config was successfully applied in memory but
// could not be written to disk — the running process is correct, but the
// change won't survive a restart until this is fixed. Maps to an HTTP 500.
type PersistError struct{ Err error }

func (e *PersistError) Error() string { return "applied but not persisted: " + e.Err.Error() }
func (e *PersistError) Unwrap() error { return e.Err }

type provInst struct {
	poolFP    string
	handlerFP string
	pool      *keypool.Pool
	handler   *proxy.Handler
}

type sideInst struct {
	supFP     string
	handlerFP string
	sup       *sidecar.Supervisor
	cancel    context.CancelFunc
	handler   http.Handler
}

// App owns the live gateway state and every goroutine/child-process it has
// started; Shutdown is the only way every one of them is guaranteed to
// stop.
type App struct {
	opts Options

	// mu serializes apply/read of the fields below. It is never held while
	// a request is in flight — request handling reads server.Server's own
	// atomically-swapped state, not App's.
	mu sync.Mutex

	rootCtx  context.Context
	rootStop context.CancelFunc
	wg       sync.WaitGroup // tracks retiring (grace-delayed) sidecar supervisors

	srv *server.Server

	cfg           *config.Config
	version       string
	appliedAt     time.Time
	listen        string // frozen at first successful apply; the bound listener can't move
	historyParams config.HistoryParams

	providers map[string]*provInst
	sidecars  map[string]*sideInst
}

// New loads (seeding first if configured and absent) and applies the config
// at opts.ConfigPath, returning a ready-to-serve App.
func New(opts Options) (*App, error) {
	if opts.SeedPath != "" {
		if _, err := os.Stat(opts.ConfigPath); errors.Is(err, fs.ErrNotExist) {
			if err := seedConfig(opts.SeedPath, opts.ConfigPath); err != nil {
				return nil, fmt.Errorf("seeding config from %s: %w", opts.SeedPath, err)
			}
		} else if err != nil {
			return nil, err
		}
	}

	raw, err := os.ReadFile(opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Parse(raw, opts.ConfigPath)
	if err != nil {
		return nil, err
	}

	rootCtx, rootStop := context.WithCancel(context.Background())
	a := &App{
		opts:      opts,
		rootCtx:   rootCtx,
		rootStop:  rootStop,
		providers: map[string]*provInst{},
		sidecars:  map[string]*sideInst{},
	}
	if _, err := a.apply(cfg, raw); err != nil {
		rootStop()
		return nil, err
	}
	return a, nil
}

func seedConfig(seedPath, configPath string) error {
	data, err := os.ReadFile(seedPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(configPath, data, 0o600)
}

// Server returns the http.Handler to run the gateway (and dashboard) on.
func (a *App) Server() http.Handler { return a.srv }

// Addr returns the listen address bound at first boot. It never changes
// across reloads — see RestartRequired.
func (a *App) Addr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.listen
}

// SetDashboard installs the dashboard handler onto the underlying server.
func (a *App) SetDashboard(h http.Handler) { a.srv.SetDashboard(h) }

// ApplyBytes validates, hot-applies and persists a config submitted through
// the dashboard's editor. ifVersion, when non-empty, must match the current
// on-disk config's version or the call fails with a *ConflictError without
// applying anything (optimistic concurrency: protects against two open
// editor tabs clobbering each other).
func (a *App) ApplyBytes(data []byte, ifVersion string) (Result, error) {
	cfg, err := config.Parse(data, "dashboard: config")
	if err != nil {
		return Result{}, &ValidationError{err}
	}

	a.mu.Lock()
	current := a.version
	a.mu.Unlock()
	if ifVersion != "" && ifVersion != current {
		return Result{}, &ConflictError{Current: current}
	}

	res, err := a.apply(cfg, data)
	if err != nil {
		return Result{}, err
	}
	if err := config.Save(a.opts.ConfigPath, data, true, 10); err != nil {
		return res, &PersistError{err}
	}
	return res, nil
}

// ReloadFromDisk re-reads and re-applies the config file as-is (SIGHUP, or
// the periodic watcher noticing an out-of-band edit).
func (a *App) ReloadFromDisk() (Result, error) {
	raw, err := os.ReadFile(a.opts.ConfigPath)
	if err != nil {
		return Result{}, err
	}
	cfg, err := config.Parse(raw, a.opts.ConfigPath)
	if err != nil {
		return Result{}, &ValidationError{err}
	}
	return a.apply(cfg, raw)
}

// Watch polls the config file every `every` and reloads when its content
// changes — this is what makes editing the file directly on a volume work
// without exec'ing into the container. Comparing against the hash of the
// last *applied* config (rather than mtime) means it never re-applies its
// own ApplyBytes writes. Returns when ctx is done.
func (a *App) Watch(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 10 * time.Second
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			raw, err := os.ReadFile(a.opts.ConfigPath)
			if err != nil {
				a.opts.Log.Warn("config watch: read failed", "error", err)
				continue
			}
			a.mu.Lock()
			changed := config.Version(raw) != a.version
			a.mu.Unlock()
			if !changed {
				continue
			}
			if _, err := a.ReloadFromDisk(); err != nil {
				a.opts.Log.Warn("config watch: reload failed", "error", err)
			} else {
				a.opts.Log.Info("config reloaded from disk")
			}
		}
	}
}

// ReadConfig returns the raw, unmasked file content and its version.
func (a *App) ReadConfig() (data []byte, version, path string, err error) {
	path = a.opts.ConfigPath
	data, err = os.ReadFile(path)
	if err != nil {
		return nil, "", path, err
	}
	return data, config.Version(data), path, nil
}

// ValidateConfig dry-runs Parse without applying anything.
func (a *App) ValidateConfig(data []byte) error {
	_, err := config.Parse(data, "validate")
	return err
}

// DashboardParams returns the currently applied dashboard policy.
func (a *App) DashboardParams() config.DashboardParams {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg == nil {
		return config.DashboardParams{Enabled: true, ConfigEdit: true, Watch: 10 * time.Second}
	}
	return a.cfg.DefaultDashboard()
}

// StatusSnapshot mirrors the shape of the existing /status endpoint —
// masked key states per provider plus sidecar health.
func (a *App) StatusSnapshot() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	providers := map[string][]keypool.KeyStatus{}
	for name, pi := range a.providers {
		providers[name] = pi.pool.Snapshot()
	}
	body := map[string]any{"providers": providers}
	if len(a.sidecars) > 0 {
		sidecars := map[string]string{}
		for name, si := range a.sidecars {
			sidecars[name] = si.sup.State()
		}
		body["sidecars"] = sidecars
	}
	return body
}

// ResetStatus mirrors /status/reset: clears cooldown/disabled state for one
// provider, or all of them when provider is "".
func (a *App) ResetStatus(provider string) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if provider != "" {
		pi, ok := a.providers[provider]
		if !ok {
			return nil, fmt.Errorf("unknown provider %q", provider)
		}
		pi.pool.Reset()
		return []string{provider}, nil
	}
	var reset []string
	for name, pi := range a.providers {
		pi.pool.Reset()
		reset = append(reset, name)
	}
	return reset, nil
}

// Shutdown stops the root context (tearing down every sidecar supervisor,
// including ones already retired-but-pending from earlier reloads) and
// waits for everything to finish, up to timeout.
func (a *App) Shutdown(timeout time.Duration) {
	a.mu.Lock()
	sidecars := make([]*sideInst, 0, len(a.sidecars))
	for _, si := range a.sidecars {
		sidecars = append(sidecars, si)
	}
	a.mu.Unlock()

	a.rootStop()
	for _, si := range sidecars {
		si.sup.Wait(timeout)
	}
	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// apply builds the world implied by cfg and swaps it in. Phase 1 (this
// function, up to the swap) either succeeds entirely or returns an error
// having made no visible change — config.validate() already guarantees
// every URL in cfg parses, so a failure here is effectively impossible in
// practice, which is exactly what makes "no rollback needed" true. raw may
// be nil (only used to compute the applied version).
func (a *App) apply(cfg *config.Config, raw []byte) (Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	var added, removed, updated []string

	newProviders := map[string]*provInst{}
	routes := map[string]http.Handler{}
	pools := map[string]*keypool.Pool{}

	for name, p := range cfg.Providers {
		rot := cfg.RotationFor(name)
		capture := cfg.CaptureFor(name)
		poolFP := fingerprint(p.Keys, rot.CooldownBase, rot.CooldownMax)
		handlerFP := fingerprint(p.BaseURL, p.KeyIn, rot.MaxRetries, rot.MaxBodyBuffer,
			rot.CooldownOn, rot.DisableOn, rot.Affinity, capture)

		prev, existed := a.providers[name]

		var pool *keypool.Pool
		if existed && prev.poolFP == poolFP {
			pool = prev.pool
		} else {
			pool = keypool.New(keypool.Config{CooldownBase: rot.CooldownBase, CooldownMax: rot.CooldownMax}, p.Keys)
		}

		var handler *proxy.Handler
		if existed && prev.poolFP == poolFP && prev.handlerFP == handlerFP {
			handler = prev.handler
		} else {
			target, err := url.Parse(p.BaseURL)
			if err != nil {
				return Result{}, fmt.Errorf("provider %s: %w", name, err)
			}
			handler = &proxy.Handler{
				Name:   name,
				Kind:   "provider",
				Target: target,
				KeyIn:  p.KeyIn,
				Pool:   pool,
				Params: proxy.Params{
					MaxRetries:    rot.MaxRetries,
					MaxBodyBuffer: rot.MaxBodyBuffer,
					CooldownOn:    rot.CooldownOn,
					DisableOn:     rot.DisableOn,
				},
				Affinity:  rot.Affinity,
				Capture:   capture,
				History:   a.opts.History,
				Transport: a.opts.Transport,
				Log:       a.opts.Log,
			}
		}

		newProviders[name] = &provInst{poolFP: poolFP, handlerFP: handlerFP, pool: pool, handler: handler}
		routes[name] = handler
		pools[name] = pool

		switch {
		case !existed:
			added = append(added, name)
		case prev.poolFP != poolFP || prev.handlerFP != handlerFP:
			updated = append(updated, name)
		}
	}
	for name := range a.providers {
		if _, ok := cfg.Providers[name]; !ok {
			removed = append(removed, name)
		}
	}

	newSidecars := map[string]*sideInst{}
	sidecarStates := map[string]func() string{}

	for name, sc := range cfg.Sidecar {
		capture := cfg.CaptureFor(name)
		supFP := fingerprint(sc.Command, sc.Port, sc.URL, healthIntervalValue(sc.HealthInterval))
		handlerFP := fingerprint(sc.Key, keyInValue(sc.KeyIn), capture, sc.Route)

		prev, existed := a.sidecars[name]

		var sup *sidecar.Supervisor
		var cancel context.CancelFunc
		switch {
		case !existed:
			sup, cancel = a.startSidecar(name, sc)
			added = append(added, "sidecar:"+name)
		case prev.supFP == supFP:
			sup, cancel = prev.sup, prev.cancel
		default:
			if len(sc.Command) > 0 {
				// Stop first: the new child would otherwise race the old
				// one for the same port and crash-loop on bind failure.
				prev.cancel()
				prev.sup.Wait(8 * time.Second)
				sup, cancel = a.startSidecar(name, sc)
			} else {
				sup, cancel = a.startSidecar(name, sc)
				a.retireSidecar(prev)
			}
			updated = append(updated, "sidecar:"+name)
		}

		rawURL := sc.URL
		if rawURL == "" {
			rawURL = fmt.Sprintf("http://127.0.0.1:%d", sc.Port)
		}
		target, err := url.Parse(rawURL)
		if err != nil {
			return Result{}, fmt.Errorf("sidecar %s: %w", name, err)
		}

		var handler http.Handler
		if existed && prev.supFP == supFP && prev.handlerFP == handlerFP {
			handler = prev.handler
		} else {
			hh := &proxy.Handler{
				Name:      name,
				Kind:      "sidecar",
				Target:    target,
				Params:    proxy.Params{MaxBodyBuffer: cfg.DefaultRotation().MaxBodyBuffer},
				Capture:   capture,
				History:   a.opts.History,
				Transport: a.opts.Transport,
				Log:       a.opts.Log,
			}
			if sc.Key != "" {
				hh.KeyIn = *sc.KeyIn
				hh.StaticKey = sc.Key
			}
			handler = sidecar.Gate(sup, hh)
		}

		newSidecars[name] = &sideInst{supFP: supFP, handlerFP: handlerFP, sup: sup, cancel: cancel, handler: handler}
		routes[sc.Route] = handler
		sidecarStates[name] = sup.State
	}
	for name, prev := range a.sidecars {
		if _, ok := cfg.Sidecar[name]; !ok {
			a.retireSidecar(prev)
			removed = append(removed, "sidecar:"+name)
		}
	}

	// Swap: single atomic pointer store (or first-time construction).
	if a.srv == nil {
		a.srv = server.New(cfg.Auth.Tokens, routes, pools, sidecarStates, a.opts.Log)
	} else {
		a.srv.Swap(&server.State{
			Tokens:        cfg.Auth.Tokens,
			Routes:        routes,
			Pools:         pools,
			SidecarStates: sidecarStates,
		})
	}

	hp := cfg.DefaultHistory()
	if hp != a.historyParams {
		a.opts.History.Resize(hp.Size, hp.Errors, hp.MaxBytes)
		a.historyParams = hp
	}

	var restartRequired []string
	if a.listen == "" {
		a.listen = cfg.Listen
	} else if cfg.Listen != a.listen {
		restartRequired = append(restartRequired, "listen")
	}

	a.providers = newProviders
	a.sidecars = newSidecars
	a.cfg = cfg
	a.appliedAt = time.Now()
	if raw != nil {
		a.version = config.Version(raw)
	}

	return Result{
		Version:         a.version,
		AppliedAt:       a.appliedAt,
		RestartRequired: restartRequired,
		Changed:         Changes{Added: added, Removed: removed, Updated: updated},
	}, nil
}

// startSidecar starts a fresh supervisor rooted under the app's lifetime.
func (a *App) startSidecar(name string, sc *config.Sidecar) (*sidecar.Supervisor, context.CancelFunc) {
	var healthInterval time.Duration
	if sc.HealthInterval != nil {
		healthInterval = time.Duration(*sc.HealthInterval)
	}
	sup := &sidecar.Supervisor{
		Name: name, Command: sc.Command, Port: sc.Port, URL: sc.URL,
		Interval: healthInterval, Log: a.opts.Log,
	}
	ctx, cancel := context.WithCancel(a.rootCtx)
	sup.Start(ctx)
	return sup, cancel
}

// retireSidecar stops a supervisor no longer referenced by any route. A
// spawned child gets a grace period first, so requests already talking to
// it (issued by a handler object obtained before the swap) get a chance to
// finish before it's killed; an externally-URL'd sidecar has no process to
// kill, so there's nothing to wait for.
func (a *App) retireSidecar(prev *sideInst) {
	if len(prev.sup.Command) == 0 {
		prev.cancel()
		return
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		time.Sleep(5 * time.Second)
		prev.cancel()
		prev.sup.Wait(8 * time.Second)
	}()
}

func keyInValue(k *config.KeyIn) config.KeyIn {
	if k == nil {
		return config.KeyIn{}
	}
	return *k
}

func healthIntervalValue(d *config.Duration) time.Duration {
	if d == nil {
		return 0
	}
	return time.Duration(*d)
}

// fingerprint hashes a stable string representation of parts. It's used to
// tell whether a provider/sidecar's configuration actually changed across a
// reload, so unchanged ones can keep their live key-pool state / child
// process instead of being rebuilt from scratch.
func fingerprint(parts ...any) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%#v|", p)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
