package keypool

import (
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
	if d := p.ReportRateLimited(idx, 0, false); d != time.Minute {
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
		if d := p.ReportRateLimited(0, 0, false); d != w {
			t.Fatalf("cooldown = %v, want %v", d, w)
		}
	}
	// Success resets the streak.
	p.ReportSuccess(0)
	if d := p.ReportRateLimited(0, 0, false); d != time.Minute {
		t.Fatalf("cooldown after success = %v, want 1m", d)
	}
}

func TestCooldownCap(t *testing.T) {
	p, _ := testPool("aaaaaaaaaaaaaaaa")
	for range 30 {
		p.ReportRateLimited(0, 0, false)
	}
	if d := p.ReportRateLimited(0, 0, false); d != time.Hour {
		t.Fatalf("cooldown = %v, want cap 1h", d)
	}
	// Retry-After above the cap is clamped too.
	if d := p.ReportRateLimited(0, 24*time.Hour, true); d != time.Hour {
		t.Fatalf("retry-after cooldown = %v, want cap 1h", d)
	}
}

func TestRetryAfterWins(t *testing.T) {
	p, _ := testPool("aaaaaaaaaaaaaaaa")
	if d := p.ReportRateLimited(0, 7*time.Second, true); d != 7*time.Second {
		t.Fatalf("cooldown = %v, want 7s", d)
	}
}

func TestAllCoolingPicksEarliest(t *testing.T) {
	p, _ := testPool("aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb")
	p.ReportRateLimited(0, 10*time.Minute, true)
	p.ReportRateLimited(1, time.Minute, true)
	if idx, _ := mustAcquire(t, p, nil); idx != 1 {
		t.Fatalf("picked %d, want 1 (earliest cooldown)", idx)
	}
}

func TestDisabledAndReset(t *testing.T) {
	p, _ := testPool("aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb")
	p.ReportInvalid(0)
	for range 3 {
		if idx, _ := mustAcquire(t, p, nil); idx == 0 {
			t.Fatal("picked disabled key")
		}
	}
	p.ReportInvalid(1)
	if _, _, ok := p.Acquire(nil); ok {
		t.Fatal("Acquire succeeded with all keys disabled")
	}
	p.Reset()
	if _, _, ok := p.Acquire(nil); !ok {
		t.Fatal("Acquire failed after Reset")
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
	p.ReportRateLimited(0, time.Minute, true)
	p.ReportInvalid(1)
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
	p.ReportRateLimited(0, time.Hour, true)
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

func TestRetryAfterZeroFloor(t *testing.T) {
	p, _ := testPool("aaaaaaaaaaaaaaaa")
	// "Retry-After: 0" is a provider-declared immediate retry, not a missing
	// header — the exponential schedule must not kick in.
	if d := p.ReportRateLimited(0, 0, true); d != time.Second {
		t.Fatalf("cooldown = %v, want 1s floor", d)
	}
}
