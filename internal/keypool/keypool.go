// Package keypool tracks per-key state for one provider and picks the key
// to use for the next attempt.
package keypool

import (
	"sync"
	"time"
)

type Config struct {
	CooldownBase time.Duration
	CooldownMax  time.Duration
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
	secret        string
	disabled      bool
	cooldownUntil time.Time
	consecLimited int
	successes     uint64
	failures      uint64
}

func New(cfg Config, secrets []string) *Pool {
	p := &Pool{cfg: cfg, now: time.Now}
	for _, s := range secrets {
		p.keys = append(p.keys, &key{secret: s})
		p.masks = append(p.masks, Mask(s))
	}
	return p
}

// Acquire picks a key for the next attempt: round-robin over active keys,
// falling back to the cooling key with the earliest expiry. Keys listed in
// tried (already used within this request) and disabled keys are skipped;
// ok is false when nothing is left.
func (p *Pool) Acquire(tried map[int]bool) (idx int, secret string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.keys)
	now := p.now()
	for i := range n {
		idx := (p.next + i) % n
		k := p.keys[idx]
		if tried[idx] || k.disabled || k.cooldownUntil.After(now) {
			continue
		}
		p.next = (idx + 1) % n
		return idx, k.secret, true
	}
	best := -1
	for i, k := range p.keys {
		if tried[i] || k.disabled {
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
	// cooldown said (it can be reached via Acquire's all-cooling fallback).
	k.cooldownUntil = time.Time{}
}

// ReportRateLimited puts the key into cooldown: for retryAfter if the
// provider supplied the header (hasRetryAfter), otherwise exponentially by
// consecutive 429s. Returns the applied cooldown.
func (p *Pool) ReportRateLimited(idx int, retryAfter time.Duration, hasRetryAfter bool) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.keys[idx]
	k.failures++
	k.consecLimited++
	var d time.Duration
	if hasRetryAfter {
		// "Retry-After: 0" means retry now; a small floor keeps the
		// cooldown bookkeeping meaningful without really benching the key.
		d = max(retryAfter, time.Second)
	} else {
		d = p.cfg.CooldownBase
		for i := 1; i < k.consecLimited && d < p.cfg.CooldownMax; i++ {
			d *= 2
		}
	}
	d = min(d, p.cfg.CooldownMax)
	k.cooldownUntil = p.now().Add(d)
	return d
}

// ReportInvalid disables the key until Reset or process restart.
func (p *Pool) ReportInvalid(idx int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.keys[idx]
	k.failures++
	k.disabled = true
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
		k.disabled = false
		k.cooldownUntil = time.Time{}
		k.consecLimited = 0
	}
}

type KeyStatus struct {
	Key           string     `json:"key"`
	State         string     `json:"state"`
	CooldownUntil *time.Time `json:"cooldown_until,omitempty"`
	Successes     uint64     `json:"successes"`
	Failures      uint64     `json:"failures"`
}

func (p *Pool) Snapshot() []KeyStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	out := make([]KeyStatus, len(p.keys))
	for i, k := range p.keys {
		st := KeyStatus{Key: p.masks[i], State: "active", Successes: k.successes, Failures: k.failures}
		switch {
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

// Mask keeps just enough of a secret to tell keys apart in logs and /status;
// short secrets are hidden entirely so the mask never reveals most of a key.
func Mask(s string) string {
	if len(s) < 16 {
		return "****"
	}
	return s[:4] + "…" + s[len(s)-4:]
}
