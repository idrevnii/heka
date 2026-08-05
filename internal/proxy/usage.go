package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/idrevnii/heka/internal/keypool"
)

// Limits for the usage tap. Both bound memory on a body heka is only
// streaming through: a single JSON payload (or SSE event) larger than
// maxUsageChunk is not worth holding, and gzipped bodies are buffered whole
// because they can only be inflated from the start.
const (
	maxUsageChunk = 1 << 20
	maxUsageGzip  = 512 << 10
)

// usageScanner watches the response bytes on their way to the client and
// picks the provider's token accounting out of them. It never buffers the
// whole response: the body is split on newlines and only the segments that
// mention usage are parsed, which for SSE means the handful of events that
// carry it rather than every token delta.
//
// Numbers are merged by taking the maximum per field, because every dialect
// reports cumulative totals: Anthropic sends the input side (including
// cache reads) in message_start and the final output count in message_delta,
// OpenAI sends one usage object in the last chunk, Gemini repeats a growing
// usageMetadata in every chunk.
type usageScanner struct {
	total   keypool.Usage
	line    []byte
	dropped bool // current segment exceeded maxUsageChunk

	gz    *bytes.Buffer // set when the body is gzipped: inflate at close
	gzOff bool          // gzip buffer overflowed; stop collecting
}

// newUsageScanner returns a scanner for resp, or nil when its body can't be
// read on the fly — a content encoding heka can't inflate. A nil scanner
// discards everything written to it, so callers need no nil checks.
func newUsageScanner(resp *http.Response) *usageScanner {
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "", "identity":
		return &usageScanner{}
	case "gzip":
		return &usageScanner{gz: &bytes.Buffer{}}
	default:
		return nil
	}
}

func (u *usageScanner) Write(p []byte) {
	if u == nil || len(p) == 0 {
		return
	}
	if u.gz != nil {
		if !u.gzOff {
			if u.gz.Len()+len(p) > maxUsageGzip {
				u.gzOff = true
			} else {
				u.gz.Write(p)
			}
		}
		return
	}
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			u.appendLine(p)
			return
		}
		// The newline is kept: outside SSE a body may be one JSON document
		// spread over many lines, and it has to stay parsable.
		u.appendLine(p[:i+1])
		u.endLine()
		p = p[i+1:]
	}
}

// endLine decides what the buffer means at a line boundary. An SSE data
// line is a complete payload, so it gets parsed and consumed; SSE framing
// lines carry nothing and are dropped; anything else keeps accumulating,
// because a plain response body is one document however it's wrapped.
func (u *usageScanner) endLine() {
	trimmed := bytes.TrimLeft(u.line, " \t\r\n")
	switch {
	case len(trimmed) == 0 || isSSEField(trimmed):
		u.line, u.dropped = u.line[:0], false
	case bytes.HasPrefix(trimmed, []byte("data:")):
		u.scanLine()
	}
}

// isSSEField reports whether a line is event-stream framing rather than a
// payload: a comment, or any field other than data.
func isSSEField(line []byte) bool {
	for _, p := range [][]byte{[]byte(":"), []byte("event:"), []byte("id:"), []byte("retry:")} {
		if bytes.HasPrefix(line, p) {
			return true
		}
	}
	return false
}

func (u *usageScanner) appendLine(p []byte) {
	if u.dropped || len(u.line)+len(p) > maxUsageChunk {
		u.dropped = true
		u.line = u.line[:0]
		return
	}
	u.line = append(u.line, p...)
}

// scanLine consumes the pending segment: one SSE event payload, or — for a
// plain JSON response, which has no newlines — the whole body.
func (u *usageScanner) scanLine() {
	line, dropped := u.line, u.dropped
	u.line, u.dropped = u.line[:0], false
	if dropped {
		return
	}
	line = bytes.TrimSpace(line)
	if after, found := bytes.CutPrefix(line, []byte("data:")); found {
		line = bytes.TrimSpace(after)
	}
	// Cheap gate: most SSE events are token deltas with no accounting in
	// them, and parsing every one of them would cost more than the proxying.
	if len(line) == 0 || line[0] != '{' ||
		(!bytes.Contains(line, []byte(`"usage"`)) && !bytes.Contains(line, []byte(`"usageMetadata"`))) {
		return
	}
	var env usageEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return
	}
	u.total = maxUsage(u.total, env.usage())
}

