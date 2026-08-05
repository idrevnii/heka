// Package proxy implements the passthrough reverse proxy with key injection
// and the retry/rotation loop.
package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/idrevnii/heka/internal/config"
	"github.com/idrevnii/heka/internal/history"
	"github.com/idrevnii/heka/internal/keypool"
)

type Params struct {
	MaxRetries    int
	MaxBodyBuffer int64
	CooldownOn    []int
	DisableOn     []int
}

// Handler proxies requests for one upstream. Pool is nil for upstreams
// without key rotation (sidecar routes); StaticKey optionally authenticates
// such an upstream with a single fixed key. A zero KeyIn means no key
// injection. The request URL path must already have the route prefix
// stripped.
type Handler struct {
	Name      string
	Kind      string // "provider" | "sidecar", recorded into history
	Target    *url.URL
	KeyIn     config.KeyIn
	Pool      *keypool.Pool
	StaticKey string
	Params    Params
	Affinity  config.AffinityParams
	Capture   config.CaptureParams
	History   *history.Store // nil-safe; no history is recorded when nil
	Transport http.RoundTripper
	Log       *slog.Logger
}

type verdict int

const (
	vOK verdict = iota
	vRateLimited
	vInvalidKey
	vRetryable
)

func (v verdict) String() string {
	switch v {
	case vRateLimited:
		return "rate_limited"
	case vInvalidKey:
		return "invalid_key"
	case vRetryable:
		return "upstream_error"
	}
	return "ok"
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Buffering exists to replay the body on retry (needs a pool) and/or to
	// capture it for the dashboard; without either, the body streams
	// straight through.
	var (
		buf        []byte
		replayable bool
	)
	if h.Pool != nil || h.Capture.Body {
		var err error
		buf, replayable, err = h.readBody(r)
		if err != nil {
			WriteError(w, http.StatusBadRequest, "failed to read request body: "+err.Error())
			h.finish(r, start, finishInfo{
				status: http.StatusBadRequest, keyIdx: -1, verdict: "bad_request", err: err,
			})
			return
		}
	}
	reqSink := captureRequest(buf, replayable, h.Capture)

	keyIdx, secret := -1, h.StaticKey
	affinity, bound := uint64(0), false
	if h.Pool != nil {
		var ok bool
		affinity, bound = affinityOf(r, buf, h.Affinity)
		keyIdx, secret, ok = h.acquire(affinity, bound, nil)
		if !ok {
			WriteError(w, http.StatusServiceUnavailable,
				fmt.Sprintf("heka: no usable keys for provider %q (all disabled)", h.Name))
			h.finish(r, start, finishInfo{
				status: http.StatusServiceUnavailable, keyIdx: -1, verdict: "no_keys", reqBody: reqSink,
			})
			return
		}
	}
	tried := map[int]bool{keyIdx: true}

	var (
		resp     *http.Response
		lastErr  error
		attempts int
		lastV    verdict
	)
	for {
		attempts++
		out, err := h.outbound(r, secret, buf, replayable)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, "heka: "+err.Error())
			h.finish(r, start, finishInfo{
				status: http.StatusInternalServerError, attempts: attempts, keyIdx: keyIdx,
				verdict: "internal_error", err: err, reqBody: reqSink,
			})
			return
		}
		resp, lastErr = h.Transport.RoundTrip(out)
		if lastErr != nil && r.Context().Err() != nil {
			// The client went away; the key is fine and there is no one
			// to answer — no failure bookkeeping, no rotation.
			h.finish(r, start, finishInfo{
				status: 499, attempts: attempts, keyIdx: keyIdx, verdict: "client_gone", reqBody: reqSink,
			})
			return
		}
		v := classify(resp, lastErr, h.Params)
		lastV = v
		h.report(v, keyIdx, resp)
		if v == vOK || h.Pool == nil || !replayable || attempts > h.Params.MaxRetries {
			break
		}
		nextIdx, nextSecret, ok := h.acquire(affinity, bound, tried)
		if !ok {
			break
		}
		tried[nextIdx] = true
		if resp != nil {
			drain(resp.Body)
		}
		h.Log.Warn("rotating key",
			"provider", h.Name,
			"reason", v.String(),
			"status", statusOf(resp, lastErr),
			"key", h.Pool.MaskedKey(keyIdx),
			"attempt", attempts)
		keyIdx, secret = nextIdx, nextSecret
	}

	if lastErr != nil {
		WriteError(w, http.StatusBadGateway, "heka: upstream error: "+lastErr.Error())
		h.finish(r, start, finishInfo{
			status: http.StatusBadGateway, attempts: attempts, keyIdx: keyIdx,
			verdict: lastV.String(), err: lastErr, reqBody: reqSink,
		})
		return
	}
	defer resp.Body.Close()

	streaming := isStreaming(resp)
	var respSink *sink
	if h.Capture.Body && (!streaming || h.Capture.Streaming) {
		respSink = newSink(h.Capture.MaxBytes)
	}
	var scanner *usageScanner
	if lastV == vOK {
		scanner = newUsageScanner(resp)
	}
	copyResponse(w, resp, respSink, scanner)
	usage := scanner.close()
	if h.Pool != nil && keyIdx >= 0 {
		h.Pool.ReportUsage(keyIdx, usage)
	}
	h.finish(r, start, finishInfo{
		status: resp.StatusCode, attempts: attempts, keyIdx: keyIdx, verdict: lastV.String(),
		reqBody: reqSink, respBody: respSink, streaming: streaming, usage: usage,
		reqCT: r.Header.Get("Content-Type"), respCT: resp.Header.Get("Content-Type"),
		respEnc: resp.Header.Get("Content-Encoding"),
	})
}

