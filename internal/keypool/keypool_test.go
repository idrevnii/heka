package keypool

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func testPool(secrets ...string) (*Pool, *time.Time) {
	p := New(Config{CooldownBase: time.Minute, CooldownMax: time.Hour}, secrets)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	return p, &now
}

func mustAcquire(t *testing.T, p *Pool, tried map[int]bool) (int, string) {
	t.Helper()
	idx, secret, ok := p.Acquire(tried)
	if !ok {
		t.Fatal("Acquire: no key available")
	}
	return idx, secret
}

func TestRoundRobin(t *testing.T) {
	p, _ := testPool("aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "cccccccccccccccc")
	var got []int
	for range 6 {
		idx, _ := mustAcquire(t, p, nil)
		got = append(got, idx)
	}
	want := []int{0, 1, 2, 0, 1, 2}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("round-robin order = %v, want %v", got, want)
		}
	}
}

func TestCooldownAndExpiry(t *testing.T) {
	p, now := testPool("aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb")
	idx, _ := mustAcquire(t, p, nil)
	if d := p.ReportRateLimited(idx, 0, false, ErrorDetail{}); d != time.Minute {
		t.Fatalf("first cooldown = %v, want 1m", d)
	}
	// While key 0 cools down, only key 1 is picked.
	for range 3 {
		if i, _ := mustAcquire(t, p, nil); i != 0 {
			// key 0 skipped as expected
			if i != 1 {
				t.Fatalf("picked %d, want 1", i)
			}
		} else {
			t.Fatal("picked cooling key 0")
		}
	}
	*now = now.Add(61 * time.Second)
	seen := map[int]bool{}
	for range 4 {
		i, _ := mustAcquire(t, p, nil)
		seen[i] = true
	}
	if !seen[0] {
		t.Fatal("key 0 not back in rotation after cooldown expiry")
	}
}

func TestExponentialCooldown(t *testing.T) {
	p, _ := testPool("aaaaaaaaaaaaaaaa")
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute}
	for _, w := range want {
		if d := p.ReportRateLimited(0, 0, false, ErrorDetail{}); d != w {
			t.Fatalf("cooldown = %v, want %v", d, w)
		}
	}
	// Success resets the streak.
	p.ReportSuccess(0)
	if d := p.ReportRateLimited(0, 0, false, ErrorDetail{}); d != time.Minute {
		t.Fatalf("cooldown after success = %v, want 1m", d)
	}
}

func TestCooldownCap(t *testing.T) {
	p, _ := testPool("aaaaaaaaaaaaaaaa")
	for range 30 {
		p.ReportRateLimited(0, 0, false, ErrorDetail{})
	}
	if d := p.ReportRateLimited(0, 0, false, ErrorDetail{}); d != time.Hour {
		t.Fatalf("cooldown = %v, want cap 1h", d)
	}
	// Retry-After above the cap is clamped too.
	if d := p.ReportRateLimited(0, 24*time.Hour, true, ErrorDetail{}); d != time.Hour {
		t.Fatalf("retry-after cooldown = %v, want cap 1h", d)
	}
}

func TestExponentialCooldownCannotOverflow(t *testing.T) {
	const ceiling = time.Duration(1<<63 - 1)
	p := New(Config{CooldownBase: ceiling/2 + 1, CooldownMax: ceiling}, []string{"key"})
	p.ReportRateLimited(0, 0, false, ErrorDetail{})
	if got := p.ReportRateLimited(0, 0, false, ErrorDetail{}); got != ceiling {
		t.Fatalf("cooldown overflowed: %v", got)
	}
}

func TestRetryAfterWins(t *testing.T) {
	p, _ := testPool("aaaaaaaaaaaaaaaa")
	if d := p.ReportRateLimited(0, 7*time.Second, true, ErrorDetail{}); d != 7*time.Second {
		t.Fatalf("cooldown = %v, want 7s", d)
	}
}

