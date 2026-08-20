// Package authlimit bounds what an unauthenticated caller can cost the gateway.
//
// Authentication is deliberately expensive. Passwords are bcrypt hashed, and an
// unknown username is compared against a dummy hash so that a wrong username
// costs the same as a wrong password -- that is what closes the username
// enumeration timing channel. The consequence is that every rejected credential
// costs a full bcrypt, tens of milliseconds of CPU, to a caller who has proved
// nothing. The successful-credential cache does not help: it is populated only
// after a compare succeeds, so failures never touch it.
//
// Two mechanisms answer that, and they answer different threats:
//
//   - Limiter throttles repeated failures from one source. It stops a single
//     client brute-forcing credentials, and it is the cheap common case.
//   - Gate caps how many password hashes may run at once, process-wide. It is
//     what still holds when the load is spread across many sources, or when the
//     gateway sits behind a proxy and every request shares one address. Without
//     it, per-source limiting alone bounds nothing in aggregate.
//
// Neither trusts a proxy header. The source is the transport-level peer address,
// consistent with the rest of the gateway, which treats X-Forwarded-For as an
// untrusted claim of identity. Behind a load balancer that makes the Limiter
// coarse -- every client shares the balancer's address -- which is exactly why
// the Gate exists.
package authlimit

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// Defaults applied when a configuration field is left at zero.
const (
	DefaultFailureThreshold = 5
	DefaultFailureWindow    = time.Minute
	DefaultBlockDuration    = time.Minute
	DefaultMaxBlockDuration = 15 * time.Minute

	// maxTrackedSources caps the failure table. An attacker spraying from many
	// addresses would otherwise turn the defence into a memory leak. Past the
	// cap new sources are not tracked, and the Gate carries the load instead.
	maxTrackedSources = 50_000

	// sweepInterval is the minimum gap between opportunistic evictions of
	// expired entries. Sweeping runs on the request path, so it is rate limited
	// rather than run on every failure.
	sweepInterval = time.Minute
)

// Config tunes the per-source failure throttle. Zero values take the defaults
// above, so a settings row written before this feature existed still yields a
// working limiter rather than an inert one.
type Config struct {
	// Enabled turns per-source throttling off entirely. The Gate is unaffected.
	Enabled bool

	// FailureThreshold is how many failures a source may accumulate within
	// FailureWindow before it is blocked.
	FailureThreshold int

	// FailureWindow is the period over which failures accumulate. A source that
	// stops failing for this long starts again with a clean slate.
	FailureWindow time.Duration

	// BlockDuration is the first block imposed once the threshold is crossed.
	// Each further block doubles it, up to MaxBlockDuration.
	BlockDuration time.Duration

	// MaxBlockDuration caps the exponential backoff.
	MaxBlockDuration time.Duration
}

func (c Config) withDefaults() Config {
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = DefaultFailureThreshold
	}
	if c.FailureWindow <= 0 {
		c.FailureWindow = DefaultFailureWindow
	}
	if c.BlockDuration <= 0 {
		c.BlockDuration = DefaultBlockDuration
	}
	if c.MaxBlockDuration <= 0 {
		c.MaxBlockDuration = DefaultMaxBlockDuration
	}
	if c.MaxBlockDuration < c.BlockDuration {
		c.MaxBlockDuration = c.BlockDuration
	}
	return c
}

// entry is one source's failure state.
type entry struct {
	failures     int
	windowStart  time.Time
	blockedUntil time.Time
	// blocks counts how many times this source has been blocked, and drives the
	// exponential backoff. It is deliberately not reset when a block expires:
	// a source that keeps coming back should be held off for longer each time.
	blocks int
	seen   time.Time
}

// Limiter throttles sources that repeatedly fail authentication.
//
// It is safe for concurrent use. All operations take a single mutex; the work
// under it is a map lookup and a few comparisons, which is orders of magnitude
// cheaper than the bcrypt it is protecting.
type Limiter struct {
	mu        sync.Mutex
	cfg       Config
	entries   map[string]*entry
	lastSweep time.Time

	// now is swappable so tests can drive time directly rather than sleeping.
	now func() time.Time
}

// NewLimiter builds a limiter with cfg applied.
func NewLimiter(cfg Config) *Limiter {
	return &Limiter{
		cfg:     cfg.withDefaults(),
		entries: make(map[string]*entry),
		now:     time.Now,
	}
}