// acquire picks the next key, honouring the request's affinity when one
// could be derived. Retries keep the same affinity: rendezvous hashing then
// gives a stable second choice, so a conversation that loses its key lands
// on the same replacement every time instead of scattering.
func (h *Handler) acquire(affinity uint64, bound bool, tried map[int]bool) (int, string, bool) {
	if bound {
		return h.Pool.AcquireFor(affinity, tried)
	}
	return h.Pool.Acquire(tried)
}

// readBody buffers up to MaxBodyBuffer bytes. When the body fits, it is
// replayable across retries; otherwise the request runs as a single attempt
// streaming the remainder from the client.
func (h *Handler) readBody(r *http.Request) ([]byte, bool, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, true, nil
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, h.Params.MaxBodyBuffer+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(buf)) > h.Params.MaxBodyBuffer {
		return buf, false, nil
	}
	return buf, true, nil
}

func (h *Handler) outbound(r *http.Request, secret string, buf []byte, replayable bool) (*http.Request, error) {
	basePath := strings.TrimSuffix(h.Target.EscapedPath(), "/")
	rest := r.URL.EscapedPath()
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	raw := h.Target.Scheme + "://" + h.Target.Host + basePath + rest
	rawQuery := h.Target.RawQuery
	if rawQuery != "" && r.URL.RawQuery != "" {
		rawQuery += "&"
	}
	rawQuery += r.URL.RawQuery
	if rawQuery != "" {
		raw += "?" + rawQuery
	}

	var body io.Reader
	switch {
	case replayable:
		if len(buf) > 0 {
			body = bytes.NewReader(buf)
		}
	case buf != nil:
		// Partially buffered oversized body: replay the prefix, stream the rest.
		body = io.MultiReader(bytes.NewReader(buf), r.Body)
	default:
		body = r.Body
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, raw, body)
	if err != nil {
		return nil, err
	}
	if !replayable && r.ContentLength > 0 {
		out.ContentLength = r.ContentLength
	}

	out.Header = r.Header.Clone()
	stripHopByHop(out.Header)
	// The gateway token must never reach the upstream; the provider key
	// replaces whatever auth the client sent.
	out.Header.Del("Authorization")
	out.Header.Del("X-Api-Key")
	out.Header.Del("X-Goog-Api-Key")
	// A gateway-level routing hint; the upstream has no use for it.
	if h.Affinity.Header != "" {
		out.Header.Del(h.Affinity.Header)
	}
	if secret != "" {
		switch {
		case h.KeyIn.Header != "":
			out.Header.Set(h.KeyIn.Header, h.KeyIn.Prefix+secret)
		case h.KeyIn.Query != "":
			out.URL.RawQuery = replaceQueryParam(out.URL.RawQuery, h.KeyIn.Query, secret)
		}
	}
	return out, nil
}

