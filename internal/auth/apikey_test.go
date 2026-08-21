package auth_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"obsdatalayer/internal/auth"
)

func mustKey(t *testing.T, svc *auth.Service, user, label string) auth.GeneratedAPIKey {
	t.Helper()
	key, err := svc.CreateAPIKey(user, label, nil)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	return key
}

// A key authenticates as its owner. That is the whole model: a credential, not
// a second authorization concept.
func TestAPIKeyAuthenticatesAsOwner(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")

	key := mustKey(t, svc, "alice", "promtail-prod")
	u, err := svc.AuthenticateAPIKey(key.Secret)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if u.Name != "alice" {
		t.Errorf("authenticated as %q, want alice", u.Name)
	}
}

// The token is issued once. Only its hash is stored, so nothing can hand it back.
func TestAPIKeySecretIsNotRecoverable(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")
	key := mustKey(t, svc, "alice", "promtail-prod")

	keys, err := svc.ListAPIKeys("alice")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
	// The listed metadata must not contain the secret in any field.
	rendered := keys[0].ID + keys[0].User + keys[0].Label + keys[0].Handle
	secretHalf := key.Secret[strings.LastIndex(key.Secret, "_")+1:]
	if strings.Contains(rendered, secretHalf) {
		t.Error("the secret is recoverable from the key metadata")
	}
}

func TestAPIKeyRejectsWrongSecret(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")
	key := mustKey(t, svc, "alice", "promtail-prod")

	// Same handle, different secret.
	tampered := key.Secret[:strings.LastIndex(key.Secret, "_")+1] + "not-the-right-secret"
	if _, err := svc.AuthenticateAPIKey(tampered); !errors.Is(err, auth.ErrInvalidAPIKey) {
		t.Errorf("expected rejection, got %v", err)
	}
}

func TestAPIKeyRejectsMalformedTokens(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")
	mustKey(t, svc, "alice", "promtail-prod")

	for _, bad := range []string{
		"", "garbage", "obsgw", "obsgw_only-two", "wrongprefix_handle_secret",
		"obsgw__secret", "obsgw_handle_",
	} {
		if _, err := svc.AuthenticateAPIKey(bad); !errors.Is(err, auth.ErrInvalidAPIKey) {
			t.Errorf("token %q: expected rejection, got %v", bad, err)
		}
	}
}