func TestAllCoolingPicksEarliest(t *testing.T) {
	p, _ := testPool("aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb")
	p.ReportRateLimited(0, 10*time.Minute, true, ErrorDetail{})
	p.ReportRateLimited(1, time.Minute, true, ErrorDetail{})
	if idx, _ := mustAcquire(t, p, nil); idx != 1 {
		t.Fatalf("picked %d, want 1 (earliest cooldown)", idx)
	}
}

func TestDisabledAndReset(t *testing.T) {
	p, _ := testPool("aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb")
	p.ReportInvalid(0, ErrorDetail{})
	for range 3 {
		if idx, _ := mustAcquire(t, p, nil); idx == 0 {
			t.Fatal("picked disabled key")
		}
	}
	p.ReportInvalid(1, ErrorDetail{})
	if _, _, ok := p.Acquire(nil); ok {
		t.Fatal("Acquire succeeded with all keys disabled")
	}
	p.Reset()
	if _, _, ok := p.Acquire(nil); !ok {
		t.Fatal("Acquire failed after Reset")
	}
}

func TestPermanentBlocksOverrideAllSelectionAndResetPaths(t *testing.T) {
	var blocked sync.Map
	p := New(Config{CooldownBase: time.Minute, CooldownMax: time.Hour, Blocked: &blocked}, []string{"a", "b", "a"})
	blocked.Store(Fingerprint("a"), true)
	p.Reset()
	p.ResetKey(0)
	p.ReportSuccess(0)
	p.ReportInvalid(0, ErrorDetail{})
	p.ReportRateLimited(0, time.Second, true, ErrorDetail{})
	for _, cooling := range []bool{false, true} {
		if cooling {
			p.ReportRateLimited(1, time.Hour, true, ErrorDetail{})
		}
		for range 10 {
			if idx, _, ok := p.Acquire(nil); !ok || idx != 1 {
				t.Fatalf("round robin (cooling=%v): idx=%d ok=%v", cooling, idx, ok)
			}
			if idx, _, ok := p.AcquireFor(42, nil); !ok || idx != 1 {
				t.Fatalf("affinity (cooling=%v): idx=%d ok=%v", cooling, idx, ok)
			}
		}
	}
	for _, idx := range []int{0, 2} {
		if p.Snapshot()[idx].State != "blocked" {
			t.Fatal("duplicate secret escaped the block")
		}
	}
	blocked.Store(Fingerprint("b"), true)
	if _, _, ok := p.Acquire(nil); ok {
		t.Fatal("round robin selected a permanently blocked key")
	}
	if _, _, ok := p.AcquireFor(42, nil); ok {
		t.Fatal("affinity selected a permanently blocked key")
	}
}

func TestResetKeyOnlyTouchesThatKey(t *testing.T) {
	p, _ := testPool("aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb")
	p.ReportInvalid(0, ErrorDetail{Status: 401, Message: "revoked"})
	p.ReportInvalid(1, ErrorDetail{Status: 401, Message: "revoked"})

	if !p.ResetKey(0) {
		t.Fatal("ResetKey(0) = false")
	}
	snap := p.Snapshot()
	if snap[0].State != "active" || snap[0].LastError != nil {
		t.Fatalf("key 0 after reset = %+v, want active with no error", snap[0])
	}
	if snap[1].State != "disabled" || snap[1].LastError == nil {
		t.Fatalf("key 1 = %+v, want still disabled", snap[1])
	}
	// Counters are history, not state: a reset must not erase them.
	if snap[0].Failures != 1 {
		t.Fatalf("key 0 failures = %d, want 1", snap[0].Failures)
	}
	if p.ResetKey(2) || p.ResetKey(-1) {
		t.Fatal("ResetKey out of range = true, want false")
	}
}

