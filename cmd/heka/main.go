// Command heka is a minimal passthrough AI gateway with API key rotation.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/idrevnii/heka/internal/config"
	"github.com/idrevnii/heka/internal/keypool"
	"github.com/idrevnii/heka/internal/proxy"
	"github.com/idrevnii/heka/internal/server"
	"github.com/idrevnii/heka/internal/sidecar"
)

func main() {
	configPath := flag.String("config", "heka.yaml", "path to config file")
	logJSON := flag.Bool("log-json", false, "log as JSON instead of text")
	flag.Parse()

	var handler slog.Handler = slog.NewTextHandler(os.Stderr, nil)
	if *logJSON {
		handler = slog.NewJSONHandler(os.Stderr, nil)
	}
	log := slog.New(handler)

	if err := run(*configPath, log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
		// Pass compressed bodies through untouched.
		DisableCompression: true,
	}

	routes := map[string]http.Handler{}
	pools := map[string]*keypool.Pool{}
	for name, p := range cfg.Providers {
		rot := cfg.RotationFor(name)
		pool := keypool.New(keypool.Config{
			CooldownBase: rot.CooldownBase,
			CooldownMax:  rot.CooldownMax,
		}, p.Keys)
		target, err := url.Parse(p.BaseURL)
		if err != nil {
			return fmt.Errorf("provider %s: %w", name, err)
		}
		routes[name] = &proxy.Handler{
			Name:   name,
			Target: target,
			KeyIn:  p.KeyIn,
			Pool:   pool,
			Params: proxy.Params{
				MaxRetries:    rot.MaxRetries,
				MaxBodyBuffer: rot.MaxBodyBuffer,
				CooldownOn:    rot.CooldownOn,
				DisableOn:     rot.DisableOn,
			},
			Transport: transport,
			Log:       log,
		}
		pools[name] = pool
	}

	var supervisors []*sidecar.Supervisor
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		// Runs on every exit path, including startup errors: stop the
		// children and wait so no sidecar is orphaned past os.Exit.
		cancel()
		for _, sup := range supervisors {
			sup.Wait(8 * time.Second)
		}
	}()

	sidecarStates := map[string]func() string{}
	for name, sc := range cfg.Sidecar {
		rawURL := sc.URL
		if rawURL == "" {
			rawURL = fmt.Sprintf("http://127.0.0.1:%d", sc.Port)
		}
		target, err := url.Parse(rawURL)
		if err != nil {
			return fmt.Errorf("sidecar %s: %w", name, err)
		}
		sup := &sidecar.Supervisor{Name: name, Command: sc.Command, Port: sc.Port, URL: sc.URL, Log: log}
		sup.Start(ctx)
		supervisors = append(supervisors, sup)
		sidecarStates[name] = sup.State

		h := &proxy.Handler{
			Name:      name,
			Target:    target,
			Params:    proxy.Params{MaxBodyBuffer: cfg.DefaultRotation().MaxBodyBuffer},
			Transport: transport,
			Log:       log,
		}
		if sc.Key != "" {
			h.KeyIn = *sc.KeyIn
			h.StaticKey = sc.Key
		}
		routes[sc.Route] = sidecar.Gate(sup, h)
	}

	srv := server.New(cfg.Auth.Tokens, routes, pools, sidecarStates, log)
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	log.Info("heka listening",
		"addr", cfg.Listen,
		"routes", slices.Sorted(maps.Keys(routes)))

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case sig := <-sigCh:
		log.Info("shutting down", "signal", sig.String())
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		log.Warn("shutdown", "error", err)
	}
	return nil
}