// Revocation must be immediate. A credential that keeps working until the next
// scheduled reload is not revoked.
func TestAPIKeyRevocationIsImmediate(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")
	key := mustKey(t, svc, "alice", "promtail-prod")

	if _, err := svc.AuthenticateAPIKey(key.Secret); err != nil {
		t.Fatalf("key should work before revocation: %v", err)
	}
	if err := svc.DeleteAPIKey("alice", key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := svc.AuthenticateAPIKey(key.Secret); !errors.Is(err, auth.ErrInvalidAPIKey) {
		t.Errorf("a revoked key still authenticates: %v", err)
	}
}

// Deleting the owner must take its keys with it, or a key would outlive the
// account and recreating the name would silently revive it.
func TestDeletingUserRevokesItsKeys(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")
	key := mustKey(t, svc, "alice", "promtail-prod")

	if err := svc.DeleteUser("alice"); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if _, err := svc.AuthenticateAPIKey(key.Secret); !errors.Is(err, auth.ErrInvalidAPIKey) {
		t.Errorf("a deleted user's key still authenticates: %v", err)
	}

	// And recreating the name must not resurrect it.
	if err := svc.CreateUser("alice", testPassword, nil); err != nil {
		t.Fatalf("recreate user: %v", err)
	}
	if _, err := svc.AuthenticateAPIKey(key.Secret); !errors.Is(err, auth.ErrInvalidAPIKey) {
		t.Errorf("recreating the user revived an old key: %v", err)
	}
}

// Expiry is optional, and honoured when set.
func TestAPIKeyExpiry(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")

	// A key with no expiry keeps working: an unattended shipper whose
	// credential lapses on a forgotten date is an outage.
	forever := mustKey(t, svc, "alice", "no-expiry")
	if _, err := svc.AuthenticateAPIKey(forever.Secret); err != nil {
		t.Errorf("a key with no expiry was rejected: %v", err)
	}

	// Expiry must be in the future at creation.
	past := time.Now().Add(-time.Hour)
	if _, err := svc.CreateAPIKey("alice", "already-dead", &past); err == nil {
		t.Error("expected an expiry in the past to be rejected")
	}

	future := time.Now().Add(time.Hour)
	timed, err := svc.CreateAPIKey("alice", "timed", &future)
	if err != nil {
		t.Fatalf("create timed key: %v", err)
	}
	if _, err := svc.AuthenticateAPIKey(timed.Secret); err != nil {
		t.Errorf("an unexpired key was rejected: %v", err)
	}
	if timed.ExpiresAt == nil || !timed.ExpiresAt.Equal(future) {
		t.Errorf("expiry not recorded: %v", timed.ExpiresAt)
	}
}

func TestAPIKeyRequiresExistingUserAndLabel(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")

	if _, err := svc.CreateAPIKey("nobody", "x", nil); err == nil {
		t.Error("expected a key for an unknown user to be rejected")
	}
	if _, err := svc.CreateAPIKey("alice", "", nil); err == nil {
		t.Error("expected an unlabelled key to be rejected: a key nobody can identify cannot be retired")
	}
}

// Two keys for one user are the point of the feature: issue the new one, deploy
// it, then revoke the old, without the account ever being without a credential.
func TestUserCanHoldMultipleKeys(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")

	old := mustKey(t, svc, "alice", "old")
	fresh := mustKey(t, svc, "alice", "new")

	for _, k := range []auth.GeneratedAPIKey{old, fresh} {
		if _, err := svc.AuthenticateAPIKey(k.Secret); err != nil {
			t.Fatalf("key %q should authenticate: %v", k.Label, err)
		}
	}
	if err := svc.DeleteAPIKey("alice", old.ID); err != nil {
		t.Fatalf("revoke old: %v", err)
	}
	if _, err := svc.AuthenticateAPIKey(old.Secret); err == nil {
		t.Error("the retired key still authenticates")
	}
	if _, err := svc.AuthenticateAPIKey(fresh.Secret); err != nil {
		t.Errorf("revoking one key broke the other: %v", err)
	}
}

// Keys are per user: one user's key must never resolve to another.
func TestAPIKeyIsScopedToItsOwner(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")
	mustUser(t, svc, "bob")

	aliceKey := mustKey(t, svc, "alice", "alice-key")
	u, err := svc.AuthenticateAPIKey(aliceKey.Secret)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if u.Name != "alice" {
		t.Errorf("alice's key resolved to %q", u.Name)
	}

	// Bob cannot revoke it either.
	if err := svc.DeleteAPIKey("bob", aliceKey.ID); err == nil {
		t.Error("one user revoked another user's key")
	}
	if _, err := svc.AuthenticateAPIKey(aliceKey.Secret); err != nil {
		t.Errorf("the key was revoked by the wrong owner: %v", err)
	}
}

// Two keys must never collide, and a token must be long enough that guessing is
// hopeless.
func TestGeneratedKeysAreDistinctAndHighEntropy(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")

	seen := map[string]bool{}
	for i := 0; i < 25; i++ {
		k := mustKey(t, svc, "alice", "key")
		if seen[k.Secret] {
			t.Fatal("generated a duplicate key")
		}
		seen[k.Secret] = true
		if !strings.HasPrefix(k.Secret, "obsgw_") {
			t.Errorf("token lacks its identifying prefix: %q", k.Secret)
		}
		// 6 bytes of handle plus 32 of secret, encoded.
		if len(k.Secret) < 50 {
			t.Errorf("token is only %d chars: %q", len(k.Secret), k.Secret)
		}
	}
}

// Last-used is what makes it safe to retire a stale credential.
func TestAPIKeyRecordsLastUsed(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")
	key := mustKey(t, svc, "alice", "promtail-prod")

	before, err := svc.ListAPIKeys("alice")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if before[0].LastUsed != nil {
		t.Errorf("an unused key reports a last-used time: %v", before[0].LastUsed)
	}

	if _, err := svc.AuthenticateAPIKey(key.Secret); err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	after, err := svc.ListAPIKeys("alice")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if after[0].LastUsed == nil {
		t.Fatal("using a key did not record a last-used time")
	}
}

func TestBearerTokenParsing(t *testing.T) {
	cases := map[string]struct {
		token string
		ok    bool
	}{
		"Bearer abc123":  {"abc123", true},
		"bearer abc123":  {"abc123", true},
		"BEARER abc123":  {"abc123", true},
		"Bearer   abc  ": {"abc", true},
		"Basic abc123":   {"", false},
		"Bearer":         {"", false},
		"Bearer ":        {"", false},
		"":               {"", false},
	}
	for header, want := range cases {
		got, ok := auth.BearerToken(header)
		if ok != want.ok || got != want.token {
			t.Errorf("BearerToken(%q) = (%q, %v), want (%q, %v)", header, got, ok, want.token, want.ok)
		}
	}
}
