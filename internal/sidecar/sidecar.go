// Package sidecar supervises a child process (CLIProxyAPI) and gates its
// route on an HTTP healthcheck.
package sidecar

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/idrevnii/heka/internal/proxy"
)

type Supervisor struct {
	Name    string
	Command []string
	Port    int
	Log     *slog.Logger

	healthy atomic.Bool
	runDone chan struct{}
}

// Start launches the process and the healthcheck loop; both stop when ctx
// is cancelled.
func (s *Supervisor) Start(ctx context.Context) {
	s.runDone = make(chan struct{})
	go s.run(ctx)
	go s.health(ctx)
}

// Wait blocks until the child process has exited after ctx cancellation,
// or the timeout elapses.
func (s *Supervisor) Wait(timeout time.Duration) {
	select {
	case <-s.runDone:
	case <-time.After(timeout):
		s.Log.Warn("sidecar did not stop in time", "sidecar", s.Name)
	}
}

func (s *Supervisor) Healthy() bool { return s.healthy.Load() }

func (s *Supervisor) State() string {
	if s.Healthy() {
		return "healthy"
	}
	return "down"
}

func (s *Supervisor) run(ctx context.Context) {
	defer close(s.runDone)
	backoff := time.Second
	for {
		cmd := exec.CommandContext(ctx, s.Command[0], s.Command[1:]...)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = 5 * time.Second
		started := time.Now()
		err := cmd.Run()
		if ctx.Err() != nil {
			return
		}
		s.Log.Warn("sidecar exited", "sidecar", s.Name, "error", err, "restart_in", backoff)
		// A process that survived a while gets a fresh backoff; only
		// crash loops escalate the delay.
		if time.Since(started) > 30*time.Second {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (s *Supervisor) health(ctx context.Context) {
	client := &http.Client{Timeout: time.Second}
	target := fmt.Sprintf("http://127.0.0.1:%d/", s.Port)
	check := func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			// Any HTTP response counts as alive; only connection
			// failures mean the process is not serving yet.
			if s.healthy.Swap(false) {
				s.Log.Warn("sidecar unhealthy", "sidecar", s.Name, "error", err)
			}
			return
		}
		resp.Body.Close()
		if !s.healthy.Swap(true) {
			s.Log.Info("sidecar healthy", "sidecar", s.Name, "port", s.Port)
		}
	}
	check()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}

// Gate returns next wrapped so that requests get a 503 while the sidecar is
// not serving yet.
func Gate(s *Supervisor, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.Healthy() {
			proxy.WriteError(w, http.StatusServiceUnavailable,
				fmt.Sprintf("heka: sidecar %q is not ready", s.Name))
			return
		}
		next.ServeHTTP(w, r)
	})
}
