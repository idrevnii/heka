// Package proxy implements the passthrough reverse proxy with key injection
// and the retry/rotation loop.
package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/idrevnii/heka/internal/keypool"
)

// KeyIn mirrors config.KeyIn; exactly one of Header or Query is set.
// A zero KeyIn means no key injection (sidecar routes).
type KeyIn struct {
	Header string
	Prefix string
	Query  string
}

type Params struct {
	MaxRetries    int
	MaxBodyBuffer int64
}

// Handler proxies requests for one upstream. Pool is nil for upstreams
// without key rotation (sidecar routes). The request URL path must already
// have the route prefix stripped.
type Handler struct {
	Name      string
	Target    *url.URL
	KeyIn     KeyIn
	Pool      *keypool.Pool
	Params    Params
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

	buf, replayable, err := h.readBody(r)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "failed to read request body: "+err.Error())
		return
	}

	keyIdx, secret := -1, ""
	if h.Pool != nil {
		var ok bool
		keyIdx, secret, ok = h.Pool.Acquire(nil)
		if !ok {
			WriteError(w, http.StatusServiceUnavailable,
				fmt.Sprintf("heka: no usable keys for provider %q (all disabled)", h.Name))
			h.logRequest(r, start, http.StatusServiceUnavailable, 0, -1)
			return
		}
	}
	tried := map[int]bool{keyIdx: true}

	var (
		resp     *http.Response
		lastErr  error
		attempts int
	)
	for {
		attempts++
		out, err := h.outbound(r, secret, buf, replayable)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, "heka: "+err.Error())
			return
		}
		resp, lastErr = h.Transport.RoundTrip(out)
		v := classify(resp, lastErr)
		h.report(v, keyIdx, resp)
		if v == vOK || h.Pool == nil || !replayable || attempts > h.Params.MaxRetries {
			break
		}
		nextIdx, nextSecret, ok := h.Pool.Acquire(tried)
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
		h.logRequest(r, start, http.StatusBadGateway, attempts, keyIdx)
		return
	}
	defer resp.Body.Close()
	copyResponse(w, resp)
	h.logRequest(r, start, resp.StatusCode, attempts, keyIdx)
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
	if r.URL.RawQuery != "" {
		raw += "?" + r.URL.RawQuery
	}

	var body io.Reader
	if replayable {
		if len(buf) > 0 {
			body = bytes.NewReader(buf)
		}
	} else {
		body = io.MultiReader(bytes.NewReader(buf), r.Body)
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
	if secret != "" {
		switch {
		case h.KeyIn.Header != "":
			out.Header.Set(h.KeyIn.Header, h.KeyIn.Prefix+secret)
		case h.KeyIn.Query != "":
			q := out.URL.Query()
			q.Set(h.KeyIn.Query, secret)
			out.URL.RawQuery = q.Encode()
		}
	}
	return out, nil
}

func (h *Handler) report(v verdict, idx int, resp *http.Response) {
	if h.Pool == nil || idx < 0 {
		return
	}
	switch v {
	case vOK:
		h.Pool.ReportSuccess(idx)
	case vRateLimited:
		d := h.Pool.ReportRateLimited(idx, retryAfter(resp))
		h.Log.Warn("key cooling down", "provider", h.Name, "key", h.Pool.MaskedKey(idx), "cooldown", d)
	case vInvalidKey:
		h.Pool.ReportInvalid(idx)
		h.Log.Warn("key disabled", "provider", h.Name, "key", h.Pool.MaskedKey(idx), "status", resp.StatusCode)
	case vRetryable:
		h.Pool.ReportFailure(idx)
	}
}

func classify(resp *http.Response, err error) verdict {
	if err != nil {
		return vRetryable
	}
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return vRateLimited
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden:
		return vInvalidKey
	}
	if resp.StatusCode >= 500 {
		return vRetryable
	}
	return vOK
}

func retryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func copyResponse(w http.ResponseWriter, resp *http.Response) {
	stripHopByHop(resp.Header)
	dst := w.Header()
	for k, vv := range resp.Header {
		dst[k] = vv
	}
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			// Flush after every chunk so SSE streams pass through live.
			rc.Flush()
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

func (h *Handler) logRequest(r *http.Request, start time.Time, status, attempts, keyIdx int) {
	attrs := []any{
		"provider", h.Name,
		"method", r.Method,
		"path", r.URL.Path,
		"status", status,
		"attempts", attempts,
		"duration", time.Since(start).Round(time.Millisecond).String(),
	}
	if h.Pool != nil && keyIdx >= 0 {
		attrs = append(attrs, "key", h.Pool.MaskedKey(keyIdx))
	}
	h.Log.Info("request", attrs...)
}

// WriteError writes a JSON error body in a shape any API client can parse.
func WriteError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"type": "heka_gateway_error", "message": msg},
	})
}