// SetConfig replaces the configuration, which is how a settings reload takes
// effect. Existing failure state is kept: a reload is not an amnesty for a
// source that is midway through a block.
func (l *Limiter) SetConfig(cfg Config) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cfg = cfg.withDefaults()
}

// Allow reports whether a source may attempt authentication. When it may not,
// the returned duration is how long the caller should wait, for a Retry-After
// header.
func (l *Limiter) Allow(source string) (bool, time.Duration) {
	if source == "" {
		return true, 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.cfg.Enabled {
		return true, 0
	}

	e, ok := l.entries[source]
	if !ok {
		return true, 0
	}
	now := l.now()
	e.seen = now
	if now.Before(e.blockedUntil) {
		// Round up so a sub-second remainder does not render as "Retry-After: 0".
		wait := e.blockedUntil.Sub(now)
		return false, wait.Round(time.Second) + time.Second
	}
	return true, 0
}

// RecordFailure notes a rejected credential and blocks the source once it has
// failed too often inside the window.
func (l *Limiter) RecordFailure(source string) {
	if source == "" {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.cfg.Enabled {
		return
	}

	now := l.now()
	l.sweepLocked(now)

	e, ok := l.entries[source]
	if !ok {
		if len(l.entries) >= maxTrackedSources {
			// Table is full. Declining to track is better than evicting an
			// entry that is actively blocking an attacker.
			return
		}
		e = &entry{windowStart: now}
		l.entries[source] = e
	}
	e.seen = now

	// A source already inside a block does not accumulate further failures --
	// it should not be reaching bcrypt at all, and counting these would let a
	// client that ignores 429s inflate its own backoff without limit.
	if now.Before(e.blockedUntil) {
		return
	}

	if now.Sub(e.windowStart) > l.cfg.FailureWindow {
		e.windowStart = now
		e.failures = 0
	}
	e.failures++

	if e.failures >= l.cfg.FailureThreshold {
		e.blockedUntil = now.Add(l.backoffLocked(e.blocks))
		e.blocks++
		e.failures = 0
		e.windowStart = now
	}
}

// RecordSuccess clears a source's failure state. A caller that proves it holds
// a valid credential is not the caller this is defending against.
func (l *Limiter) RecordSuccess(source string) {
	if source == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, source)
}

// backoffLocked returns the block length for the nth block of a source,
// doubling each time and capped at MaxBlockDuration.
func (l *Limiter) backoffLocked(blocks int) time.Duration {
	d := l.cfg.BlockDuration
	for i := 0; i < blocks; i++ {
		d *= 2
		if d >= l.cfg.MaxBlockDuration {
			return l.cfg.MaxBlockDuration
		}
	}
	if d > l.cfg.MaxBlockDuration {
		return l.cfg.MaxBlockDuration
	}
	return d
}

// sweepLocked drops entries that are neither blocked nor inside their failure
// window. It runs at most once per sweepInterval so that a burst of failures
// does not walk the whole table on every request.
func (l *Limiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < sweepInterval {
		return
	}
	l.lastSweep = now

	// An entry is only removable once its block has expired and it has been
	// idle for longer than the window, so sweeping cannot forgive an active
	// attacker.
	idle := l.cfg.FailureWindow
	if idle < time.Minute {
		idle = time.Minute
	}
	for source, e := range l.entries {
		if now.Before(e.blockedUntil) {
			continue
		}
		retain := idle
		if e.blocks > 0 {
			// A source that has been blocked keeps its history far longer than
			// one that has merely failed. Forgetting it as soon as the failure
			// window lapses would hand it a way to cycle indefinitely at the
			// base delay: offend, serve the block, be swept, offend again. The
			// backoff only bites if what it counts outlives the block it
			// imposed.
			retain = l.cfg.MaxBlockDuration
			if retain < idle {
				retain = idle
			}
		}
		if now.Sub(e.seen) > retain {
			delete(l.entries, source)
		}
	}
}

// Tracked reports how many sources currently hold state. Exposed for tests and
// for the admin metrics summary.
func (l *Limiter) Tracked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// SourceKey reduces a RemoteAddr to the key failures are counted against.
//
// IPv6 is grouped by /64 rather than by exact address. A single host is
// routinely given a whole /64, so counting per address would let one attacker
// start over 18 quintillion times.
func SourceKey(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		// Not an address the gateway can reason about -- a Unix socket, say.
		// Return it verbatim so it is still counted as one source.
		return host
	}
	addr = addr.Unmap()
	if addr.Is6() {
		if prefix, err := addr.Prefix(64); err == nil {
			return prefix.String()
		}
	}
	return addr.String()
}