// maxUsage merges two reports field by field. Every dialect reports running
// totals, so the largest value seen for a field is the final one — and this
// stays correct whichever order the events arrive in.
func maxUsage(a, b keypool.Usage) keypool.Usage {
	return keypool.Usage{
		Input:      max(a.Input, b.Input),
		CacheRead:  max(a.CacheRead, b.CacheRead),
		CacheWrite: max(a.CacheWrite, b.CacheWrite),
		Output:     max(a.Output, b.Output),
	}
}

// close flushes the pending segment (or inflates a gzipped body) and returns
// what was found. The zero Usage means nothing usable was reported.
func (u *usageScanner) close() keypool.Usage {
	if u == nil {
		return keypool.Usage{}
	}
	if u.gz != nil {
		buf := u.gz
		u.gz = nil
		// A truncated gzip stream still inflates up to the cut; the read
		// error at the end is expected and the prefix is what we want.
		if zr, err := gzip.NewReader(bytes.NewReader(buf.Bytes())); err == nil {
			plain, _ := io.ReadAll(io.LimitReader(zr, maxUsageChunk*4))
			u.Write(plain)
		}
	}
	u.scanLine()
	return u.total
}

// usageEnvelope covers where each provider dialect puts its accounting:
// top-level for OpenAI chat completions, nested under message for
// Anthropic's message_start, under response for OpenAI's Responses API, and
// usageMetadata for Gemini.
type usageEnvelope struct {
	Usage         *tokenUsage `json:"usage"`
	Message       *hasUsage   `json:"message"`
	Response      *hasUsage   `json:"response"`
	UsageMetadata *geminiMeta `json:"usageMetadata"`
}

type hasUsage struct {
	Usage *tokenUsage `json:"usage"`
}

type tokenUsage struct {
	// Anthropic. InputTokens excludes both cache fields.
	InputTokens              uint64 `json:"input_tokens"`
	OutputTokens             uint64 `json:"output_tokens"`
	CacheReadInputTokens     uint64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens uint64 `json:"cache_creation_input_tokens"`
	// OpenAI. PromptTokens already includes the cached ones.
	PromptTokens        uint64 `json:"prompt_tokens"`
	CompletionTokens    uint64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens uint64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	// OpenAI Responses API, which reuses input_tokens/output_tokens above
	// but — unlike Anthropic — counts cached tokens inside input_tokens.
	InputTokensDetails *struct {
		CachedTokens uint64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

type geminiMeta struct {
	PromptTokenCount        uint64 `json:"promptTokenCount"`
	CandidatesTokenCount    uint64 `json:"candidatesTokenCount"`
	CachedContentTokenCount uint64 `json:"cachedContentTokenCount"`
}

func (e usageEnvelope) usage() keypool.Usage {
	var u keypool.Usage
	if m := e.UsageMetadata; m != nil {
		u.Input = m.PromptTokenCount
		u.CacheRead = m.CachedContentTokenCount
		u.Output = m.CandidatesTokenCount
	}
	t := e.Usage
	if t == nil && e.Message != nil {
		t = e.Message.Usage
	}
	if t == nil && e.Response != nil {
		t = e.Response.Usage
	}
	if t == nil {
		return u
	}
	switch {
	case t.PromptTokens > 0 || t.PromptTokensDetails != nil:
		// OpenAI chat completions: prompt_tokens is the whole prompt,
		// cached tokens included.
		u.Input = t.PromptTokens
		u.Output = t.CompletionTokens
		if d := t.PromptTokensDetails; d != nil {
			u.CacheRead = d.CachedTokens
		}
	case t.InputTokensDetails != nil:
		// OpenAI Responses: same field names as Anthropic, opposite
		// convention — input_tokens already contains the cached ones.
		u.Input = t.InputTokens
		u.CacheRead = t.InputTokensDetails.CachedTokens
		u.Output = t.OutputTokens
	default:
		// Anthropic: input_tokens counts only what was neither read from
		// nor written to the cache, so both have to be added back to get
		// the size of the actual prompt.
		u.Input = t.InputTokens + t.CacheReadInputTokens + t.CacheCreationInputTokens
		u.CacheRead = t.CacheReadInputTokens
		u.CacheWrite = t.CacheCreationInputTokens
		u.Output = t.OutputTokens
	}
	return u
}