// replaceQueryParam removes every existing value for name and appends the
// provider value. Unrelated fields retain their original order and encoding.
func replaceQueryParam(rawQuery, name, value string) string {
	kv := url.QueryEscape(name) + "=" + url.QueryEscape(value)
	if rawQuery == "" {
		return kv
	}

	parts := strings.Split(rawQuery, "&")
	kept := make([]string, 0, len(parts)+1)
	for _, part := range parts {
		escapedName, _, _ := strings.Cut(part, "=")
		decodedName, err := url.QueryUnescape(escapedName)
		if err == nil && decodedName == name {
			continue
		}
		kept = append(kept, part)
	}
	kept = append(kept, kv)
	return strings.Join(kept, "&")
}

func (h *Handler) report(v verdict, idx int, resp *http.Response) {
	if h.Pool == nil || idx < 0 {
		return
	}
	switch v {
	case vOK:
		h.Pool.ReportSuccess(idx)
	case vRateLimited:
		ra, hasRA := retryAfter(resp)
		d := h.Pool.ReportRateLimited(idx, ra, hasRA)
		h.Log.Warn("key cooling down", "provider", h.Name, "key", h.Pool.MaskedKey(idx), "cooldown", d)
	case vInvalidKey:
		h.Pool.ReportInvalid(idx)
		h.Log.Warn("key disabled", "provider", h.Name, "key", h.Pool.MaskedKey(idx), "status", resp.StatusCode)
	case vRetryable:
		h.Pool.ReportFailure(idx)
	}
}

func classify(resp *http.Response, err error, p Params) verdict {
	if err != nil {
		return vRetryable
	}
	switch {
	case slices.Contains(p.CooldownOn, resp.StatusCode):
		return vRateLimited
	case slices.Contains(p.DisableOn, resp.StatusCode):
		return vInvalidKey
	case resp.StatusCode >= 500:
		return vRetryable
	}
	return vOK
}

// retryAfter parses the Retry-After header; ok reports whether the provider
// supplied a usable value (so 0 can be told apart from "no header").
func retryAfter(resp *http.Response) (d time.Duration, ok bool) {
	if resp == nil {
		return 0, false
	}
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0), true
	}
	return 0, false
}

// isStreaming reports whether a response should be treated as a live stream
// (SSE, or a body of unknown length) rather than a fixed-size payload.
func isStreaming(resp *http.Response) bool {
	return resp.ContentLength < 0 ||
		strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
}

