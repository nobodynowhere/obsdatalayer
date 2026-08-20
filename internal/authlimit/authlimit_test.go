package authlimit_test

import (
	"testing"
	"time"

	"obsdatalayer/internal/authlimit"
)

func enabled() authlimit.Config {
	return authlimit.Config{
		Enabled:          true,
		FailureThreshold: 3,
		FailureWindow:    time.Minute,
		BlockDuration:    time.Minute,
		MaxBlockDuration: 10 * time.Minute,
	}
}

// failN drives a source to the threshold.
func failN(l *authlimit.Limiter, source string, n int) {
	for i := 0; i < n; i++ {
		l.RecordFailure(source)
	}
}

func TestAllowsUntilThreshold(t *testing.T) {
	l := authlimit.NewLimiter(enabled())

	// Two failures is below the threshold of three.
	failN(l, "10.0.0.1", 2)
	if ok, _ := l.Allow("10.0.0.1"); !ok {
		t.Fatal("expected a source below the threshold to be allowed")
	}

	l.RecordFailure("10.0.0.1")
	ok, retryAfter := l.Allow("10.0.0.1")
	if ok {
		t.Fatal("expected the source to be blocked at the threshold")
	}
	if retryAfter <= 0 {
		t.Errorf("expected a positive Retry-After, got %v", retryAfter)
	}
}

// A block on one source must not touch another, or one noisy client would take
// down every other caller sharing the gateway.
func TestBlockIsPerSource(t *testing.T) {
	l := authlimit.NewLimiter(enabled())
	failN(l, "10.0.0.1", 3)

	if ok, _ := l.Allow("10.0.0.1"); ok {
		t.Fatal("expected the failing source to be blocked")
	}
	if ok, _ := l.Allow("10.0.0.2"); !ok {
		t.Fatal("expected an unrelated source to be unaffected")
	}
}

// Proving you hold a valid credential clears the slate: the throttle exists to
// stop guessing, not to punish a client that mistyped a password once.
func TestSuccessClearsFailures(t *testing.T) {
	l := authlimit.NewLimiter(enabled())
	failN(l, "10.0.0.1", 2)
	l.RecordSuccess("10.0.0.1")

	// Back to a clean slate: two more failures must not block.
	failN(l, "10.0.0.1", 2)
	if ok, _ := l.Allow("10.0.0.1"); !ok {
		t.Fatal("expected the counter to have been reset by the success")
	}
}

func TestBlockExpires(t *testing.T) {
	cfg := enabled()
	l := authlimit.NewLimiter(cfg)
	now := time.Now()
	authlimit.SetClock(l, func() time.Time { return now })

	failN(l, "10.0.0.1", 3)
	if ok, _ := l.Allow("10.0.0.1"); ok {
		t.Fatal("expected an immediate block")
	}

	now = now.Add(cfg.BlockDuration + time.Second)
	if ok, _ := l.Allow("10.0.0.1"); !ok {
		t.Fatal("expected the block to lapse")
	}
}

// Each successive block doubles, so a client that comes straight back after
// serving one waits longer the next time.
func TestBackoffDoubles(t *testing.T) {
	cfg := enabled()
	l := authlimit.NewLimiter(cfg)
	now := time.Now()
	authlimit.SetClock(l, func() time.Time { return now })

	var waits []time.Duration
	for round := 0; round < 3; round++ {
		failN(l, "10.0.0.1", cfg.FailureThreshold)
		_, wait := l.Allow("10.0.0.1")
		waits = append(waits, wait)
		// Serve out the block, then offend again.
		now = now.Add(wait + time.Second)
	}

	for i := 1; i < len(waits); i++ {
		if waits[i] <= waits[i-1] {
			t.Errorf("expected block %d (%v) to exceed block %d (%v)",
				i, waits[i], i-1, waits[i-1])
		}
	}
}

func TestBackoffIsCapped(t *testing.T) {
	cfg := enabled()
	cfg.BlockDuration = time.Minute
	cfg.MaxBlockDuration = 4 * time.Minute
	l := authlimit.NewLimiter(cfg)
	now := time.Now()
	authlimit.SetClock(l, func() time.Time { return now })

	var wait time.Duration
	for round := 0; round < 8; round++ {
		failN(l, "10.0.0.1", cfg.FailureThreshold)
		_, wait = l.Allow("10.0.0.1")
		now = now.Add(wait + time.Second)
	}

	// Retry-After is rounded up to whole seconds, so allow one second of slack.
	if wait > cfg.MaxBlockDuration+time.Second {
		t.Errorf("backoff exceeded the cap: got %v, cap %v", wait, cfg.MaxBlockDuration)
	}
}

// Failures older than the window should not combine with recent ones, or a
// client failing once an hour would eventually be blocked for it.
func TestFailuresDecayOutsideWindow(t *testing.T) {
	cfg := enabled()
	l := authlimit.NewLimiter(cfg)
	now := time.Now()
	authlimit.SetClock(l, func() time.Time { return now })

	l.RecordFailure("10.0.0.1")
	l.RecordFailure("10.0.0.1")
	now = now.Add(cfg.FailureWindow + time.Second)
	l.RecordFailure("10.0.0.1")

	if ok, _ := l.Allow("10.0.0.1"); !ok {
		t.Fatal("expected stale failures to have decayed rather than accumulating")
	}
}

