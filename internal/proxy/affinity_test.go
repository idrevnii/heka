package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/idrevnii/heka/internal/config"
)

var affinityOn = config.AffinityParams{Enabled: true, Header: "X-Heka-Affinity"}

func hashOf(t *testing.T, body string, headers ...string) uint64 {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	h, ok := affinityOf(r, []byte(body), affinityOn)
	if !ok {
		t.Fatalf("no affinity derived from %s", body)
	}
	return h
}

func noAffinity(t *testing.T, body string, p config.AffinityParams) {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	if h, ok := affinityOf(r, []byte(body), p); ok {
		t.Fatalf("affinity %d derived from %s, want none", h, body)
	}
}

// A conversation grows with every turn; the binding must not.
func TestAffinitySurvivesConversationTurns(t *testing.T) {
	turn1 := `{"model":"claude","system":[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral"}}],
		"tools":[{"name":"bash"}],
		"messages":[{"role":"user","content":"hello","cache_control":{"type":"ephemeral"}}]}`
	// Second turn: history appended, and the client moved its cache
	// breakpoint off the first message onto the latest one.
	turn2 := `{"model":"claude","system":[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral"}}],
		"tools":[{"name":"bash"}],
		"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},
			{"role":"user","content":"and now?","cache_control":{"type":"ephemeral"}}]}`
	if hashOf(t, turn1) != hashOf(t, turn2) {
		t.Fatal("affinity changed between turns of the same conversation")
	}
}

func TestAffinitySeparatesConversations(t *testing.T) {
	base := `{"model":"claude","system":"s","messages":[{"role":"user","content":%q}]}`
	a := hashOf(t, strings.Replace(base, "%q", `"first chat"`, 1))
	b := hashOf(t, strings.Replace(base, "%q", `"second chat"`, 1))
	if a == b {
		t.Fatal("different first messages produced the same affinity")
	}
	// A different model is a different cache namespace upstream.
	c := hashOf(t, `{"model":"other","system":"s","messages":[{"role":"user","content":"first chat"}]}`)
	if a == c {
		t.Fatal("different models produced the same affinity")
	}
	// So is a different system prompt or tool set.
	d := hashOf(t, `{"model":"claude","system":"other","messages":[{"role":"user","content":"first chat"}]}`)
	if a == d {
		t.Fatal("different system prompts produced the same affinity")
	}
}

func TestAffinityIgnoresFieldOrder(t *testing.T) {
	a := hashOf(t, `{"model":"claude","system":"s","messages":[{"role":"user","content":"hi"}]}`)
	b := hashOf(t, `{"messages":[{"content":"hi","role":"user"}],"system":"s","model":"claude"}`)
	if a != b {
		t.Fatal("JSON field order changed the affinity")
	}
}

func TestAffinityHeaderWins(t *testing.T) {
	body := `{"model":"claude","system":"s","messages":[{"role":"user","content":"hi"}]}`
	other := `{"model":"claude","system":"s","messages":[{"role":"user","content":"different"}]}`
	a := hashOf(t, body, "X-Heka-Affinity", "session-7")
	b := hashOf(t, other, "X-Heka-Affinity", "session-7")
	if a != b {
		t.Fatal("explicit affinity header did not override the body")
	}
	if c := hashOf(t, body, "X-Heka-Affinity", "session-8"); a == c {
		t.Fatal("distinct affinity headers collided")
	}
	// An empty header name turns the client-supplied hint off.
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	r.Header.Set("X-Heka-Affinity", "session-7")
	h, ok := affinityOf(r, []byte(body), config.AffinityParams{Enabled: true})
	if !ok || h == a {
		t.Fatalf("header honoured despite being unconfigured: %d ok=%v", h, ok)
	}
}

func TestAffinityPromptCacheKey(t *testing.T) {
	a := hashOf(t, `{"model":"gpt","prompt_cache_key":"sess-1","messages":[{"role":"user","content":"hi"}]}`)
	b := hashOf(t, `{"model":"gpt","prompt_cache_key":"sess-1","messages":[{"role":"user","content":"totally other"}]}`)
	if a != b {
		t.Fatal("prompt_cache_key did not pin the binding")
	}
	if c := hashOf(t, `{"model":"gpt","prompt_cache_key":"sess-2","messages":[{"role":"user","content":"hi"}]}`); a == c {
		t.Fatal("distinct prompt_cache_keys collided")
	}
}

func TestAffinityProviderDialects(t *testing.T) {
	// Gemini: contents + systemInstruction.
	gemini := hashOf(t, `{"systemInstruction":{"parts":[{"text":"s"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if gemini == hashOf(t, `{"systemInstruction":{"parts":[{"text":"s"}]},"contents":[{"role":"user","parts":[{"text":"other"}]}]}`) {
		t.Fatal("gemini contents did not affect the affinity")
	}
	// OpenAI responses: input as a bare string.
	if hashOf(t, `{"model":"gpt","input":"hi"}`) == hashOf(t, `{"model":"gpt","input":"other"}`) {
		t.Fatal("string input did not affect the affinity")
	}
}

func TestNoAffinityFallsBackToRoundRobin(t *testing.T) {
	p := affinityOn
	noAffinity(t, `not json at all`, p)
	noAffinity(t, ``, p)
	// A search API request has no prompt to bind to; the model alone is too
	// coarse to pin every request of one model to a single key.
	noAffinity(t, `{"query":"golang","max_results":5}`, p)
	noAffinity(t, `{"model":"claude"}`, p)
	noAffinity(t, `{"model":"claude","messages":[]}`, p)
	// Disabled outright.
	noAffinity(t, `{"model":"claude","system":"s","messages":[{"role":"user","content":"hi"}]}`,
		config.AffinityParams{Header: "X-Heka-Affinity"})
}

// End to end: two turns of one conversation must reach the same key even
// though a round-robin pool would have alternated.
func TestAffinityKeepsConversationOnOneKey(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.WriteHeader(http.StatusOK)
	})
	h, _ := newHandler(t, up.srv.URL, config.KeyIn{Header: "X-Api-Key"}, key1, key2)
	h.Affinity = affinityOn

	send := func(body string) string {
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req)
		return up.requests[up.count()-1].Header.Get("X-Api-Key")
	}

	conv := `{"model":"claude","system":"s","messages":[{"role":"user","content":"hello"}%s]}`
	first := send(strings.Replace(conv, "%s", "", 1))
	for _, tail := range []string{
		`,{"role":"assistant","content":"hi"}`,
		`,{"role":"assistant","content":"hi"},{"role":"user","content":"more"}`,
	} {
		if got := send(strings.Replace(conv, "%s", tail, 1)); got != first {
			t.Fatalf("turn went out on %q, want %q", got, first)
		}
	}
	// The affinity hint is a gateway concern and must not leak upstream.
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude"}`))
	req.Header.Set("X-Heka-Affinity", "sess-1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if v := up.requests[up.count()-1].Header.Get("X-Heka-Affinity"); v != "" {
		t.Fatalf("affinity header forwarded upstream: %q", v)
	}
}
