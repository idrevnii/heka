// Package keypool tracks per-key state for one provider and picks the key
// to use for the next attempt.
package keypool

import (
	"crypto/sha256"
	"encoding/hex"
	"hash/fnv"
	"sync"
	"time"
)

type Config struct {
	CooldownBase time.Duration
	CooldownMax  time.Duration
	// Blocked contains key fingerprints. Shared across pools and reloads so
	// even a handler finishing on an old pool sees permanent blocks.
	Blocked *sync.Map
}

type Pool struct {
	mu    sync.Mutex
	cfg   Config
	keys  []*key
	masks []string // immutable after New, safe to read without the lock
	next  int
	now   func() time.Time
}

type key struct {
	secret      string
	fingerprint string
	// id identifies the key for rendezvous hashing. It is derived from the
	// secret rather than from the position in the pool, so reordering the
	// keys in the config doesn't reshuffle every affinity binding.
	id            uint64
	disabled      bool
	cooldownUntil time.Time
	consecLimited int
	successes     uint64
	failures      uint64
	usage         Usage
	lastError     *KeyError
}

// ErrorDetail is what the provider said when it rejected a key: the HTTP
// status and as much of the response body as the caller was willing to read.
// A zero value means the failure carried no upstream response.
type ErrorDetail struct {
	Status  int
	Message string
}

// KeyError is the last rejection recorded against a key, kept so the
// dashboard can explain *why* a key is disabled or cooling instead of only
// naming the state. Cleared when the key next succeeds, or on reset.
type KeyError struct {
	At      time.Time `json:"at"`
	Reason  string    `json:"reason"` // "invalid_key" | "rate_limited"
	Status  int       `json:"status,omitempty"`
	Message string    `json:"message,omitempty"`
}

// record replaces the key's last error. The KeyError is never mutated after
// this, so Snapshot can hand the pointer out without copying.
func (k *key) record(reason string, d ErrorDetail, now time.Time) {
	k.lastError = &KeyError{At: now, Reason: reason, Status: d.Status, Message: d.Message}
}

// reset returns the key to the state it had at startup, leaving the counters
// and usage totals (which are history, not state) alone.
func (k *key) reset() {
	k.disabled = false
	k.cooldownUntil = time.Time{}
	k.consecLimited = 0
	k.lastError = nil
}

// Usage is the token accounting reported by a provider for one response,
// normalized across dialects. Input is the whole prompt including whatever
// was served from cache, so CacheRead/Input is the cache hit rate — the
// number that says whether affinity is doing its job.
type Usage struct {
	Input      uint64
	CacheRead  uint64
	CacheWrite uint64
	Output     uint64
}

func (u Usage) empty() bool { return u == Usage{} }

func (u *Usage) add(o Usage) {
	u.Input += o.Input
	u.CacheRead += o.CacheRead
	u.CacheWrite += o.CacheWrite
	u.Output += o.Output
}

func New(cfg Config, secrets []string) *Pool {
	p := &Pool{cfg: cfg, now: time.Now}
	for _, s := range secrets {
		p.keys = append(p.keys, &key{secret: s, id: keyID(s), fingerprint: Fingerprint(s)})
		p.masks = append(p.masks, Mask(s))
	}
	return p
}

// Acquire picks a key for the next attempt: round-robin over active keys,
// falling back to the cooling key with the earliest expiry. Keys listed in
// tried (already used within this request) and disabled keys are skipped;
// ok is false when nothing is left.
func (p *Pool) Acquire(tried map[int]bool) (idx int, secret string, ok bool) {
	return p.acquire(0, false, tried)
}

// AcquireFor picks the key bound to affinity — a hash of the request's
// cacheable prefix — so that consecutive turns of one conversation keep
// landing on the same key and hitting the same upstream prompt cache.
//
// The binding is rendezvous hashing over the keys usable right now: when a
// key drops out (cooldown, disabled, already tried within this request),
// only the requests bound to it move, and every other binding is left
// alone. Round-robin's cursor is not advanced — affinity traffic and
// unbound traffic don't perturb each other.
func (p *Pool) AcquireFor(affinity uint64, tried map[int]bool) (idx int, secret string, ok bool) {
	return p.acquire(affinity, true, tried)
}

