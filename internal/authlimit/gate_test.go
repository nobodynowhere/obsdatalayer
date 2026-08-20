package authlimit_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"obsdatalayer/internal/authlimit"
)

func TestGateAdmitsUpToCap(t *testing.T) {
	g := authlimit.NewGate(2, 0)

	if !g.Acquire(context.Background()) || !g.Acquire(context.Background()) {
		t.Fatal("expected the first two acquisitions to succeed")
	}
	if g.Acquire(context.Background()) {
		t.Fatal("expected the third acquisition to be refused")
	}
	if got := g.InFlight(); got != 2 {
		t.Errorf("InFlight = %d, want 2", got)
	}

	g.Release()
	if !g.Acquire(context.Background()) {
		t.Error("expected a released slot to be reusable")
	}
}

// A nil gate is how "no limit configured" is represented, and must behave as an
// unlimited one rather than panicking.
func TestNilGateIsUnlimited(t *testing.T) {
	var g *authlimit.Gate

	for i := 0; i < 100; i++ {
		if !g.Acquire(context.Background()) {
			t.Fatal("expected a nil gate to admit everything")
		}
	}
	g.Release()
	if got := g.Cap(); got != 0 {
		t.Errorf("Cap = %d, want 0 for unlimited", got)
	}
}

func TestNewGateWithNonPositiveMaxIsUnlimited(t *testing.T) {
	for _, max := range []int{0, -1} {
		if g := authlimit.NewGate(max, 0); g != nil {
			t.Errorf("NewGate(%d) = %v, want nil (unlimited)", max, g)
		}
	}
}

// A caller waits for a slot rather than being shed the instant the gate is
// full: a burst of legitimate first-time logins after a restart should queue
// briefly, not fail.
func TestGateWaitsForASlot(t *testing.T) {
	g := authlimit.NewGate(1, 500*time.Millisecond)
	if !g.Acquire(context.Background()) {
		t.Fatal("expected the first acquisition to succeed")
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		g.Release()
	}()

	start := time.Now()
	if !g.Acquire(context.Background()) {
		t.Fatal("expected the waiting caller to get the released slot")
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("returned too fast to have waited: %v", elapsed)
	}
}

// The wait is bounded. Under sustained attack an unbounded queue converts CPU
// exhaustion into memory exhaustion, so a caller that cannot be served is shed.
func TestGateWaitIsBounded(t *testing.T) {
	g := authlimit.NewGate(1, 50*time.Millisecond)
	if !g.Acquire(context.Background()) {
		t.Fatal("expected the first acquisition to succeed")
	}

	start := time.Now()
	if g.Acquire(context.Background()) {
		t.Fatal("expected the waiting caller to be shed")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("wait was not bounded: %v", elapsed)
	}
}

// A client that hangs up should stop occupying a hashing slot it will never
// read the answer from.
func TestGateAbandonsOnCanceledContext(t *testing.T) {
	g := authlimit.NewGate(1, 10*time.Second)
	if !g.Acquire(context.Background()) {
		t.Fatal("expected the first acquisition to succeed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if g.Acquire(ctx) {
		t.Fatal("expected a canceled caller not to take a slot")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("cancellation did not cut the wait short: %v", elapsed)
	}
}

// The cap is the whole point: it must hold under concurrency, or the bound it
// advertises is fiction.
func TestGateCapHoldsUnderConcurrency(t *testing.T) {
	const cap = 4
	g := authlimit.NewGate(cap, time.Second)

	var inFlight, peak atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !g.Acquire(context.Background()) {
				return
			}
			defer g.Release()

			n := inFlight.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			inFlight.Add(-1)
		}()
	}
	wg.Wait()

	if got := peak.Load(); got > cap {
		t.Errorf("concurrent holders peaked at %d, above the cap of %d", got, cap)
	}
	if got := g.InFlight(); got != 0 {
		t.Errorf("expected every slot released, %d still held", got)
	}
}

// Releasing without acquiring would permanently enlarge the gate, quietly
// removing the bound. Better to fail loudly.
func TestGateReleaseWithoutAcquirePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected an unmatched Release to panic")
		}
	}()
	authlimit.NewGate(1, 0).Release()
}
