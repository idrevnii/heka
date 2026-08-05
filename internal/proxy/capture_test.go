package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/idrevnii/heka/internal/config"
	"github.com/idrevnii/heka/internal/history"
)

func TestCaptureDisabledByDefault(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Write([]byte(`{"ok":true}`))
	})
	h, _ := newHandler(t, up.srv.URL, config.KeyIn{Header: "x-api-key"}, key1)
	store := history.New(10, 10, 1<<20)
	h.History = store

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/x", strings.NewReader(`{"hello":"world"}`))
	h.ServeHTTP(rec, req)

	list := store.List(history.Filter{})
	if len(list) != 1 {
		t.Fatalf("records = %d", len(list))
	}
	got, _ := store.Get(list[0].ID)
	if got.ReqBody != nil || got.RespBody != nil {
		t.Fatalf("capture disabled: expected no bodies, got req=%q resp=%q", got.ReqBody, got.RespBody)
	}
	if got.CaptureSkip != "disabled" {
		t.Fatalf("CaptureSkip = %q, want %q", got.CaptureSkip, "disabled")
	}
}

func TestCaptureEnabledStoresBothBodies(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"echo":true}`))
	})
	h, _ := newHandler(t, up.srv.URL, config.KeyIn{Header: "x-api-key"}, key1)
	h.Capture = config.CaptureParams{Body: true, MaxBytes: 1 << 16}
	store := history.New(10, 10, 1<<20)
	h.History = store

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/x", strings.NewReader(`{"hello":"world"}`))
	h.ServeHTTP(rec, req)

	list := store.List(history.Filter{})
	got, _ := store.Get(list[0].ID)
	if string(got.ReqBody) != `{"hello":"world"}` {
		t.Fatalf("req body = %q", got.ReqBody)
	}
	if string(got.RespBody) != `{"echo":true}` {
		t.Fatalf("resp body = %q", got.RespBody)
	}
}

func TestCaptureNeverIncludesProviderKey(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Write([]byte("ok"))
	})
	h, _ := newHandler(t, up.srv.URL, config.KeyIn{Header: "x-api-key"}, key1)
	h.Capture = config.CaptureParams{Body: true, MaxBytes: 1 << 16}
	store := history.New(10, 10, 1<<20)
	h.History = store

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/x", strings.NewReader(`{}`))
	h.ServeHTTP(rec, req)

	list := store.List(history.Filter{})
	got, _ := store.Get(list[0].ID)
	if strings.Contains(string(got.ReqBody), key1) || strings.Contains(got.Upstream, key1) {
		t.Fatalf("captured record leaked the provider key: %+v", got)
	}
	if got.Key == "" || got.Key == key1 {
		t.Fatalf("Key field should be the masked key, got %q", got.Key)
	}
}

func TestCaptureTruncatesAtMaxBytes(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Write([]byte("0123456789"))
	})
	h, _ := newHandler(t, up.srv.URL, config.KeyIn{Header: "x-api-key"}, key1)
	h.Capture = config.CaptureParams{Body: true, MaxBytes: 4}
	store := history.New(10, 10, 1<<20)
	h.History = store

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/x", strings.NewReader("abcdefgh"))
	h.ServeHTTP(rec, req)

	list := store.List(history.Filter{})
	got, _ := store.Get(list[0].ID)
	if len(got.ReqBody) != 4 || !got.ReqTrunc {
		t.Fatalf("req capture not truncated to 4 bytes: %q trunc=%v", got.ReqBody, got.ReqTrunc)
	}
	if len(got.RespBody) != 4 || !got.RespTrunc {
		t.Fatalf("resp capture not truncated to 4 bytes: %q trunc=%v", got.RespBody, got.RespTrunc)
	}
	// The client must still receive the full, untruncated response.
	if rec.Body.String() != "0123456789" {
		t.Fatalf("client response truncated: %q", rec.Body.String())
	}
}

func TestCaptureSkipsStreamingUnlessOptedIn(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		io.WriteString(w, "data: hi\n\n")
	})
	h, _ := newHandler(t, up.srv.URL, config.KeyIn{Header: "x-api-key"}, key1)
	h.Capture = config.CaptureParams{Body: true, MaxBytes: 1 << 16, Streaming: false}
	store := history.New(10, 10, 1<<20)
	h.History = store

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/x", nil)
	h.ServeHTTP(rec, req)

	list := store.List(history.Filter{})
	got, _ := store.Get(list[0].ID)
	if got.RespBody != nil {
		t.Fatalf("streaming response should not be captured by default, got %q", got.RespBody)
	}
	if got.CaptureSkip != "streaming" {
		t.Fatalf("CaptureSkip = %q, want %q", got.CaptureSkip, "streaming")
	}
	if !got.Streaming {
		t.Fatal("Streaming flag should be true")
	}
}

func TestCaptureStreamingWhenOptedIn(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: hi\n\n")
	})
	h, _ := newHandler(t, up.srv.URL, config.KeyIn{Header: "x-api-key"}, key1)
	h.Capture = config.CaptureParams{Body: true, MaxBytes: 1 << 16, Streaming: true}
	store := history.New(10, 10, 1<<20)
	h.History = store

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/x", nil)
	h.ServeHTTP(rec, req)

	list := store.List(history.Filter{})
	got, _ := store.Get(list[0].ID)
	if string(got.RespBody) != "data: hi\n\n" {
		t.Fatalf("resp body = %q", got.RespBody)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestBadRequestBodyIsNowRecorded(t *testing.T) {
	h, _ := newHandler(t, "http://127.0.0.1:1", config.KeyIn{Header: "x-api-key"}, key1)
	store := history.New(10, 10, 1<<20)
	h.History = store

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/x", errReader{})
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	list := store.List(history.Filter{})
	if len(list) != 1 {
		t.Fatalf("expected the failed request to be recorded, got %d records", len(list))
	}
	if list[0].Status != http.StatusBadRequest || list[0].Err == "" {
		t.Fatalf("record = %+v", list[0])
	}
}

func TestOutboundBuildFailureIsNowRecorded(t *testing.T) {
	h, _ := newHandler(t, "http://127.0.0.1:1", config.KeyIn{Header: "x-api-key"}, key1)
	store := history.New(10, 10, 1<<20)
	h.History = store

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/x", strings.NewReader("{}"))
	req.Method = "BAD METHOD" // space is invalid in an HTTP method token
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	list := store.List(history.Filter{})
	if len(list) != 1 {
		t.Fatalf("expected the failed request to be recorded, got %d records", len(list))
	}
	if list[0].Status != http.StatusInternalServerError || list[0].Err == "" {
		t.Fatalf("record = %+v", list[0])
	}
}