func TestLastErrorRecordedAndCleared(t *testing.T) {
	p, now := testPool("aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb")
	p.ReportInvalid(0, ErrorDetail{Status: 401, Message: `{"error":"invalid x-api-key"}`})
	p.ReportRateLimited(1, time.Minute, true, ErrorDetail{Status: 429, Message: "slow down"})

	snap := p.Snapshot()
	if e := snap[0].LastError; e == nil || e.Reason != "invalid_key" || e.Status != 401 ||
		!strings.Contains(e.Message, "invalid x-api-key") || !e.At.Equal(*now) {
		t.Fatalf("key 0 last error = %+v", snap[0].LastError)
	}
	if e := snap[1].LastError; e == nil || e.Reason != "rate_limited" || e.Status != 429 {
		t.Fatalf("key 1 last error = %+v", snap[1].LastError)
	}
	// Snapshot indices are the handle the dashboard resets by.
	if snap[0].Index != 0 || snap[1].Index != 1 {
		t.Fatalf("indices = %d,%d", snap[0].Index, snap[1].Index)
	}

	p.ReportSuccess(0)
	if e := p.Snapshot()[0].LastError; e != nil {
		t.Fatalf("last error after success = %+v, want nil", e)
	}
}

func TestTriedExcluded(t *testing.T) {
	p, _ := testPool("aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb")
	idx1, _ := mustAcquire(t, p, nil)
	idx2, _, ok := p.Acquire(map[int]bool{idx1: true})
	if !ok || idx2 == idx1 {
		t.Fatalf("second Acquire = %d ok=%v, want the other key", idx2, ok)
	}
	if _, _, ok := p.Acquire(map[int]bool{0: true, 1: true}); ok {
		t.Fatal("Acquire succeeded with all keys tried")
	}
}

func TestSnapshotAndMask(t *testing.T) {
	p, _ := testPool("sk-ant-api03-verysecret0001", "sk-ant-api03-verysecret0002")
	p.ReportRateLimited(0, time.Minute, true, ErrorDetail{})
	p.ReportInvalid(1, ErrorDetail{})
	snap := p.Snapshot()
	if snap[0].State != "cooldown" || snap[0].CooldownUntil == nil {
		t.Fatalf("key 0 state = %+v, want cooldown", snap[0])
	}
	if snap[1].State != "disabled" {
		t.Fatalf("key 1 state = %q, want disabled", snap[1].State)
	}
	if snap[0].Key != "sk-a…0001" {
		t.Fatalf("masked key = %q", snap[0].Key)
	}
	// Anything shorter than 16 chars would leak most of itself — fully mask.
	for _, s := range []string{"short", "abcdef123456789"} {
		if Mask(s) != "****" {
			t.Fatalf("Mask(%q) = %q, want ****", s, Mask(s))
		}
	}
}

func TestSuccessClearsCooldown(t *testing.T) {
	p, _ := testPool("aaaaaaaaaaaaaaaa")
	p.ReportRateLimited(0, time.Hour, true, ErrorDetail{})
	if p.Snapshot()[0].State != "cooldown" {
		t.Fatal("expected cooldown before success")
	}
	// The all-cooling fallback hands the key out; if it then works, it must
	// return to normal rotation immediately.
	p.ReportSuccess(0)
	if st := p.Snapshot()[0].State; st != "active" {
		t.Fatalf("state after success = %q, want active", st)
	}
}

