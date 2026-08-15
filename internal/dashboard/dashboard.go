// Package dashboard serves the embedded web dashboard: a JSON API over
// internal/history and internal/app, plus the static frontend that renders
// it. Authentication is handled by internal/server before a request ever
// reaches this package.
package dashboard

import (
	"bytes"
	"compress/gzip"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/idrevnii/heka/internal/app"
	"github.com/idrevnii/heka/internal/config"
	"github.com/idrevnii/heka/internal/history"
	"github.com/idrevnii/heka/internal/proxy"
)

//go:embed static
var staticFiles embed.FS

// Backend is what the dashboard needs from the running gateway. *app.App
// implements it; tests can supply a fake.
type Backend interface {
	ReadConfig() (data []byte, version, path string, err error)
	ValidateConfig(data []byte) error
	ApplyBytes(data []byte, ifVersion string) (app.Result, error)
	ReloadFromDisk() (app.Result, error)
	DashboardParams() config.DashboardParams
	StatusSnapshot() map[string]any
	ResetStatus(provider string) ([]string, error)
	ResetStatusKey(provider string, key int) error
	CheckKey(ctx context.Context, provider string, key int) (app.CheckResult, error)
}

type Handler struct {
	backend Backend
	history *history.Store
	log     *slog.Logger
	mux     *http.ServeMux
	static  http.Handler
}

// New builds the dashboard's HTTP handler. Mount it at "/dashboard" (and
// everything under it) — internal/server already scopes auth to that
// prefix before delegating here.
func New(backend Backend, hist *history.Store, log *slog.Logger) *Handler {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic("dashboard: embedded static assets: " + err.Error())
	}
	h := &Handler{
		backend: backend,
		history: hist,
		log:     log,
		static:  http.StripPrefix("/dashboard/", http.FileServerFS(sub)),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /dashboard/api/overview", h.handleOverview)
	mux.HandleFunc("GET /dashboard/api/requests", h.handleRequests)
	mux.HandleFunc("GET /dashboard/api/requests/{id}", h.handleRequestDetail)
	mux.HandleFunc("GET /dashboard/api/errors", h.handleErrors)
	mux.HandleFunc("GET /dashboard/api/status", h.handleStatus)
	mux.HandleFunc("POST /dashboard/api/status/reset", h.handleStatusReset)
	mux.HandleFunc("POST /dashboard/api/status/check", h.handleStatusCheck)
	mux.HandleFunc("GET /dashboard/api/config", h.handleConfigGet)
	mux.HandleFunc("PUT /dashboard/api/config", h.handleConfigPut)
	mux.HandleFunc("POST /dashboard/api/config/validate", h.handleConfigValidate)
	mux.HandleFunc("POST /dashboard/api/reload", h.handleReload)
	mux.HandleFunc("GET /dashboard", h.handleRedirect)
	mux.HandleFunc("GET /dashboard/", h.handleStaticFile)
	h.mux = mux
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) handleRedirect(w http.ResponseWriter, r *http.Request) {
	target := "/dashboard/"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func (h *Handler) handleStaticFile(w http.ResponseWriter, r *http.Request) {
	h.static.ServeHTTP(w, r)
}

func (h *Handler) handleOverview(w http.ResponseWriter, r *http.Request) {
	proxy.WriteJSON(w, http.StatusOK, h.history.Overview(5*time.Minute))
}

func (h *Handler) handleRequests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := history.Filter{
		Limit:      atoiDefault(q.Get("limit"), 100),
		Route:      q.Get("route"),
		OnlyErrors: q.Get("errors") == "1",
	}
	if v := q.Get("min_status"); v != "" {
		f.MinStatus, _ = strconv.Atoi(v)
	}
	if v := q.Get("max_status"); v != "" {
		f.MaxStatus, _ = strconv.Atoi(v)
	}
	if v := q.Get("since"); v != "" {
		f.SinceID, _ = strconv.ParseUint(v, 10, 64)
	}
	proxy.WriteJSON(w, http.StatusOK, h.history.List(f))
}

// bodyView is the JSON-friendly rendering of a captured body: gzip-decoded
// when possible (captured bytes are often gzip, since the outbound
// transport passes compression through untouched) and either UTF-8 text or
// base64 for anything that isn't.
type bodyView struct {
	Text      string `json:"text,omitempty"`
	Encoding  string `json:"encoding,omitempty"` // "utf8" | "base64"
	Truncated bool   `json:"truncated,omitempty"`
	Decoded   bool   `json:"decoded,omitempty"`
}

func renderBody(data []byte, truncated bool, contentEncoding string) *bodyView {
	if len(data) == 0 {
		return nil
	}
	decoded := false
	if contentEncoding == "gzip" {
		if gr, err := gzip.NewReader(bytes.NewReader(data)); err == nil {
			if out, err := io.ReadAll(io.LimitReader(gr, 4<<20)); err == nil {
				data = out
				decoded = true
			}
		}
	}
	bv := &bodyView{Truncated: truncated, Decoded: decoded}
	if utf8.Valid(data) {
		bv.Text = string(data)
		bv.Encoding = "utf8"
	} else {
		bv.Text = base64.StdEncoding.EncodeToString(data)
		bv.Encoding = "base64"
	}
	return bv
}

