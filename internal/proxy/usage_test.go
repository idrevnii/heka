package proxy

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/idrevnii/heka/internal/config"
	"github.com/idrevnii/heka/internal/keypool"
)

func scan(t *testing.T, encoding, body string) keypool.Usage {
	t.Helper()
	resp := &http.Response{Header: http.Header{}}
	raw := []byte(body)
	if encoding != "" {
		resp.Header.Set("Content-Encoding", encoding)
	}
	if encoding == "gzip" {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write(raw)
		zw.Close()
		raw = buf.Bytes()
	}
	u := newUsageScanner(resp)
	// Feed in small chunks: the scanner sees network-sized reads, not whole
	// events, and must not depend on where the boundaries fall.
	for off := 0; off < len(raw); off += 7 {
		u.Write(raw[off:min(off+7, len(raw))])
	}
	return u.close()
}

func TestUsageAnthropicJSON(t *testing.T) {
	got := scan(t, "", `{"id":"msg_1","usage":{"input_tokens":10,"cache_read_input_tokens":900,
		"cache_creation_input_tokens":90,"output_tokens":25}}`)
	// input_tokens excludes both cache fields upstream; heka reports the
	// whole prompt so the hit rate is meaningful.
	want := keypool.Usage{Input: 1000, CacheRead: 900, CacheWrite: 90, Output: 25}
	if got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

func TestUsageAnthropicStream(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("event: message_start\n")
	sb.WriteString(`data: {"type":"message_start","message":{"usage":{"input_tokens":10,` +
		`"cache_read_input_tokens":900,"cache_creation_input_tokens":0,"output_tokens":1}}}` + "\n\n")
	// Plenty of deltas in between, none of them carrying accounting.
	for i := range 50 {
		sb.WriteString(fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\","+
			"\"delta\":{\"type\":\"text_delta\",\"text\":\"chunk %d\"}}\n\n", i))
	}
	sb.WriteString("event: message_delta\n")
	sb.WriteString(`data: {"type":"message_delta","usage":{"output_tokens":128}}` + "\n\n")

	got := scan(t, "", sb.String())
	// The input side comes from message_start, the final output count from
	// message_delta — neither event has both.
	want := keypool.Usage{Input: 910, CacheRead: 900, Output: 128}
	if got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

// A body split over several lines is still one document, not one payload
// per line the way an event stream is.
func TestUsagePrettyPrintedJSON(t *testing.T) {
	got := scan(t, "", "{\n  \"id\": \"msg_1\",\n  \"usage\": {\n    \"input_tokens\": 10,\n"+
		"    \"cache_read_input_tokens\": 90,\n    \"output_tokens\": 5\n  }\n}\n")
	want := keypool.Usage{Input: 100, CacheRead: 90, Output: 5}
	if got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

func TestUsageOpenAI(t *testing.T) {
	// Chat completions: prompt_tokens already includes the cached ones.
	got := scan(t, "", `{"usage":{"prompt_tokens":1000,"completion_tokens":40,
		"prompt_tokens_details":{"cached_tokens":768}}}`)
	want := keypool.Usage{Input: 1000, CacheRead: 768, Output: 40}
	if got != want {
		t.Fatalf("chat usage = %+v, want %+v", got, want)
	}
	// Responses API reuses Anthropic's field names with OpenAI's meaning:
	// input_tokens is the whole prompt, cached tokens included.
	got = scan(t, "", `{"response":{"usage":{"input_tokens":1000,"output_tokens":40,
		"input_tokens_details":{"cached_tokens":768}}}}`)
	if got != want {
		t.Fatalf("responses usage = %+v, want %+v", got, want)
	}
}

func TestUsageGemini(t *testing.T) {
	got := scan(t, "", `{"usageMetadata":{"promptTokenCount":1000,"cachedContentTokenCount":600,
		"candidatesTokenCount":30}}`)
	want := keypool.Usage{Input: 1000, CacheRead: 600, Output: 30}
	if got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

func TestUsageGzipped(t *testing.T) {
	got := scan(t, "gzip", `{"usage":{"prompt_tokens":500,"completion_tokens":10,
		"prompt_tokens_details":{"cached_tokens":400}}}`)
	want := keypool.Usage{Input: 500, CacheRead: 400, Output: 10}
	if got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

func TestUsageAbsentOrUnreadable(t *testing.T) {
	if got := scan(t, "", `{"results":[{"title":"golang"}]}`); got != (keypool.Usage{}) {
		t.Fatalf("search response reported usage: %+v", got)
	}
	if got := scan(t, "", "not json at all"); got != (keypool.Usage{}) {
		t.Fatalf("non-JSON reported usage: %+v", got)
	}
	// An encoding heka can't inflate yields no scanner at all, and a nil
	// scanner has to stay safe to write to and close.
	resp := &http.Response{Header: http.Header{"Content-Encoding": []string{"br"}}}
	u := newUsageScanner(resp)
	if u != nil {
		t.Fatalf("scanner created for brotli: %+v", u)
	}
	u.Write([]byte("whatever"))
	if got := u.close(); got != (keypool.Usage{}) {
		t.Fatalf("nil scanner reported usage: %+v", got)
	}
}

func TestUsageOversizedChunkDropped(t *testing.T) {
	// A single JSON blob past the cap is skipped rather than buffered.
	huge := `{"usage":{"prompt_tokens":1,"junk":"` + strings.Repeat("x", maxUsageChunk) + `"}}`
	if got := scan(t, "", huge); got != (keypool.Usage{}) {
		t.Fatalf("oversized body reported usage: %+v", got)
	}
}

// End to end: totals land on the key that served the request, and the
// pool's snapshot exposes the hit rate.
func TestUsageRecordedPerKey(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"usage":{"input_tokens":100,"cache_read_input_tokens":900,"output_tokens":7}}`)
	})
	h, pool := newHandler(t, up.srv.URL, config.KeyIn{Header: "X-Api-Key"}, key1, key2)

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"m"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	snap := pool.Snapshot()
	// Key 0 got the 429 and no usage; key 1 served the answer.
	if snap[0].InputTokens != 0 || snap[0].CacheHitRate != nil {
		t.Fatalf("rate-limited key charged for tokens: %+v", snap[0])
	}
	if snap[1].InputTokens != 1000 || snap[1].CacheReadTokens != 900 || snap[1].OutputTokens != 7 {
		t.Fatalf("serving key usage = %+v", snap[1])
	}
	if snap[1].CacheHitRate == nil || *snap[1].CacheHitRate != 0.9 {
		t.Fatalf("cache hit rate = %v, want 0.9", snap[1].CacheHitRate)
	}
}