// A client ignoring 429s must not be able to inflate its own backoff without
// bound by continuing to hammer while blocked.
func TestFailuresWhileBlockedDoNotCompound(t *testing.T) {
	cfg := enabled()
	l := authlimit.NewLimiter(cfg)
	now := time.Now()
	authlimit.SetClock(l, func() time.Time { return now })

	failN(l, "10.0.0.1", cfg.FailureThreshold)
	_, first := l.Allow("10.0.0.1")

	failN(l, "10.0.0.1", 50)
	_, after := l.Allow("10.0.0.1")

	if after > first {
		t.Errorf("hammering while blocked extended the block: %v then %v", first, after)
	}
}

func TestDisabledAllowsEverything(t *testing.T) {
	cfg := enabled()
	cfg.Enabled = false
	l := authlimit.NewLimiter(cfg)

	failN(l, "10.0.0.1", 100)
	if ok, _ := l.Allow("10.0.0.1"); !ok {
		t.Fatal("expected a disabled limiter to allow everything")
	}
}

// A reload must not be an amnesty for a source midway through a block.
func TestSetConfigKeepsExistingBlocks(t *testing.T) {
	l := authlimit.NewLimiter(enabled())
	failN(l, "10.0.0.1", 3)
	if ok, _ := l.Allow("10.0.0.1"); ok {
		t.Fatal("expected a block before the reload")
	}

	cfg := enabled()
	cfg.FailureThreshold = 10
	l.SetConfig(cfg)

	if ok, _ := l.Allow("10.0.0.1"); ok {
		t.Error("a reload cleared an active block")
	}
}

// Zero values must yield a working limiter, because that is what a settings row
// written before this feature existed reads as.
func TestZeroConfigTakesDefaults(t *testing.T) {
	l := authlimit.NewLimiter(authlimit.Config{Enabled: true})

	failN(l, "10.0.0.1", authlimit.DefaultFailureThreshold)
	if ok, _ := l.Allow("10.0.0.1"); ok {
		t.Fatal("expected the default threshold to apply, not an unlimited one")
	}
}

func TestSourceKey(t *testing.T) {
	cases := map[string]string{
		"192.0.2.10:54321": "192.0.2.10",
		"192.0.2.10":       "192.0.2.10",
		// Every address in one /64 collapses to the same key: a single host is
		// routinely handed a whole /64, and counting per address would let one
		// attacker start over effectively forever.
		"[2001:db8::1]:443":    "2001:db8::/64",
		"[2001:db8::dead]:443": "2001:db8::/64",
		// A different /64 is a different source.
		"[2001:db8:0:1::1]:443": "2001:db8:0:1::/64",
		// IPv4-mapped IPv6 is still one IPv4 address, not a /64.
		"[::ffff:192.0.2.10]:443": "192.0.2.10",
	}
	for input, want := range cases {
		if got := authlimit.SourceKey(input); got != want {
			t.Errorf("SourceKey(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSourceKeyHandlesNonAddress(t *testing.T) {
	if got := authlimit.SourceKey("@"); got != "@" {
		t.Errorf("expected an unparseable peer to be returned verbatim, got %q", got)
	}
}

// An empty source is not tracked: with nothing to attribute failures to, one
// caller's failures would otherwise block every other unattributable caller.
func TestEmptySourceIsNotTracked(t *testing.T) {
	l := authlimit.NewLimiter(enabled())
	failN(l, "", 100)

	if ok, _ := l.Allow(""); !ok {
		t.Fatal("expected an unattributable source to be allowed")
	}
	if got := l.Tracked(); got != 0 {
		t.Errorf("expected no tracked sources, got %d", got)
	}
}

func TestConcurrentUse(t *testing.T) {
	l := authlimit.NewLimiter(enabled())
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 200; j++ {
				l.RecordFailure("10.0.0.1")
				l.Allow("10.0.0.1")
				l.RecordSuccess("10.0.0.2")
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

// Regression: the opportunistic sweep used to evict an entry as soon as its
// failure window lapsed, which discarded the backoff history along with it. A
// source could then cycle forever at the base delay -- offend, serve the block,
// get forgotten, offend again -- and the escalation never bit.
func TestSweepKeepsBackoffHistory(t *testing.T) {
	cfg := enabled()
	l := authlimit.NewLimiter(cfg)
	now := time.Now()
	authlimit.SetClock(l, func() time.Time { return now })

	failN(l, "10.0.0.1", cfg.FailureThreshold)
	_, first := l.Allow("10.0.0.1")

	// Serve the block out, and idle well past the failure window so a sweep is
	// due and the entry looks stale.
	now = now.Add(first + cfg.FailureWindow + time.Second)

	failN(l, "10.0.0.1", cfg.FailureThreshold)
	_, second := l.Allow("10.0.0.1")

	if second <= first {
		t.Errorf("a repeat offender was forgiven by the sweep: first %v, second %v", first, second)
	}
}
