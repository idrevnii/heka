package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/idrevnii/heka/internal/app"
	"github.com/idrevnii/heka/internal/config"
	"github.com/idrevnii/heka/internal/history"
)

type fakeBackend struct {
	content   string
	version   string
	editable  bool
	applyErr  error
	applied   string
	statusMap map[string]any
	resetKeys []string
	checked   []string
}

func (f *fakeBackend) ReadConfig() ([]byte, string, string, error) {
	return []byte(f.content), f.version, "/fake/heka.yaml", nil
}

func (f *fakeBackend) ValidateConfig(data []byte) error {
	if strings.Contains(string(data), "bad") {
		return errors.New("invalid: contains bad")
	}
	return nil
}

func (f *fakeBackend) ApplyBytes(data []byte, ifVersion string) (app.Result, error) {
	if f.applyErr != nil {
		return app.Result{}, f.applyErr
	}
	if ifVersion != "" && ifVersion != f.version {
		return app.Result{}, &app.ConflictError{Current: f.version}
	}
	f.applied = string(data)
	f.version = "v2"
	return app.Result{Version: f.version, AppliedAt: time.Now()}, nil
}

func (f *fakeBackend) ReloadFromDisk() (app.Result, error) {
	return app.Result{Version: f.version}, nil
}

func (f *fakeBackend) DashboardParams() config.DashboardParams {
	return config.DashboardParams{Enabled: true, ConfigEdit: f.editable, Watch: 10 * time.Second}
}

func (f *fakeBackend) StatusSnapshot() map[string]any { return f.statusMap }

func (f *fakeBackend) ResetStatus(provider string) ([]string, error) {
	if provider == "missing" {
		return nil, errors.New("unknown provider")
	}
	return []string{provider}, nil
}

func (f *fakeBackend) ResetStatusKey(provider string, key int) error {
	if provider == "missing" {
		return errors.New("unknown provider")
	}
	if key != 0 {
		return errors.New("no such key")
	}
	f.resetKeys = append(f.resetKeys, provider)
	return nil
}

func (f *fakeBackend) CheckKey(ctx context.Context, provider string, key int) (app.CheckResult, error) {
	if provider == "missing" {
		return app.CheckResult{}, errors.New("unknown provider")
	}
	f.checked = append(f.checked, provider)
	return app.CheckResult{OK: true, Status: 200, Message: "{}"}, nil
}

func newTestHandler() (*Handler, *fakeBackend, *history.Store) {
	be := &fakeBackend{content: "auth: {}", version: "v1", editable: true, statusMap: map[string]any{"providers": map[string]any{}}}
	store := history.New(10, 10, 1<<20)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(be, store, log), be, store
}

func TestOverviewAndRequests(t *testing.T) {
	h, _, store := newTestHandler()
	store.Add(history.Record{Route: "mock", Status: 200, Duration: 5})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/dashboard/api/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("overview status = %d", rec.Code)
	}
	var ov map[string]any
	json.Unmarshal(rec.Body.Bytes(), &ov)
	if ov["total"].(float64) != 1 {
		t.Fatalf("overview total = %v", ov["total"])
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/dashboard/api/requests", nil))
	var list []map[string]any
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 {
		t.Fatalf("requests = %d", len(list))
	}
	if _, hasBody := list[0]["req_body"]; hasBody {
		t.Fatal("list view should not include bodies")
	}
}

func TestRequestDetailRendersBody(t *testing.T) {
	h, _, store := newTestHandler()
	id := store.Add(history.Record{Route: "mock", Status: 200, ReqBody: []byte(`{"a":1}`)})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/dashboard/api/requests/"+itoa(id), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var detail map[string]any
	json.Unmarshal(rec.Body.Bytes(), &detail)
	reqBody := detail["req_body"].(map[string]any)
	if reqBody["text"] != `{"a":1}` || reqBody["encoding"] != "utf8" {
		t.Fatalf("req_body = %+v", reqBody)
	}
}

func TestConfigGetPutValidateFlow(t *testing.T) {
	h, be, _ := newTestHandler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/dashboard/api/config", nil))
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["version"] != "v1" || got["content"] != "auth: {}" {
		t.Fatalf("config get = %+v", got)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/dashboard/api/config/validate", strings.NewReader(`{"content":"bad config"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("validate bad = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("PUT", "/dashboard/api/config", strings.NewReader(`{"content":"new content","version":"v1"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("put = %d: %s", rec.Code, rec.Body)
	}
	if be.applied != "new content" {
		t.Fatalf("backend did not receive the new content: %q", be.applied)
	}

	// Stale version now that the backend moved to v2.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("PUT", "/dashboard/api/config", strings.NewReader(`{"content":"x","version":"v1"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale put = %d, want 409", rec.Code)
	}
}

func TestConfigEditDisabled(t *testing.T) {
	h, be, _ := newTestHandler()
	be.editable = false

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/dashboard/api/config", strings.NewReader(`{"content":"x","version":"v1"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("put with editing disabled = %d, want 403", rec.Code)
	}
}

func TestStatusResetUnknownProvider(t *testing.T) {
	h, _, _ := newTestHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/dashboard/api/status/reset?provider=missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestStatusResetSingleKey(t *testing.T) {
	h, be, _ := newTestHandler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/dashboard/api/status/reset?provider=anthropic&key=0", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if len(be.resetKeys) != 1 || be.resetKeys[0] != "anthropic" {
		t.Fatalf("backend saw %v, want one anthropic key reset", be.resetKeys)
	}

	for _, q := range []string{"?key=0", "?provider=anthropic&key=nope"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/dashboard/api/status/reset"+q, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("reset %q = %d, want 400", q, rec.Code)
		}
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/dashboard/api/status/reset?provider=anthropic&key=9", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("reset out-of-range key = %d, want 404", rec.Code)
	}
}

func itoa(id uint64) string {
	b := []byte{}
	if id == 0 {
		return "0"
	}
	for id > 0 {
		b = append([]byte{byte('0' + id%10)}, b...)
		id /= 10
	}
	return string(b)
}