func (p *Pool) acquire(affinity uint64, bound bool, tried map[int]bool) (idx int, secret string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.keys)
	now := p.now()
	if bound {
		best, bestScore := -1, uint64(0)
		for i, k := range p.keys {
			if tried[i] || k.disabled || p.IsBlocked(i) || k.cooldownUntil.After(now) {
				continue
			}
			if score := mix(affinity, k.id); best == -1 || score > bestScore {
				best, bestScore = i, score
			}
		}
		if best >= 0 {
			return best, p.keys[best].secret, true
		}
		// Nothing live to bind to: share the all-cooling fallback below.
	} else {
		for i := range n {
			idx := (p.next + i) % n
			k := p.keys[idx]
			if tried[idx] || k.disabled || p.IsBlocked(idx) || k.cooldownUntil.After(now) {
				continue
			}
			p.next = (idx + 1) % n
			return idx, k.secret, true
		}
	}
	best := -1
	for i, k := range p.keys {
		if tried[i] || k.disabled || p.IsBlocked(i) {
			continue
		}
		if best == -1 || k.cooldownUntil.Before(p.keys[best].cooldownUntil) {
			best = i
		}
	}
	if best == -1 {
		return -1, "", false
	}
	return best, p.keys[best].secret, true
}

func (p *Pool) ReportSuccess(idx int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.keys[idx]
	k.successes++
	k.consecLimited = 0
	// A key that just worked is not rate-limited, whatever an earlier
	// cooldown said (it can be reached via Acquire's all-cooling fallback),
	// and whatever it last complained about is no longer its problem.
	k.cooldownUntil = time.Time{}
	k.lastError = nil
}

// ReportUsage adds one response's token accounting to the key's totals.
// Providers report cache hits per key, so this is tracked per key too: a
// pool where one key does all the cache reads is a pool whose affinity is
// working, and a pool where none of them do is one where it isn't.
func (p *Pool) ReportUsage(idx int, u Usage) {
	if u.empty() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys[idx].usage.add(u)
}

// ReportRateLimited puts the key into cooldown: for retryAfter if the
// provider supplied the header (hasRetryAfter), otherwise exponentially by
// consecutive 429s. detail is the upstream's own explanation, kept for the
// dashboard. Returns the applied cooldown.
func (p *Pool) ReportRateLimited(idx int, retryAfter time.Duration, hasRetryAfter bool, detail ErrorDetail) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.keys[idx]
	k.failures++
	k.consecLimited++
	k.record("rate_limited", detail, p.now())
	var d time.Duration
	if hasRetryAfter {
		// "Retry-After: 0" means retry now; a small floor keeps the
		// cooldown bookkeeping meaningful without really benching the key.
		d = max(retryAfter, time.Second)
	} else {
		d = p.cfg.CooldownBase
		for i := 1; i < k.consecLimited && d < p.cfg.CooldownMax; i++ {
			if d > p.cfg.CooldownMax/2 {
				d = p.cfg.CooldownMax
			} else {
				d *= 2
			}
		}
	}
	d = min(d, p.cfg.CooldownMax)
	k.cooldownUntil = p.now().Add(d)
	return d
}

// ReportInvalid disables the key until Reset or process restart. detail is
// the upstream's own explanation, kept for the dashboard — a disabled key is
// the one state an operator can't diagnose from the state name alone.
func (p *Pool) ReportInvalid(idx int, detail ErrorDetail) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.keys[idx]
	k.failures++
	k.disabled = true
	k.record("invalid_key", detail, p.now())
}

// ReportFailure counts a retryable failure (5xx / network) without changing
// the key state.
func (p *Pool) ReportFailure(idx int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys[idx].failures++
}

