package proxy

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"slices"

	"github.com/idrevnii/heka/internal/config"
)

// affinityOf derives the value that binds a request to one key: a hash of
// the part of the payload that forms the upstream prompt-cache prefix.
//
// Providers cache prompt prefixes per key (per organization), so a request
// only reads a warm cache if it goes out on the key that wrote it. What has
// to be hashed is therefore whatever stays byte-stable across the turns of a
// conversation while still telling two conversations apart: the model, the
// system prompt, the tool definitions and the *first* message. Later
// messages are deliberately ignored — they grow every turn, and hashing
// them would re-bind the conversation on each request, which is exactly the
// behaviour this replaces.
//
// ok is false when nothing usable is found (a non-JSON body, a payload with
// no prompt in it, a body too large to have been buffered whole); the caller
// then falls back to plain round-robin.
func affinityOf(r *http.Request, body []byte, p config.AffinityParams) (hash uint64, ok bool) {
	if !p.Enabled {
		return 0, false
	}
	if p.Header != "" {
		if v := r.Header.Get(p.Header); v != "" {
			h := fnv.New64a()
			io.WriteString(h, v)
			return h.Sum64(), true
		}
	}
	if len(body) == 0 {
		return 0, false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return 0, false
	}

	h := fnv.New64a()
	prefix := false
	write := func(field string) {
		raw, present := top[field]
		if !present {
			return
		}
		io.WriteString(h, field)
		writeCanonical(h, raw)
		prefix = true
	}
	writeFirst := func(field string) {
		first, present := firstElem(top[field])
		if !present {
			return
		}
		io.WriteString(h, field)
		writeCanonical(h, first)
		prefix = true
	}

	// The model is part of the cache identity everywhere, but on its own it
	// says nothing about the session, so it never sets prefix by itself.
	if raw, present := top["model"]; present {
		io.WriteString(h, "model")
		writeCanonical(h, raw)
	}
	// An explicit cache key from the client wins over anything inferred:
	// OpenAI's prompt_cache_key is this exact signal, sent by the client
	// that actually knows where its cache boundaries are.
	var cacheKey string
	if err := json.Unmarshal(top["prompt_cache_key"], &cacheKey); err == nil && cacheKey != "" {
		write("prompt_cache_key")
		return h.Sum64(), true
	}
	write("system")             // Anthropic, OpenAI chat
	write("system_instruction") // Gemini
	write("systemInstruction")  // Gemini, camelCase dialect
	write("tools")
	writeFirst("messages") // Anthropic, OpenAI chat
	writeFirst("input")    // OpenAI responses
	writeFirst("contents") // Gemini

	if !prefix {
		return 0, false
	}
	return h.Sum64(), true
}

// firstElem returns the first element of a JSON array, or the value itself
// when the field is not an array (OpenAI's `input` also accepts a bare
// string). An empty array yields no value at all.
func firstElem(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return raw, true
	}
	if len(arr) == 0 {
		return nil, false
	}
	return arr[0], true
}

// writeCanonical feeds raw to h in a form that ignores object key order and
// drops cache_control markers. Those markers matter: clients move their
// cache breakpoints from turn to turn, so a marker landing on the first
// message would otherwise change its hash and unpin the conversation.
func writeCanonical(h io.Writer, raw json.RawMessage) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		h.Write(raw)
		return
	}
	writeValue(h, v)
}

func writeValue(h io.Writer, v any) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			if k == "cache_control" {
				continue
			}
			keys = append(keys, k)
		}
		slices.Sort(keys)
		io.WriteString(h, "{")
		for _, k := range keys {
			io.WriteString(h, k)
			io.WriteString(h, ":")
			writeValue(h, t[k])
			io.WriteString(h, ",")
		}
		io.WriteString(h, "}")
	case []any:
		io.WriteString(h, "[")
		for _, e := range t {
			writeValue(h, e)
			io.WriteString(h, ",")
		}
		io.WriteString(h, "]")
	default:
		fmt.Fprintf(h, "%v", t)
	}
}
