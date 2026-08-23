package auth_test

import (
	"testing"

	"obsdatalayer/internal/auth"
)

// The operational actions -- status, config and metrics -- gate
// the upstream endpoints that are not tenant-scoped. These tests pin the
// vocabulary and, most of all, the one carve-out that a reader would not
// otherwise expect: a wildcard grant does not include metrics.

func TestOperationalGrantsAreValidOnEveryBackend(t *testing.T) {
	env := newTestEnv(t)

	for _, backend := range []string{"loki", "mimir", "tempo"} {
		for _, action := range []string{auth.ActionStatus, auth.ActionConfig, auth.ActionMetrics} {
			t.Run(backend+"/"+action, func(t *testing.T) {
				if err := grant(backend, action, env.a).Validate(); err != nil {
					t.Fatalf("grant %s:%s rejected: %v", backend, action, err)
				}
			})
		}
	}

	// Tempo previously supported no discrete action at all, so a grant naming
	// one was rejected outright. It must still reject the ones Tempo genuinely
	// does not serve, with an explanation rather than a bare refusal.
	err := grant("tempo", auth.ActionRulesRead, env.a).Validate()
	if err == nil {
		t.Fatal("tempo has no ruler; rules:read must be rejected")
	}
}

// TestOperationalGrantsCarryTenants is the design decision made explicit: these
// grants are ordinary tenant-carrying grants. The tenant IDs decide which
// instances the holder may address; they are never sent upstream, because the
// endpoints are not registered behind their backend's tenant middleware.
func TestOperationalGrantsCarryTenants(t *testing.T) {
	env := newTestEnv(t)

	tenantless := auth.Grant{Backend: "loki", Action: auth.ActionStatus}
	if err := tenantless.Validate(); err == nil {
		t.Fatal("a grant with no tenant_ids must be rejected, like every other data-plane grant")
	}
	// Unlike write or tail, an operational grant may span tenants: it selects
	// instances rather than resolving to one tenant to assert upstream.
	if err := grant("loki", auth.ActionStatus, env.a, env.b).Validate(); err != nil {
		t.Fatalf("a multi-tenant status grant must be allowed: %v", err)
	}
}

func TestOperationalActionsAreReadLike(t *testing.T) {
	for _, action := range []string{auth.ActionStatus, auth.ActionConfig, auth.ActionMetrics} {
		if !auth.ActionIsRead(action) {
			t.Errorf("%s must classify as a read; nothing about it mutates a backend", action)
		}
		if auth.ActionIsWrite(action) {
			t.Errorf("%s must not classify as a write", action)
		}
		if auth.ActionRequiresSingleTenant(action) {
			t.Errorf("%s selects instances rather than asserting a tenant, so it may span tenants", action)
		}
	}
}

// TestWildcardGrantCoversStatusAndConfigButNotMetrics is the carve-out. It goes
// through the real Service, so it exercises the Casbin matcher rather than the
// Go-side helper that has to agree with it.
func TestWildcardGrantCoversStatusAndConfigButNotMetrics(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc

	mustRole(t, svc, "everything", grant("*", auth.ActionAny, env.a))
	mustUser(t, svc, "alice", "everything")

	for _, tc := range []struct {
		action string
		want   bool
	}{
		{action: auth.ActionRead, want: true},
		{action: auth.ActionWrite, want: true},
		{action: auth.ActionRulesRead, want: true},
		{action: auth.ActionStatus, want: true},
		{action: auth.ActionConfig, want: true},
		// The one exception: the raw exposition carries every tenant's series,
		// so breadth alone must not confer it.
		{action: auth.ActionMetrics, want: false},
	} {
		t.Run(tc.action, func(t *testing.T) {
			got := svc.AccessDecision("alice", "loki", tc.action).Allowed
			if got != tc.want {
				t.Fatalf("wildcard grant on %s: allowed=%v, want %v", tc.action, got, tc.want)
			}
		})
	}
}

// TestMetricsReadMustBeGrantedByName is the other half: the carve-out withholds
// it from a wildcard, it does not make it ungrantable.
func TestMetricsReadMustBeGrantedByName(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc

	mustRole(t, svc, "operator", grant("loki", auth.ActionMetrics, env.a))
	mustUser(t, svc, "bob", "operator")

	if !svc.AccessDecision("bob", "loki", auth.ActionMetrics).Allowed {
		t.Fatal("an explicit metrics grant must authorize metrics")
	}
	// And it confers nothing else.
	if svc.AccessDecision("bob", "loki", auth.ActionRead).Allowed {
		t.Fatal("metrics must not confer read")
	}
	if svc.AccessDecision("bob", "mimir", auth.ActionMetrics).Allowed {
		t.Fatal("a loki grant must not confer metrics on mimir")
	}
}
