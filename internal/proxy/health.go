package proxy

import (
	"sync"
	"time"

	"obsdatalayer/internal/config"
)

// Read failover tuning. These are constants rather than settings because they
// describe how quickly the gateway notices a target is down, not a policy an
// operator is likely to want to express. Promote them to the settings row if
// that turns out to be wrong.
const (
	// readFailureThreshold is how many consecutive read failures put a target
	// into cool-off. One failure is not enough: a single timeout during a
	// rolling restart should not demote a healthy replica.
	readFailureThreshold = 2

	// readCoolOff is how long a failing target is skipped before being tried
	// again. Short enough that recovery is picked up quickly, long enough that
	// a dead target is not re-dialled on every request.
	readCoolOff = 15 * time.Second
)

// targetHealth remembers which read targets are failing, so that one dead
// replica does not add a connection timeout to every read.
//
// It is keyed by target URL rather than by instance, so it survives a config
// reload: the config is rebuilt and swapped wholesale, and health that reset on
// every reload would be forgotten every 30 seconds by default and never reach
// the failure threshold.
//
// The zero value is not usable; construct with newTargetHealth.
type targetHealth struct {
	mu    sync.Mutex
	state map[string]*targetState
	now   func() time.Time
}

type targetState struct {
	failures int
	until    time.Time
	// seen is the last time this target was touched, used to evict entries for
	// targets that no longer exist in any instance.
	seen time.Time
}

func newTargetHealth() *targetHealth {
	return &targetHealth{state: make(map[string]*targetState), now: time.Now}
}

// order returns targets with the ones currently in cool-off moved to the back,
// preserving the configured order within each group.
//
// It never returns fewer targets than it was given. When every target is in
// cool-off the original order is returned unchanged: the alternative is
// refusing to read at all, and a target that might have recovered is a better
// answer than no answer.
func (h *targetHealth) order(targets []config.PushTarget) []config.PushTarget {
	if len(targets) < 2 {
		return targets
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()

	healthy := make([]config.PushTarget, 0, len(targets))
	cooling := make([]config.PushTarget, 0, len(targets))
	for _, t := range targets {
		if st, ok := h.state[t.URL]; ok && now.Before(st.until) {
			cooling = append(cooling, t)
			continue
		}
		healthy = append(healthy, t)
	}
	if len(cooling) == 0 {
		return targets
	}
	return append(healthy, cooling...)
}

// recordFailure notes a failed read against a target, putting it into cool-off
// once it has failed often enough in a row.
func (h *targetHealth) recordFailure(url string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.sweepLocked(now)

	st, ok := h.state[url]
	if !ok {
		st = &targetState{}
		h.state[url] = st
	}
	st.seen = now
	st.failures++
	if st.failures >= readFailureThreshold {
		st.until = now.Add(readCoolOff)
	}
}

// recordSuccess clears a target's failure history. A target that answers is
// healthy, whatever it did before.
func (h *targetHealth) recordSuccess(url string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.state[url]; ok {
		delete(h.state, url)
	}
}

// coolingOff reports whether a target is currently being skipped. For tests and
// for the debug log.
func (h *targetHealth) coolingOff(url string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.state[url]
	return ok && h.now().Before(st.until)
}

// sweepLocked drops entries for targets that have not been seen for a while.
// Instances are created and deleted at runtime, so without this the map would
// retain a row for every URL ever configured.
func (h *targetHealth) sweepLocked(now time.Time) {
	for url, st := range h.state {
		if now.Before(st.until) {
			continue
		}
		if now.Sub(st.seen) > 10*readCoolOff {
			delete(h.state, url)
		}
	}
}