func TestAffinityIsStable(t *testing.T) {
	p, _ := testPool("aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "cccccccccccccccc")
	first, _, ok := p.AcquireFor(42, nil)
	if !ok {
		t.Fatal("AcquireFor: no key available")
	}
	for range 10 {
		if idx, _, _ := p.AcquireFor(42, nil); idx != first {
			t.Fatalf("bound key moved: %d, want %d", idx, first)
		}
	}
	// Unbound traffic in between must not drag the binding along.
	mustAcquire(t, p, nil)
	mustAcquire(t, p, nil)
	if idx, _, _ := p.AcquireFor(42, nil); idx != first {
		t.Fatalf("bound key moved after round-robin traffic: %d, want %d", idx, first)
	}
}

func TestAffinitySpreadsAcrossKeys(t *testing.T) {
	p, _ := testPool("aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "cccccccccccccccc")
	counts := map[int]int{}
	const n = 600
	for i := range n {
		idx, _, _ := p.AcquireFor(uint64(i)*0x9e3779b1, nil)
		counts[idx]++
	}
	if len(counts) != 3 {
		t.Fatalf("affinity used %d of 3 keys: %v", len(counts), counts)
	}
	// Rendezvous hashing is not exact, but a third of the load ±50% is a
	// generous band; anything outside it means the mix is degenerate.
	for idx, c := range counts {
		if c < n/3/2 || c > n/3*3/2 {
			t.Fatalf("key %d got %d of %d requests: %v", idx, c, n, counts)
		}
	}
}

func TestAffinityMinimalDisruption(t *testing.T) {
	p, now := testPool("aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "cccccccccccccccc")
	before := map[uint64]int{}
	for i := range uint64(300) {
		idx, _, _ := p.AcquireFor(i, nil)
		before[i] = idx
	}
	// Bench one key: only the sessions bound to it may move.
	p.ReportRateLimited(0, 10*time.Minute, true, ErrorDetail{})
	moved, stayed := 0, 0
	for i, was := range before {
		idx, _, _ := p.AcquireFor(i, nil)
		if idx == 0 {
			t.Fatalf("affinity %d picked cooling key 0", i)
		}
		if idx == was {
			stayed++
		} else {
			if was != 0 {
				t.Fatalf("affinity %d moved from live key %d to %d", i, was, idx)
			}
			moved++
		}
	}
	if moved == 0 || stayed == 0 {
		t.Fatalf("moved %d, stayed %d — expected a partial remap", moved, stayed)
	}
	// ...and everything comes back once the key is healthy again.
	*now = now.Add(11 * time.Minute)
	for i, was := range before {
		if idx, _, _ := p.AcquireFor(i, nil); idx != was {
			t.Fatalf("affinity %d = %d after cooldown expiry, want %d", i, idx, was)
		}
	}
}

func TestAffinityIgnoresKeyOrder(t *testing.T) {
	a, _ := testPool("aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "cccccccccccccccc")
	b, _ := testPool("cccccccccccccccc", "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb")
	for i := range uint64(50) {
		_, s1, _ := a.AcquireFor(i, nil)
		_, s2, _ := b.AcquireFor(i, nil)
		if s1 != s2 {
			t.Fatalf("affinity %d picked different secrets after reorder", i)
		}
	}
}

func TestAffinityRetryIsDeterministic(t *testing.T) {
	p, _ := testPool("aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "cccccccccccccccc")
	first, _, _ := p.AcquireFor(7, nil)
	second, _, ok := p.AcquireFor(7, map[int]bool{first: true})
	if !ok || second == first {
		t.Fatalf("retry key = %d ok=%v, want another key", second, ok)
	}
	for range 5 {
		if idx, _, _ := p.AcquireFor(7, map[int]bool{first: true}); idx != second {
			t.Fatalf("retry key moved: %d, want %d", idx, second)
		}
	}
	if _, _, ok := p.AcquireFor(7, map[int]bool{0: true, 1: true, 2: true}); ok {
		t.Fatal("AcquireFor succeeded with all keys tried")
	}
}

func TestAffinityAllCoolingFallback(t *testing.T) {
	p, _ := testPool("aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb")
	p.ReportRateLimited(0, 10*time.Minute, true, ErrorDetail{})
	p.ReportRateLimited(1, time.Minute, true, ErrorDetail{})
	if idx, _, _ := p.AcquireFor(1, nil); idx != 1 {
		t.Fatalf("picked %d, want 1 (earliest cooldown)", idx)
	}
	p.ReportInvalid(0, ErrorDetail{})
	p.ReportInvalid(1, ErrorDetail{})
	if _, _, ok := p.AcquireFor(1, nil); ok {
		t.Fatal("AcquireFor succeeded with all keys disabled")
	}
}

func TestRetryAfterZeroFloor(t *testing.T) {
	p, _ := testPool("aaaaaaaaaaaaaaaa")
	// "Retry-After: 0" is a provider-declared immediate retry, not a missing
	// header — the exponential schedule must not kick in.
	if d := p.ReportRateLimited(0, 0, true, ErrorDetail{}); d != time.Second {
		t.Fatalf("cooldown = %v, want 1s floor", d)
	}
}