type recordDetail struct {
	history.Record
	ReqBody  *bodyView `json:"req_body,omitempty"`
	RespBody *bodyView `json:"resp_body,omitempty"`
}

func (h *Handler) handleRequestDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		proxy.WriteError(w, http.StatusBadRequest, "invalid id")
		return
	}
	rec, ok := h.history.Get(id)
	if !ok {
		proxy.WriteError(w, http.StatusNotFound, "heka: no such request")
		return
	}
	detail := recordDetail{
		Record:   rec,
		ReqBody:  renderBody(rec.ReqBody, rec.ReqTrunc, ""),
		RespBody: renderBody(rec.RespBody, rec.RespTrunc, rec.RespEnc),
	}
	proxy.WriteJSON(w, http.StatusOK, detail)
}

func (h *Handler) handleErrors(w http.ResponseWriter, r *http.Request) {
	limit := atoiDefault(r.URL.Query().Get("limit"), 100)
	proxy.WriteJSON(w, http.StatusOK, h.history.Errors(limit))
}

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	proxy.WriteJSON(w, http.StatusOK, h.backend.StatusSnapshot())
}

// handleStatusReset clears key state: the whole gateway, one provider
// (?provider=), or a single key of one provider (?provider=&key=<index>) —
// the last being how the dashboard's per-key button un-benches a key an
// operator has just fixed upstream.
func (h *Handler) handleStatusReset(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	provider := q.Get("provider")
	if raw := q.Get("key"); raw != "" {
		if provider == "" {
			proxy.WriteError(w, http.StatusBadRequest, "heka: key requires provider")
			return
		}
		idx, err := strconv.Atoi(raw)
		if err != nil {
			proxy.WriteError(w, http.StatusBadRequest, "heka: invalid key index")
			return
		}
		if err := h.backend.ResetStatusKey(provider, idx); err != nil {
			proxy.WriteError(w, http.StatusNotFound, "heka: "+err.Error())
			return
		}
		proxy.WriteJSON(w, http.StatusOK, map[string]any{"reset": []string{provider}, "key": idx})
		return
	}
	reset, err := h.backend.ResetStatus(provider)
	if err != nil {
		proxy.WriteError(w, http.StatusNotFound, "heka: "+err.Error())
		return
	}
	proxy.WriteJSON(w, http.StatusOK, map[string]any{"reset": reset})
}

// handleStatusCheck probes one key of one provider with a real (one-token)
// request. Unlike reset, which only trusts the operator, this asks the
// upstream — the only source that knows whether a key benched a week ago is
// actually usable again.
func (h *Handler) handleStatusCheck(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	idx, err := strconv.Atoi(q.Get("key"))
	if err != nil {
		proxy.WriteError(w, http.StatusBadRequest, "heka: invalid key index")
		return
	}
	res, err := h.backend.CheckKey(r.Context(), q.Get("provider"), idx)
	if err != nil {
		proxy.WriteError(w, http.StatusBadRequest, "heka: "+err.Error())
		return
	}
	proxy.WriteJSON(w, http.StatusOK, res)
}

func (h *Handler) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	data, version, path, err := h.backend.ReadConfig()
	if err != nil {
		proxy.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	params := h.backend.DashboardParams()
	proxy.WriteJSON(w, http.StatusOK, map[string]any{
		"path":     path,
		"content":  string(data),
		"version":  version,
		"editable": params.ConfigEdit,
	})
}

func (h *Handler) handleConfigValidate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content string `json:"content"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if err := h.backend.ValidateConfig([]byte(body.Content)); err != nil {
		proxy.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	proxy.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleConfigPut(w http.ResponseWriter, r *http.Request) {
	if !h.backend.DashboardParams().ConfigEdit {
		proxy.WriteError(w, http.StatusForbidden, "heka: config editing is disabled")
		return
	}
	var body struct {
		Content string `json:"content"`
		Version string `json:"version"`
	}
	if !readJSON(w, r, &body) {
		return
	}

	res, err := h.backend.ApplyBytes([]byte(body.Content), body.Version)
	if err != nil {
		var verr *app.ValidationError
		var cerr *app.ConflictError
		var perr *app.PersistError
		switch {
		case errors.As(err, &verr):
			proxy.WriteError(w, http.StatusBadRequest, err.Error())
		case errors.As(err, &cerr):
			data, version, _, rerr := h.backend.ReadConfig()
			body := map[string]any{
				"error": map[string]string{"type": "heka_gateway_error", "message": err.Error()},
			}
			if rerr == nil {
				body["current_version"] = version
				body["current_content"] = string(data)
			}
			proxy.WriteJSON(w, http.StatusConflict, body)
		case errors.As(err, &perr):
			proxy.WriteJSON(w, http.StatusInternalServerError, map[string]any{
				"error":  map[string]string{"type": "heka_gateway_error", "message": err.Error()},
				"result": res,
			})
		default:
			proxy.WriteError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	proxy.WriteJSON(w, http.StatusOK, res)
}

func (h *Handler) handleReload(w http.ResponseWriter, r *http.Request) {
	res, err := h.backend.ReloadFromDisk()
	if err != nil {
		proxy.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	proxy.WriteJSON(w, http.StatusOK, res)
}

// readJSON decodes a size-capped JSON request body. On failure it writes
// the error response itself and returns false.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		proxy.WriteError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