// copyResponse streams resp's body to w. When sink is non-nil, every chunk
// written to the client is also (best-effort, size-capped) mirrored into it
// for the dashboard's request history.
func copyResponse(w http.ResponseWriter, resp *http.Response, sink *sink, usage *usageScanner) {
	stripHopByHop(resp.Header)
	maps.Copy(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	// Flush per chunk only when the response actually streams (SSE or
	// unknown length); fixed-size bodies keep the writer's coalescing.
	streaming := isStreaming(resp)
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			sink.Write(buf[:n])
			usage.Write(buf[:n])
			if streaming {
				rc.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func stripHopByHop(h http.Header) {
	for _, f := range strings.Split(h.Get("Connection"), ",") {
		if f = textproto.TrimString(f); f != "" {
			h.Del(f)
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

func drain(body io.ReadCloser) {
	io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	body.Close()
}

func statusOf(resp *http.Response, err error) any {
	if err != nil {
		return err.Error()
	}
	return resp.StatusCode
}

// sink is a size-capped in-memory body capture buffer. A nil *sink silently
// discards everything written to it, so callers can pass nil unconditionally
// when capture is off.
type sink struct {
	buf       []byte
	max       int64
	truncated bool
}

func newSink(max int64) *sink {
	if max <= 0 {
		return nil
	}
	return &sink{max: max}
}

func (s *sink) Write(p []byte) {
	if s == nil || len(p) == 0 {
		return
	}
	room := s.max - int64(len(s.buf))
	if room <= 0 {
		s.truncated = true
		return
	}
	if int64(len(p)) > room {
		s.buf = append(s.buf, p[:room]...)
		s.truncated = true
		return
	}
	s.buf = append(s.buf, p...)
}

// captureRequest returns a sink pre-filled from the already-buffered request
// body when capture is enabled — no extra buffering pass. buf may be a
// truncated prefix (see readBody/outbound) when the body exceeded
// MaxBodyBuffer; that's reflected as truncated too.
func captureRequest(buf []byte, replayable bool, capture config.CaptureParams) *sink {
	if !capture.Body || len(buf) == 0 {
		return nil
	}
	s := newSink(capture.MaxBytes)
	if s == nil {
		return nil
	}
	s.Write(buf)
	if !replayable {
		s.truncated = true
	}
	return s
}

// finishInfo carries everything finish needs to both log (matching the
// previous logRequest's shape exactly) and, when a history.Store is
// configured, record the request.
type finishInfo struct {
	status, attempts, keyIdx int
	verdict                  string
	err                      error
	reqCT, respCT, respEnc   string
	streaming                bool
	reqBody, respBody        *sink
	usage                    keypool.Usage
}

// finish is the single exit point for every ServeHTTP return path: it logs
// one line (as logRequest always did) and, if capture produced anything (or
// even if it didn't — CaptureSkip records why), appends a history.Record.
func (h *Handler) finish(r *http.Request, start time.Time, fi finishInfo) {
	duration := time.Since(start)
	attrs := []any{
		"provider", h.Name,
		"method", r.Method,
		"path", r.URL.Path,
		"status", fi.status,
		"attempts", fi.attempts,
		"duration", duration.Round(time.Millisecond).String(),
	}
	if h.Pool != nil && fi.keyIdx >= 0 {
		attrs = append(attrs, "key", h.Pool.MaskedKey(fi.keyIdx))
	}
	h.Log.Info("request", attrs...)

	if h.History == nil {
		return
	}
	rec := history.Record{
		Time:      start,
		Route:     h.Name,
		Kind:      h.Kind,
		Method:    r.Method,
		Path:      r.URL.Path,
		Query:     r.URL.RawQuery,
		Upstream:  h.Target.String() + r.URL.Path,
		Status:    fi.status,
		Attempts:  fi.attempts,
		Duration:  duration.Milliseconds(),
		Verdict:   fi.verdict,
		Streaming: fi.streaming,
		ReqCT:     fi.reqCT,
		RespCT:    fi.respCT,
		RespEnc:   fi.respEnc,

		InputTokens:      fi.usage.Input,
		CacheReadTokens:  fi.usage.CacheRead,
		CacheWriteTokens: fi.usage.CacheWrite,
		OutputTokens:     fi.usage.Output,
	}
	if h.Pool != nil && fi.keyIdx >= 0 {
		rec.Key = h.Pool.MaskedKey(fi.keyIdx)
	}
	if fi.err != nil {
		rec.Err = fi.err.Error()
	}
	if fi.reqBody != nil {
		rec.ReqBody = fi.reqBody.buf
		rec.ReqTrunc = fi.reqBody.truncated
	}
	switch {
	case fi.respBody != nil:
		rec.RespBody = fi.respBody.buf
		rec.RespTrunc = fi.respBody.truncated
	case !h.Capture.Body:
		rec.CaptureSkip = "disabled"
	case fi.streaming:
		rec.CaptureSkip = "streaming"
	}
	h.History.Add(rec)
}

// WriteJSON writes a JSON response body with the given status code.
func WriteJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(body)
}

// WriteError writes a JSON error body in a shape any API client can parse.
func WriteError(w http.ResponseWriter, code int, msg string) {
	WriteJSON(w, code, map[string]any{
		"error": map[string]string{"type": "heka_gateway_error", "message": msg},
	})
}
