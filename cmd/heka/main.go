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
			KeyIn: proxy.KeyIn{
				Header: p.KeyIn.Header,
				Prefix: p.KeyIn.Prefix,
				Query:  p.KeyIn.Query,
			},
			Pool:      pool,
			Params:    proxy.Params{MaxRetries: rot.MaxRetries, MaxBodyBuffer: rot.MaxBodyBuffer},
			Transport: transport,
			Log:       log,
		}
		pools[name] = pool
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var supervisors []*sidecar.Supervisor
	sidecarStates := map[string]func() string{}
	for name, sc := range cfg.Sidecar {
		params := proxy.Params{MaxBodyBuffer: cfg.DefaultRotation().MaxBodyBuffer}
		if sc.URL != "" {
			target, err := url.Parse(sc.URL)
			if err != nil {
				return fmt.Errorf("sidecar %s: %w", name, err)
			}
			routes[sc.Route] = &proxy.Handler{
				Name: name, Target: target, Params: params, Transport: transport, Log: log,
			}
			sidecarStates[name] = func() string { return "external" }
			continue
		}
		sup := &sidecar.Supervisor{Name: name, Command: sc.Command, Port: sc.Port, Log: log}
		sup.Start(ctx)
		supervisors = append(supervisors, sup)
		sidecarStates[name] = sup.State
		target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", sc.Port))
		routes[sc.Route] = sidecar.Gate(sup, &proxy.Handler{
			Name: name, Target: target, Params: params, Transport: transport, Log: log,
		})
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
	cancel()
	for _, sup := range supervisors {
		sup.Wait(8 * time.Second)
	}
	return nil
}
