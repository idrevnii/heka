// Command heka is a minimal passthrough AI gateway with API key rotation.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/idrevnii/heka/internal/app"
	"github.com/idrevnii/heka/internal/dashboard"
	"github.com/idrevnii/heka/internal/history"
)

func main() {
	configPath := flag.String("config", "heka.yaml", "path to config file")
	seedPath := flag.String("seed", "", "default config copied to -config on first boot if it doesn't exist yet")
	logJSON := flag.Bool("log-json", false, "log as JSON instead of text")
	flag.Parse()

	var base slog.Handler = slog.NewTextHandler(os.Stderr, nil)
	if *logJSON {
		base = slog.NewJSONHandler(os.Stderr, nil)
	}

	if err := run(*configPath, *seedPath, base); err != nil {
		slog.New(base).Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(configPath, seedPath string, base slog.Handler) error {
	// The history store must exist before the logger (it captures
	// Warn/Error log lines) and the logger before the config load — so it
	// starts with built-in defaults and is resized to the configured
	// values as soon as the first config is applied.
	store := history.New(500, 200, 32<<20)
	log := slog.New(history.NewLogHandler(base, store))

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

	a, err := app.New(app.Options{
		ConfigPath: configPath,
		SeedPath:   seedPath,
		Transport:  transport,
		Log:        log,
		History:    store,
	})
	if err != nil {
		return err
	}
	defer a.Shutdown(15 * time.Second)

	a.SetDashboard(dashboard.New(a, store, log))

	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	go a.Watch(watchCtx, a.DashboardParams().Watch)

	httpSrv := &http.Server{
		Addr:              a.Addr(),
		Handler:           a.Server(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	log.Info("heka listening", "addr", a.Addr(), "config", configPath)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
loop:
	for {
		select {
		case err := <-errCh:
			return err
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				if _, err := a.ReloadFromDisk(); err != nil {
					log.Warn("SIGHUP reload failed", "error", err)
				} else {
					log.Info("config reloaded via SIGHUP")
				}
				continue loop
			}
			log.Info("shutting down", "signal", sig.String())
			break loop
		}
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		log.Warn("shutdown", "error", err)
	}
	return nil
}
