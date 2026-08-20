package proxy

import (
	"testing"
	"time"

	"obsdatalayer/internal/config"
)

func targets(urls ...string) []config.PushTarget {
	out := make([]config.PushTarget, len(urls))
	for i, u := range urls {
		out[i] = config.PushTarget{URL: u}
	}
	return out
}

func urls(ts []config.PushTarget) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.URL
	}
	return out
}

func equal(a []string, b ...string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Below the threshold nothing is demoted: a single timeout during a rolling
// restart should not move a healthy replica down the order.
func TestHealthKeepsOrderBelowThreshold(t *testing.T) {
	h := newTargetHealth()
	h.recordFailure("a")

	if got := urls(h.order(targets("a", "b"))); !equal(got, "a", "b") {
		t.Errorf("order = %v, want the configured order", got)
	}
}

func TestHealthDemotesAfterThreshold(t *testing.T) {
	h := newTargetHealth()
	for i := 0; i < readFailureThreshold; i++ {
		h.recordFailure("a")
	}

	if !h.coolingOff("a") {
		t.Fatal("expected the target to be cooling off")
	}
	if got := urls(h.order(targets("a", "b"))); !equal(got, "b", "a") {
		t.Errorf("order = %v, want the failing target last", got)
	}
}

// Success is proof of health, whatever came before it.
func TestHealthSuccessClearsFailures(t *testing.T) {
	h := newTargetHealth()
	for i := 0; i < readFailureThreshold; i++ {
		h.recordFailure("a")
	}
	h.recordSuccess("a")

	if h.coolingOff("a") {
		t.Error("a target that answered is still cooling off")
	}
	if got := urls(h.order(targets("a", "b"))); !equal(got, "a", "b") {
		t.Errorf("order = %v, want the configured order restored", got)
	}
}

func TestHealthCoolOffExpires(t *testing.T) {
	h := newTargetHealth()
	now := time.Now()
	h.now = func() time.Time { return now }

	for i := 0; i < readFailureThreshold; i++ {
		h.recordFailure("a")
	}
	if !h.coolingOff("a") {
		t.Fatal("expected cool-off")
	}

	now = now.Add(readCoolOff + time.Second)
	if h.coolingOff("a") {
		t.Error("cool-off did not expire")
	}
}

// Refusing to read at all would be worse than trying a target that might have
// recovered, so a fully-demoted set keeps its configured order.
func TestHealthNeverReturnsEmpty(t *testing.T) {
	h := newTargetHealth()
	for _, u := range []string{"a", "b"} {
		for i := 0; i < readFailureThreshold; i++ {
			h.recordFailure(u)
		}
	}

	got := urls(h.order(targets("a", "b")))
	if len(got) != 2 {
		t.Fatalf("order dropped targets: %v", got)
	}
	if !equal(got, "a", "b") {
		t.Errorf("order = %v, want the configured order when everything is cooling off", got)
	}
}

// Health is keyed by URL so it survives a config reload; the config is rebuilt
// and swapped every 30 seconds by default, and health that reset with it would
// never reach the threshold.
func TestHealthSurvivesRebuiltTargetValues(t *testing.T) {
	h := newTargetHealth()
	for i := 0; i < readFailureThreshold; i++ {
		h.recordFailure("http://a.local")
	}

	// A reload produces fresh PushTarget values for the same URLs.
	rebuilt := targets("http://a.local", "http://b.local")
	if got := urls(h.order(rebuilt)); !equal(got, "http://b.local", "http://a.local") {
		t.Errorf("order = %v, want health to survive the rebuild", got)
	}
}

// A single target has nowhere to fail over to, so ordering is a no-op.
func TestHealthSingleTargetIsUntouched(t *testing.T) {
	h := newTargetHealth()
	for i := 0; i < readFailureThreshold*3; i++ {
		h.recordFailure("only")
	}

	if got := urls(h.order(targets("only"))); !equal(got, "only") {
		t.Errorf("order = %v, want the single target returned", got)
	}
}

// Instances come and go at runtime; without eviction the map would keep a row
// for every URL ever configured.
func TestHealthSweepsStaleEntries(t *testing.T) {
	h := newTargetHealth()
	now := time.Now()
	h.now = func() time.Time { return now }

	h.recordFailure("gone")
	now = now.Add(20 * readCoolOff)
	h.recordFailure("current")

	h.mu.Lock()
	_, stale := h.state["gone"]
	h.mu.Unlock()
	if stale {
		t.Error("a long-idle target was not swept")
	}
}
