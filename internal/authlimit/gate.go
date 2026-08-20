package authlimit

import (
	"context"
	"runtime"
	"time"
)

// DefaultHashWait is how long a caller waits for a hashing slot before the
// gateway gives up and sheds the request.
const DefaultHashWait = 250 * time.Millisecond

// DefaultMaxConcurrentHashes returns the hashing concurrency used when none is
// configured: half the available CPUs, and never fewer than two.
//
// Half, not all, is the point. The gateway's real job is proxying telemetry,
// and authentication is overhead on the way there. Letting password hashing
// occupy every core is precisely the denial of service this guards against, so
// the remaining half stays available to serve traffic even while an attacker is
// feeding the gateway garbage credentials.
func DefaultMaxConcurrentHashes() int {
	n := runtime.GOMAXPROCS(0) / 2
	if n < 2 {
		n = 2
	}
	return n
}

// Gate bounds how many password hashes run concurrently.
//
// It is a counting semaphore with a bounded wait. A caller that cannot get a
// slot in time is shed rather than queued indefinitely: under attack an
// unbounded queue only converts a CPU exhaustion into a memory exhaustion, and
// a client told to retry promptly is better served than one left hanging.
//
// A nil Gate imposes no limit, which is how "disabled" is represented.
type Gate struct {
	tokens chan struct{}
	wait   time.Duration
}

// NewGate builds a gate admitting max concurrent hashes, each caller waiting up
// to wait for a slot. A max of zero or less returns nil, meaning unlimited.
func NewGate(max int, wait time.Duration) *Gate {
	if max <= 0 {
		return nil
	}
	if wait < 0 {
		wait = 0
	}
	return &Gate{tokens: make(chan struct{}, max), wait: wait}
}

// Acquire takes a slot, waiting up to the configured limit. It reports whether
// a slot was obtained; when it returns true the caller must call Release.
//
// A nil Gate always admits.
func (g *Gate) Acquire(ctx context.Context) bool {
	if g == nil {
		return true
	}

	// The uncontended path: a free slot is taken without arming a timer.
	select {
	case g.tokens <- struct{}{}:
		return true
	default:
	}

	if g.wait == 0 {
		return false
	}

	timer := time.NewTimer(g.wait)
	defer timer.Stop()
	select {
	case g.tokens <- struct{}{}:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		// The client hung up. Taking a slot now would hold it for a request
		// nobody is waiting on.
		return false
	}
}

// Release returns a slot. It must be called exactly once per Acquire that
// returned true.
func (g *Gate) Release() {
	if g == nil {
		return
	}
	select {
	case <-g.tokens:
	default:
		// Releasing more than was acquired is a programming error. Dropping it
		// silently would permanently enlarge the gate, so fail loudly instead.
		panic("authlimit: Gate.Release called without a matching Acquire")
	}
}

// Cap reports the configured concurrency, or zero when unlimited.
func (g *Gate) Cap() int {
	if g == nil {
		return 0
	}
	return cap(g.tokens)
}

// InFlight reports how many slots are currently held.
func (g *Gate) InFlight() int {
	if g == nil {
		return 0
	}
	return len(g.tokens)
}
