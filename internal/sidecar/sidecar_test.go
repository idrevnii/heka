package sidecar

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// External-URL mode must healthcheck the instance just like a supervised one.
func TestExternalURLHealthcheck(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	sup := &Supervisor{
		Name:     "ext",
		URL:      up.URL,
		Interval: 20 * time.Millisecond,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup.Start(ctx)

	waitFor(t, "healthy", sup.Healthy)
	if sup.State() != "healthy" {
		t.Fatalf("state = %q", sup.State())
	}

	up.Close()
	waitFor(t, "down after upstream death", func() bool { return !sup.Healthy() })

	// Gate must answer 503 while the upstream is down.
	rec := httptest.NewRecorder()
	Gate(sup, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("gate let a request through while unhealthy")
	})).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("gate = %d, want 503", rec.Code)
	}

	// No child process: Wait must return immediately.
	done := make(chan struct{})
	go func() { sup.Wait(5 * time.Second); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wait blocked for URL-mode supervisor")
	}
}

func TestRedirectIsHealthyAndCancellationClosesGate(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusFound)
	}))
	defer up.Close()
	sup := &Supervisor{URL: up.URL, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup.Start(ctx)
	waitFor(t, "healthy despite redirect loop", sup.Healthy)
	cancel()
	waitFor(t, "unhealthy after cancellation", func() bool { return !sup.Healthy() })
}
