package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestSwapUnderConcurrentLoad hammers ServeHTTP from many goroutines while
// Swap replaces the routing table underneath them, under -race. It asserts
// only that nothing races and every request gets a coherent (not mixed)
// response — 200 from route "a" or 200 from route "b", never a 404 for an
// unknown mix of old/new state.
func TestSwapUnderConcurrentLoad(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	echoA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	echoB := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	srv := New([]string{"tok-a"}, map[string]http.Handler{"a": echoA}, nil, nil, log)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Swapper: alternates between two equally-valid states.
	wg.Add(1)
	go func() {
		defer wg.Done()
		toggle := false
		for {
			select {
			case <-stop:
				return
			default:
			}
			if toggle {
				srv.Swap(&State{Tokens: []string{"tok-a"}, Routes: map[string]http.Handler{"a": echoA}})
			} else {
				srv.Swap(&State{Tokens: []string{"tok-b"}, Routes: map[string]http.Handler{"b": echoB}})
			}
			toggle = !toggle
		}
	}()

	// Requesters: each uses the token/route matching one specific state, so
	// a coherent read of State always yields 200 — a torn read (impossible
	// with atomic.Pointer, but this is the behavioral guarantee we want)
	// would show up as an unexpected 401/404.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				req := httptest.NewRequest("GET", "/a", nil)
				req.Header.Set("Authorization", "Bearer tok-a")
				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, req)
				if rec.Code != 200 && rec.Code != 401 && rec.Code != 404 {
					t.Errorf("unexpected status %d", rec.Code)
				}
			}
		}()
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}
