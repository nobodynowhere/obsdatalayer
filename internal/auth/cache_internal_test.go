package auth

import (
	"testing"
	"time"
)

// The sweep is what bounds the map now that a reload no longer wipes it. Valid
// only evicts the key it looks up, so an entry orphaned by a password rotation
// is never revisited and would otherwise be immortal.
func TestCredentialCacheSweepBoundsGrowth(t *testing.T) {
	c := newCredentialCache(time.Minute)

	// Simulate a long-lived process rotating a credential repeatedly: each
	// rotation stores under a new key and orphans the previous one.
	for i := 0; i < 500; i++ {
		c.Store("alice", "password", "hash-generation-"+string(rune('a'+i%26))+string(rune('a'+i/26)))
		// Advance past the TTL so the previous generation is expired and the
		// next Store is eligible to sweep.
		c.mu.Lock()
		for key := range c.entries {
			c.entries[key] = time.Now().Add(-time.Second)
		}
		c.lastSweep = time.Now().Add(-2 * time.Minute)
		c.mu.Unlock()
	}

	// Whatever the history, the map must not have accumulated 500 entries.
	if got := c.size(); got > 2 {
		t.Errorf("cache grew to %d entries despite sweeping; expected it to stay bounded", got)
	}
}

// A live credential must survive its own sweep.
func TestCredentialCacheSweepKeepsLiveEntries(t *testing.T) {
	c := newCredentialCache(time.Minute)
	c.Store("alice", "password", "hash")

	c.mu.Lock()
	c.lastSweep = time.Now().Add(-2 * time.Minute) // force the next Store to sweep
	c.mu.Unlock()
	c.Store("bob", "password", "hash")

	if !c.Valid("alice", "password", "hash") {
		t.Error("the sweep evicted a live entry")
	}
	if !c.Valid("bob", "password", "hash") {
		t.Error("the entry that triggered the sweep was lost")
	}
}

// An expired entry must not authenticate, sweep or no sweep.
func TestCredentialCacheHonoursTTL(t *testing.T) {
	c := newCredentialCache(50 * time.Millisecond)
	c.Store("alice", "password", "hash")

	if !c.Valid("alice", "password", "hash") {
		t.Fatal("expected a fresh entry to be valid")
	}
	time.Sleep(80 * time.Millisecond)
	if c.Valid("alice", "password", "hash") {
		t.Error("an expired entry still authenticated")
	}
}

// The hash is part of the key. This is the invariant that makes clearing the
// cache on reload unnecessary: a rotated password cannot collide with the entry
// left behind by the old one.
func TestCredentialCacheKeyBindsHash(t *testing.T) {
	c := newCredentialCache(time.Minute)
	c.Store("alice", "password", "old-hash")

	if c.Valid("alice", "password", "new-hash") {
		t.Error("an entry stored under one hash matched a different hash")
	}
	if c.Valid("alice", "different-password", "old-hash") {
		t.Error("an entry matched a different password")
	}
	if c.Valid("bob", "password", "old-hash") {
		t.Error("an entry matched a different user")
	}
}