func (p *Pool) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, k := range p.keys {
		k.reset()
	}
}

// ResetKey clears cooldown/disabled state for a single key, identified by
// its index in Snapshot. Reports false when idx is outside the pool — which
// is what a stale dashboard tab sees after a reload shrank the key list.
func (p *Pool) ResetKey(idx int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx < 0 || idx >= len(p.keys) {
		return false
	}
	p.keys[idx].reset()
	return true
}

type KeyStatus struct {
	// Index is the key's position in the pool — the handle the dashboard
	// passes back to reset this one key.
	Index         int        `json:"index"`
	ID            string     `json:"id"`
	Key           string     `json:"key"`
	State         string     `json:"state"`
	CooldownUntil *time.Time `json:"cooldown_until,omitempty"`
	Successes     uint64     `json:"successes"`
	Failures      uint64     `json:"failures"`

	InputTokens      uint64 `json:"input_tokens"`
	CacheReadTokens  uint64 `json:"cache_read_tokens"`
	CacheWriteTokens uint64 `json:"cache_write_tokens"`
	OutputTokens     uint64 `json:"output_tokens"`
	// CacheHitRate is CacheReadTokens/InputTokens, or null when the provider
	// reported no input tokens at all (search APIs, or a pool that hasn't
	// served a request yet).
	CacheHitRate *float64 `json:"cache_hit_rate"`
	// LastError is why the key was last rejected, present until it succeeds
	// again or is reset.
	LastError *KeyError `json:"last_error,omitempty"`
}

func (p *Pool) Snapshot() []KeyStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	out := make([]KeyStatus, len(p.keys))
	for i, k := range p.keys {
		st := KeyStatus{
			Index: i, ID: k.fingerprint, Key: p.masks[i], State: "active", Successes: k.successes, Failures: k.failures,
			LastError:        k.lastError,
			InputTokens:      k.usage.Input,
			CacheReadTokens:  k.usage.CacheRead,
			CacheWriteTokens: k.usage.CacheWrite,
			OutputTokens:     k.usage.Output,
		}
		if k.usage.Input > 0 {
			rate := float64(k.usage.CacheRead) / float64(k.usage.Input)
			st.CacheHitRate = &rate
		}
		switch {
		case p.IsBlocked(i):
			st.State = "blocked"
		case k.disabled:
			st.State = "disabled"
		case k.cooldownUntil.After(now):
			st.State = "cooldown"
			t := k.cooldownUntil
			st.CooldownUntil = &t
		}
		out[i] = st
	}
	return out
}

func (p *Pool) MaskedKey(idx int) string {
	if idx < 0 || idx >= len(p.masks) {
		return ""
	}
	return p.masks[idx]
}

// IsBlocked is independent of resettable runtime state. Keys are immutable
// after New; only the shared concurrent map changes.
func (p *Pool) IsBlocked(idx int) bool {
	if p.cfg.Blocked == nil || idx < 0 || idx >= len(p.keys) {
		return false
	}
	_, blocked := p.cfg.Blocked.Load(p.keys[idx].fingerprint)
	return blocked
}

// Fingerprint identifies a secret without storing it in the blocklist.
func Fingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func keyID(secret string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(secret))
	return h.Sum64()
}

// mix scores one (affinity, key) pair for rendezvous hashing. It has to
// scatter well in both arguments: a weak mix would make neighbouring
// affinity hashes prefer the same key and undo the balancing.
func mix(affinity, id uint64) uint64 {
	x := affinity ^ (id + 0x9e3779b97f4a7c15 + (affinity << 6) + (affinity >> 2))
	// splitmix64 finalizer
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// Mask keeps just enough of a secret to tell keys apart in logs and /status;
// short secrets are hidden entirely so the mask never reveals most of a key.
func Mask(s string) string {
	if len(s) < 16 {
		return "****"
	}
	return s[:4] + "…" + s[len(s)-4:]
}
