package auth_test

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/db"
	"obsdatalayer/internal/tenant"
)

const testPassword = "correct-horse-battery-staple"

// testEnv is a Service wired to a tenant store pre-populated with three
// tenants, whose UUIDs the tests use when building grants.
type testEnv struct {
	svc     *auth.Service
	tenants *tenant.Store
	a, b, c string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	// A private in-memory database per test, shared across the pool's
	// connections so the Casbin adapter and the user table see the same data.
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	gormDB, err := db.Open(db.Config{Type: "sqlite", Path: dsn})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.Migrate(gormDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gormDB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	tenants, err := tenant.NewStore(gormDB)
	if err != nil {
		t.Fatalf("new tenant store: %v", err)
	}

	env := &testEnv{tenants: tenants}
	env.a = mustTenant(t, tenants, "tenant-a")
	env.b = mustTenant(t, tenants, "tenant-b")
	env.c = mustTenant(t, tenants, "tenant-c")

	svc, err := auth.NewService(gormDB, tenants)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	env.svc = svc
	return env
}

// mustTenant registers a tenant and returns its UUID, which is the value the
// gateway injects as X-Scope-OrgID.
func mustTenant(t *testing.T, store *tenant.Store, name string) string {
	t.Helper()
	created, err := store.Create("", name, nil)
	if err != nil {
		t.Fatalf("create tenant %q: %v", name, err)
	}
	return created.ID
}

// mustRole creates a role with the given grants.
func mustRole(t *testing.T, svc *auth.Service, name string, grants ...auth.Grant) {
	t.Helper()
	if err := svc.CreateRole(name, "", grants); err != nil {
		t.Fatalf("create role %q: %v", name, err)
	}
}

// mustUser creates a user with the given roles.
func mustUser(t *testing.T, svc *auth.Service, name string, roles ...string) {
	t.Helper()
	if err := svc.CreateUser(name, testPassword, roles); err != nil {
		t.Fatalf("create user %q: %v", name, err)
	}
}

func grant(backend, action string, tenants ...string) auth.Grant {
	return auth.Grant{Backend: backend, Action: action, TenantIDs: tenants}
}

func readPolicyGrant(selector string, tenants ...string) auth.Grant {
	return auth.Grant{Backend: "mimir", Action: "read", TenantIDs: tenants, ReadLabelSelector: selector}
}

// ---- authentication ---------------------------------------------------------

func TestAuthenticateSuccess(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")

	u, err := svc.Authenticate("alice", testPassword)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if u.Name != "alice" {
		t.Errorf("expected alice, got %q", u.Name)
	}
}

func TestAuthenticateWrongPassword(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")

	if _, err := svc.Authenticate("alice", "wrong-password-here"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestAuthenticateUnknownUser(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")

	if _, err := svc.Authenticate("nobody", testPassword); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("expected ErrInvalidCredentials, got %v", err)
	}
}

// TestAuthenticateIsTimingSafe guards a real regression: the unknown-user path
// once compared against a malformed dummy hash, which bcrypt rejected during
// parsing without doing any key stretching. That made unknown usernames resolve
// in ~375ns versus ~64ms for a real account, letting anyone enumerate valid
// usernames one request at a time.
//
// The bound is deliberately loose (unknown must cost at least a quarter of
// known) so the test cannot flake on a noisy machine, while still failing by
// several orders of magnitude if the equalizing compare is dropped.
func TestAuthenticateIsTimingSafe(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")

	measure := func(user string) time.Duration {
		const samples = 3
		best := time.Duration(1<<63 - 1)
		for i := 0; i < samples; i++ {
			start := time.Now()
			_, _ = svc.Authenticate(user, "definitely-the-wrong-password")
			if d := time.Since(start); d < best {
				best = d
			}
		}
		return best
	}

	known := measure("alice")
	unknown := measure("does-not-exist")

	if unknown < known/4 {
		t.Errorf("unknown-user auth is %v but known-user auth is %v: the unknown path "+
			"is not doing equivalent bcrypt work, so usernames are enumerable by timing",
			unknown, known)
	}
}

// ---- tenant resolution ------------------------------------------------------

func TestTenantIDsForDirectGrant(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")
	if err := svc.SetUserGrants("alice", []auth.Grant{grant("loki", "read", env.a)}); err != nil {
		t.Fatalf("set grants: %v", err)
	}

	ids, ok := svc.TenantIDsFor("alice", "loki", "read")
	if !ok {
		t.Fatal("expected loki:read to be allowed")
	}
	if len(ids) != 1 || ids[0] != env.a {
		t.Errorf("expected [%s], got %v", env.a, ids)
	}

	if _, ok := svc.TenantIDsFor("alice", "loki", "write"); ok {
		t.Error("expected loki:write to be denied")
	}
	if _, ok := svc.TenantIDsFor("alice", "mimir", "read"); ok {
		t.Error("expected mimir:read to be denied")
	}
}

func TestTenantIDsViaRole(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustRole(t, svc, "logs-reader", grant("loki", "read", env.a, env.b))
	mustUser(t, svc, "alice", "logs-reader")

	ids, ok := svc.TenantIDsFor("alice", "loki", "read")
	if !ok {
		t.Fatal("expected role-inherited grant to allow loki:read")
	}
	want := []string{env.a, env.b}
	sort.Strings(want)
	if len(ids) != 2 || ids[0] != want[0] || ids[1] != want[1] {
		t.Errorf("expected sorted %v, got %v", want, ids)
	}
}

func TestTenantIDsMergedAcrossRoles(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustRole(t, svc, "role-a", grant("loki", "read", env.b, env.a))
	mustRole(t, svc, "role-b", grant("loki", "read", env.b, env.c))
	mustUser(t, svc, "alice", "role-a", "role-b")

	ids, ok := svc.TenantIDsFor("alice", "loki", "read")
	if !ok {
		t.Fatal("expected allow")
	}
	want := []string{env.a, env.b, env.c}
	sort.Strings(want)
	if len(ids) != len(want) {
		t.Fatalf("expected %v, got %v", want, ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("expected sorted, deduplicated %v, got %v", want, ids)
		}
	}
}

func TestMimirReadAccessResolvesGrantLabelSelector(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustRole(t, svc, "metrics-reader", readPolicyGrant(` {cluster="prod"} `, env.a))
	mustUser(t, svc, "alice", "metrics-reader")

	access, ok := svc.AccessFor("alice", "mimir", "read")
	if !ok {
		t.Fatal("expected mimir:read to be allowed")
	}
	if len(access.TenantIDs) != 1 || access.TenantIDs[0] != env.a {
		t.Fatalf("expected [%s], got %v", env.a, access.TenantIDs)
	}
	if len(access.LabelSelectors) != 1 || access.LabelSelectors[0] != `{cluster="prod"}` {
		t.Fatalf("expected label selector, got %v", access.LabelSelectors)
	}
}

func TestMimirReadAccessRejectsMixedTenantLabelSelectors(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustRole(t, svc, "restricted-reader", readPolicyGrant(`{cluster="prod"}`, env.a))
	mustRole(t, svc, "unrestricted-reader", grant("mimir", "read", env.b))
	mustUser(t, svc, "alice", "restricted-reader", "unrestricted-reader")

	if _, ok := svc.AccessFor("alice", "mimir", "read"); ok {
		t.Fatal("expected mixed restricted/unrestricted tenant read to be denied")
	}
}

func TestMimirReadAccessRejectsDifferentLabelSelectors(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustRole(t, svc, "prod-reader", readPolicyGrant(`{cluster="prod"}`, env.a))
	mustRole(t, svc, "dev-reader", readPolicyGrant(`{cluster="dev"}`, env.b))
	mustUser(t, svc, "alice", "prod-reader", "dev-reader")

	if _, ok := svc.AccessFor("alice", "mimir", "read"); ok {
		t.Fatal("expected different read selectors to be denied")
	}
}

func TestWriteAccessDeniesMergedTenantsAcrossRoles(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustRole(t, svc, "writer-a", grant("mimir", "write", env.a))
	mustRole(t, svc, "writer-b", grant("mimir", "write", env.b))
	mustUser(t, svc, "alice", "writer-a", "writer-b")

	if _, ok := svc.AccessFor("alice", "mimir", "write"); ok {
		t.Fatal("expected merged write tenants to be denied")
	}
}

func TestGrantValidationRejectsMultiTenantWrite(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")

	err := svc.SetUserGrants("alice", []auth.Grant{grant("mimir", "write", env.a, env.b)})
	if !errors.Is(err, auth.ErrInvalidGrant) {
		t.Fatalf("expected ErrInvalidGrant, got %v", err)
	}
}

func TestCreateRoleRejectsDifferentWriteTenants(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc

	err := svc.CreateRole("writers", "", []auth.Grant{
		grant("loki", "write", env.a),
		grant("mimir", "write", env.b),
	})
	if !errors.Is(err, auth.ErrInvalidGrant) {
		t.Fatalf("expected ErrInvalidGrant, got %v", err)
	}
}

func TestCreateRoleAllowsReadTenantsAndSingleWriteTenant(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc

	err := svc.CreateRole("reader-writer", "", []auth.Grant{
		grant("loki", "read", env.a, env.b, env.c),
		grant("mimir", "write", env.a),
		grant("tempo", "write", env.a),
	})
	if err != nil {
		t.Fatalf("expected reads to span tenants and writes to share one tenant, got %v", err)
	}
}

func TestSetRoleGrantsRejectsDifferentWriteTenants(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustRole(t, svc, "writers", grant("loki", "write", env.a))

	err := svc.SetRoleGrants("writers", []auth.Grant{
		grant("loki", "write", env.a),
		grant("mimir", "write", env.b),
	})
	if !errors.Is(err, auth.ErrInvalidGrant) {
		t.Fatalf("expected ErrInvalidGrant, got %v", err)
	}
}

func TestGrantValidationRejectsReadLabelSelectorOnWrite(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")

	err := svc.SetUserGrants("alice", []auth.Grant{{
		Backend:           "mimir",
		Action:            "write",
		TenantIDs:         []string{env.a},
		ReadLabelSelector: `{cluster="prod"}`,
	}})
	if !errors.Is(err, auth.ErrInvalidGrant) {
		t.Fatalf("expected ErrInvalidGrant, got %v", err)
	}
}

func TestTenantIDsWildcardBackendAndAction(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustRole(t, svc, "everything", grant(auth.BackendAny, auth.ActionAny, env.a))
	mustUser(t, svc, "alice", "everything")

	for _, backend := range []string{"loki", "mimir", "tempo"} {
		for _, action := range []string{auth.ActionRead, auth.ActionWrite} {
			if _, ok := svc.TenantIDsFor("alice", backend, action); !ok {
				t.Errorf("expected wildcard grant to allow %s:%s", backend, action)
			}
		}
	}
	for _, action := range []string{auth.ActionRulesRead, auth.ActionRulesWrite, auth.ActionAlertsRead, auth.ActionAlertsWrite} {
		if _, ok := svc.TenantIDsFor("alice", "mimir", action); !ok {
			t.Errorf("expected wildcard grant to allow mimir:%s", action)
		}
	}
}

func TestMimirReadDoesNotAllowRulesOrAlerts(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustRole(t, svc, "metrics-reader", grant("mimir", auth.ActionRead, env.a, env.b))
	mustUser(t, svc, "alice", "metrics-reader")

	if _, ok := svc.TenantIDsFor("alice", "mimir", auth.ActionRead); !ok {
		t.Fatal("expected metric read to be allowed")
	}
	for _, action := range []string{auth.ActionRulesRead, auth.ActionAlertsRead} {
		if _, ok := svc.TenantIDsFor("alice", "mimir", action); ok {
			t.Fatalf("mimir:read must not allow mimir:%s", action)
		}
	}
}

func TestMimirRulesAndAlertsReadCanBeMultiTenant(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustRole(t, svc, "rules-a", grant("mimir", auth.ActionRulesRead, env.a))
	mustRole(t, svc, "rules-b", grant("mimir", auth.ActionRulesRead, env.b))
	mustRole(t, svc, "alerts-ab", grant("mimir", auth.ActionAlertsRead, env.a, env.b))
	mustUser(t, svc, "alice", "rules-a", "rules-b", "alerts-ab")

	want := []string{env.a, env.b}
	sort.Strings(want)
	if tenants, ok := svc.TenantIDsFor("alice", "mimir", auth.ActionRulesRead); !ok || !equalStrings(tenants, want) {
		t.Fatalf("expected alice rules read for tenants A+B, got %v ok=%v", tenants, ok)
	}
	if tenants, ok := svc.TenantIDsFor("alice", "mimir", auth.ActionAlertsRead); !ok || !equalStrings(tenants, want) {
		t.Fatalf("expected alice alerts read for tenants A+B, got %v ok=%v", tenants, ok)
	}
}

func TestUnknownUserHasNoTenants(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	if _, ok := svc.TenantIDsFor("ghost", "loki", "read"); ok {
		t.Error("expected unknown subject to be denied")
	}
}

// ---- admin boundary ---------------------------------------------------------

// TestWildcardGrantDoesNotConferAdmin pins the central privilege-separation
// property of the Casbin model: a data-plane wildcard must never reach the
// admin plane, which has to be granted explicitly.
func TestWildcardGrantDoesNotConferAdmin(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustRole(t, svc, "everything", grant(auth.BackendAny, auth.ActionAny, env.a))
	mustUser(t, svc, "alice", "everything")

	if svc.CanAdmin("alice") {
		t.Error("a (*, *) data grant must not confer admin-plane access")
	}
	if _, ok := svc.TenantIDsFor("alice", auth.ObjectAdmin, auth.ActionAccess); ok {
		t.Error("a (*, *) data grant must not match the admin object")
	}
}

func TestExplicitAdminGrant(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustRole(t, svc, "ops", auth.Grant{Backend: auth.ObjectAdmin, Action: auth.ActionAccess})
	mustUser(t, svc, "root", "ops")

	if !svc.CanAdmin("root") {
		t.Error("expected explicit admin grant to allow admin access")
	}
}

func TestNonAdminCannotAdmin(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")
	if svc.CanAdmin("alice") {
		t.Error("user with no grants must not have admin access")
	}
}

// ---- bootstrap --------------------------------------------------------------

func TestEnsureBootstrapAdmin(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc

	res, err := svc.EnsureBootstrapAdmin()
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !res.Created {
		t.Fatal("expected an admin user to be created on an empty database")
	}
	if res.Username != "admin" || res.Password == "" {
		t.Fatalf("expected generated admin credentials, got %+v", res)
	}
	if !svc.CanAdmin("admin") {
		t.Error("bootstrap admin should have admin access")
	}
	if _, err := svc.Authenticate("admin", res.Password); err != nil {
		t.Errorf("generated password should authenticate: %v", err)
	}
}

func TestEnsureBootstrapAdminIsIdempotent(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	if _, err := svc.EnsureBootstrapAdmin(); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	res, err := svc.EnsureBootstrapAdmin()
	if err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if res.Created {
		t.Error("expected no user to be created on the second run")
	}
}

func TestEnsureBootstrapAdminRepairsExistingAdminRole(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	if err := svc.CreateRole(auth.RoleAdmin, "", nil); err != nil {
		t.Fatalf("create broken admin role: %v", err)
	}

	res, err := svc.EnsureBootstrapAdmin()
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !res.Created {
		t.Fatal("expected bootstrap to create an admin user")
	}
	if !svc.CanAdmin("admin") {
		t.Error("expected repaired admin role to grant admin access")
	}
	if _, err := svc.Authenticate("admin", res.Password); err != nil {
		t.Errorf("generated password should authenticate: %v", err)
	}
}

func TestEnsureBootstrapAdminRecoversWhenUsersExistButNoAdmins(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")

	res, err := svc.EnsureBootstrapAdmin()
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !res.Created {
		t.Fatal("expected bootstrap to create a recovery admin")
	}
	if !svc.CanAdmin("admin") {
		t.Error("expected recovery admin to have admin access")
	}
	if _, err := svc.Authenticate("admin", res.Password); err != nil {
		t.Errorf("generated password should authenticate: %v", err)
	}
}

// ---- user and role management ----------------------------------------------

func TestCreateUserRejectsShortPassword(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	if err := svc.CreateUser("alice", "short", nil); err == nil {
		t.Error("expected short passwords to be rejected")
	}
}

func TestCreateUserRejectsUnknownRole(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	if err := svc.CreateUser("alice", testPassword, []string{"nope"}); err == nil {
		t.Error("expected assignment of a nonexistent role to fail")
	}
}

func TestCreateDuplicateUser(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")
	if err := svc.CreateUser("alice", testPassword, nil); !errors.Is(err, auth.ErrExists) {
		t.Errorf("expected ErrExists, got %v", err)
	}
}

func TestSetUserRolesRejectsUnknownUser(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustRole(t, svc, "reader", grant("loki", "read", env.a))

	if err := svc.SetUserRoles("ghost", []string{"reader"}); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
	if _, ok := svc.TenantIDsFor("ghost", "loki", "read"); ok {
		t.Error("unknown user should not gain role grants")
	}
}

func TestDeleteUserRemovesGrants(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")
	if err := svc.SetUserGrants("alice", []auth.Grant{grant("loki", "read", env.a)}); err != nil {
		t.Fatalf("set grants: %v", err)
	}
	if err := svc.DeleteUser("alice"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.GetUser("alice"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
	if _, ok := svc.TenantIDsFor("alice", "loki", "read"); ok {
		t.Error("grants must not survive user deletion")
	}
}

func TestDeleteMissingUser(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	if err := svc.DeleteUser("ghost"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestSetPassword(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")

	const newPassword = "an-entirely-different-secret"
	if err := svc.SetPassword("alice", newPassword); err != nil {
		t.Fatalf("set password: %v", err)
	}
	if _, err := svc.Authenticate("alice", newPassword); err != nil {
		t.Errorf("new password should work: %v", err)
	}
	if _, err := svc.Authenticate("alice", testPassword); err == nil {
		t.Error("old password should no longer work")
	}
}

func TestDeleteRoleRevokesInheritedAccess(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustRole(t, svc, "logs-reader", grant("loki", "read", env.a))
	mustUser(t, svc, "alice", "logs-reader")

	if _, ok := svc.TenantIDsFor("alice", "loki", "read"); !ok {
		t.Fatal("precondition: alice should have access via the role")
	}
	if err := svc.DeleteRole("logs-reader"); err != nil {
		t.Fatalf("delete role: %v", err)
	}
	if _, ok := svc.TenantIDsFor("alice", "loki", "read"); ok {
		t.Error("deleting a role must revoke the access it granted")
	}
}

func TestGetUserReportsRolesAndGrants(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustRole(t, svc, "role-a", grant("loki", "read", env.a))
	mustRole(t, svc, "role-b", grant("mimir", "write", env.b))
	mustUser(t, svc, "alice", "role-a", "role-b")

	info, err := svc.GetUser("alice")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	sort.Strings(info.Roles)
	if len(info.Roles) != 2 || info.Roles[0] != "role-a" || info.Roles[1] != "role-b" {
		t.Errorf("expected [role-a role-b], got %v", info.Roles)
	}
	if info.Admin {
		t.Error("alice should not be reported as admin")
	}
}

func TestListRolesIncludesMembers(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustRole(t, svc, "logs-reader", grant("loki", "read", env.a))
	mustUser(t, svc, "alice", "logs-reader")
	mustUser(t, svc, "bob", "logs-reader")

	roles, err := svc.ListRoles()
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	var found bool
	for _, r := range roles {
		if r.Name != "logs-reader" {
			continue
		}
		found = true
		sort.Strings(r.Members)
		if len(r.Members) != 2 || r.Members[0] != "alice" || r.Members[1] != "bob" {
			t.Errorf("expected members [alice bob], got %v", r.Members)
		}
	}
	if !found {
		t.Error("expected logs-reader in the role list")
	}
}

// ---- grant validation -------------------------------------------------------

func TestGrantValidation(t *testing.T) {
	const uuidA = "6f1d2c9e-9d3a-4c1b-8f47-2b0a5e7c1d34"

	cases := []struct {
		name    string
		grant   auth.Grant
		wantErr bool
	}{
		{"valid read", grant("loki", "read", uuidA), false},
		{"valid mimir rules read", grant("mimir", "rules:read", uuidA), false},
		{"valid mimir rules write", grant("mimir", "rules:write", uuidA), false},
		{"valid mimir alerts read", grant("mimir", "alerts:read", uuidA), false},
		{"valid mimir alerts write", grant("mimir", "alerts:write", uuidA), false},
		{"valid wildcard", grant("*", "*", uuidA), false},
		{"valid admin", auth.Grant{Backend: "admin", Action: "access"}, false},
		{"unknown backend", grant("kafka", "read", uuidA), true},
		{"unknown action", grant("loki", "purge", uuidA), true},
		{"valid loki delete", grant("loki", "delete", uuidA), false},
		{"valid loki rules read", grant("loki", "rules:read", uuidA), false},
		{"valid loki rules write", grant("loki", "rules:write", uuidA), false},
		{"valid loki alerts read", grant("loki", "alerts:read", uuidA), false},
		// Loki has a ruler but no alertmanager, so there is no alert config to write.
		{"alerts write on loki", grant("loki", "alerts:write", uuidA), true},
		{"alerts action on tempo", grant("tempo", "alerts:write", uuidA), true},
		{"rules action on tempo", grant("tempo", "rules:read", uuidA), true},
		// A control action must name a concrete backend; the wildcard does not
		// implicitly confer rule or alert management.
		{"rules action on wildcard backend", grant("*", "rules:read", uuidA), true},
		{"loki rules write with multiple tenants", grant("loki", "rules:write", uuidA, "56f1bd96-55a2-4f34-9451-99eeccdd40d8"), true},
		{"loki rules read with multiple tenants", grant("loki", "rules:read", uuidA, "56f1bd96-55a2-4f34-9451-99eeccdd40d8"), false},
		{"rules read with multiple tenants", grant("mimir", "rules:read", uuidA, "56f1bd96-55a2-4f34-9451-99eeccdd40d8"), false},
		{"alerts read with multiple tenants", grant("mimir", "alerts:read", uuidA, "56f1bd96-55a2-4f34-9451-99eeccdd40d8"), false},
		{"rules write with multiple tenants", grant("mimir", "rules:write", uuidA, "56f1bd96-55a2-4f34-9451-99eeccdd40d8"), true},
		{"alerts write with multiple tenants", grant("mimir", "alerts:write", uuidA, "56f1bd96-55a2-4f34-9451-99eeccdd40d8"), true},
		{"no tenants", grant("loki", "read"), true},
		{"empty tenant", grant("loki", "read", ""), true},
		{"tenant is not a uuid", grant("loki", "read", "tenant-a"), true},
		{"tenant with separator", grant("loki", "read", "a|b"), true},
		{"admin with wrong action", auth.Grant{Backend: "admin", Action: "read"}, true},
		{"admin with tenants", auth.Grant{Backend: "admin", Action: "access", TenantIDs: []string{uuidA}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.grant.Validate()
			if tc.wantErr && err == nil {
				t.Error("expected an error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected no error, got %v", err)
			}
		})
	}
}

// ---- reload -----------------------------------------------------------------

func TestReloadPicksUpExternalChanges(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")

	// Simulate another replica deleting the user directly in the database.
	gormDB := serviceDB(t, svc)
	if err := gormDB.Where("name = ?", "alice").Delete(&db.User{}).Error; err != nil {
		t.Fatalf("external delete: %v", err)
	}
	if _, err := svc.Authenticate("alice", testPassword); err != nil {
		t.Fatal("expected the cached snapshot to still authenticate before reload")
	}
	if err := svc.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, err := svc.Authenticate("alice", testPassword); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Error("expected reload to drop the deleted user")
	}
}

// serviceDB opens a second handle to the same in-memory database as svc.
func serviceDB(t *testing.T, _ *auth.Service) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	gormDB, err := db.Open(db.Config{Type: "sqlite", Path: dsn})
	if err != nil {
		t.Fatalf("reopen test db: %v", err)
	}
	// Close it with the test. A shared-cache in-memory database lives as long as
	// any connection to it does, so leaking this handle keeps the database alive
	// past the test and the next `go test -count=N` iteration reopens a database
	// that still holds the previous run's rows.
	t.Cleanup(func() {
		if sqlDB, err := gormDB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return gormDB
}

func equalStrings(a, b []string) bool {
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

// ---- subject namespacing ----------------------------------------------------

// Casbin keeps all subjects in one namespace. Before subjects were prefixed by
// kind, a user and a role sharing a name were the same subject, so deleting the
// user stripped the role's grants from every other member -- and the bootstrap
// user "admin" collided with the built-in "admin" role, silently revoking admin
// access from everyone the moment that user was removed.
func TestUserAndRoleMayShareAName(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc

	mustRole(t, svc, "shared", grant("loki", "read", env.a))
	mustUser(t, svc, "shared") // same name, different kind
	mustUser(t, svc, "bob", "shared")

	if _, ok := svc.TenantIDsFor("bob", "loki", "read"); !ok {
		t.Fatal("precondition: bob should inherit the role's grant")
	}
	// The user "shared" holds no role, so it must have no access of its own.
	if _, ok := svc.TenantIDsFor("shared", "loki", "read"); ok {
		t.Error("the user must not pick up the same-named role's grants")
	}

	if err := svc.DeleteUser("shared"); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if _, ok := svc.TenantIDsFor("bob", "loki", "read"); !ok {
		t.Error("deleting a user must not strip a same-named role's grants from its members")
	}
}

func TestDeletingBootstrapAdminKeepsOtherAdmins(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc

	if _, err := svc.EnsureBootstrapAdmin(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := svc.CreateUser("second", testPassword, []string{auth.RoleAdmin}); err != nil {
		t.Fatalf("create second admin: %v", err)
	}
	if err := svc.DeleteUser("admin"); err != nil {
		t.Fatalf("delete admin: %v", err)
	}
	if !svc.CanAdmin("second") {
		t.Error("removing the bootstrap admin user must not revoke the admin role itself")
	}
}

// A grant naming an unregistered tenant is a bad request, not a missing
// resource, and must not be reported as ErrNotFound.
func TestGrantWithUnknownTenantIsInvalid(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")

	err := svc.SetUserGrants("alice", []auth.Grant{
		grant("loki", "read", "6f1d2c9e-9d3a-4c1b-8f47-2b0a5e7c1d34"),
	})
	if !errors.Is(err, auth.ErrInvalidGrant) {
		t.Errorf("expected ErrInvalidGrant, got %v", err)
	}
}

// TestDeleteGrantsAreSingleTenant pins that deletion never resolves to an
// ambiguous tenant, in either direction. Deleting data is irreversible, so a
// caller who cannot say which tenant they mean must not delete from any.
func TestDeleteGrantsAreSingleTenant(t *testing.T) {
	const uuidA = "6f1d2c9e-9d3a-4c1b-8f47-2b0a5e7c1d34"
	const uuidB = "56f1bd96-55a2-4f34-9451-99eeccdd40d8"

	for _, action := range []string{auth.ActionDelete} {
		t.Run(action+" single tenant", func(t *testing.T) {
			if err := grant("loki", action, uuidA).Validate(); err != nil {
				t.Errorf("expected a single-tenant delete grant to be valid: %v", err)
			}
		})
		t.Run(action+" multi tenant", func(t *testing.T) {
			if err := grant("loki", action, uuidA, uuidB).Validate(); err == nil {
				t.Error("expected a multi-tenant delete grant to be rejected")
			}
		})
		t.Run(action+" on tempo", func(t *testing.T) {
			if err := grant("tempo", action, uuidA).Validate(); err == nil {
				t.Error("expected a delete grant on tempo to be rejected")
			}
		})
	}
}

// ---- hashing concurrency ----------------------------------------------------

// The cap has to be enforced where bcrypt actually runs, or it bounds nothing.
//
// Contention is created by releasing many callers at once from a barrier rather
// than by racing one goroutine against a polling loop: with a single slot and no
// willingness to wait, whichever caller loses the race must be shed, and the
// test does not depend on how long any particular bcrypt takes.
func TestAuthenticateRespectsHashLimit(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")

	svc.SetHashLimit(1, 0)
	if got := svc.HashLimit(); got != 1 {
		t.Fatalf("HashLimit = %d, want 1", got)
	}

	const callers = 16
	var shed, checked atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.Authenticate("alice", "wrong-password-to-force-a-hash")
			switch {
			case errors.Is(err, auth.ErrHashLimitReached):
				shed.Add(1)
			case errors.Is(err, auth.ErrInvalidCredentials):
				checked.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if shed.Load() == 0 {
		t.Errorf("with one slot and no wait, %d concurrent callers all got through; the cap bounds nothing", callers)
	}
	if checked.Load() == 0 {
		t.Error("expected at least one caller to actually reach the credential check")
	}
}

// The reason the cache is consulted before the gate: a caller presenting a
// credential that was recently verified must not queue behind an attacker's
// hashing. Without this, a flood of bad credentials denies service to everyone
// holding good ones, which is the denial of service the gate exists to prevent.
func TestCachedCredentialBypassesHashLimit(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")

	// Prime the cache with a genuine verification.
	if _, err := svc.Authenticate("alice", testPassword); err != nil {
		t.Fatalf("priming call: %v", err)
	}

	// Now close the gate completely: no slots, no waiting.
	svc.SetHashLimit(1, 0)
	blocked := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(blocked)
		_, _ = svc.Authenticate("alice", "wrong-password-to-occupy-the-slot")
		close(done)
	}()
	<-blocked

	// While that slot is held, the cached credential must still resolve.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := svc.Authenticate("alice", testPassword); err != nil {
			t.Fatalf("a cached credential was refused while hashing was saturated: %v", err)
		}
		select {
		case <-done:
			return
		default:
		}
	}
	<-done
}

// A zero or negative cap means unlimited, which is how the feature is turned off.
func TestHashLimitCanBeDisabled(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")

	svc.SetHashLimit(0, 0)
	if got := svc.HashLimit(); got != 0 {
		t.Errorf("HashLimit = %d, want 0 for unlimited", got)
	}
	if _, err := svc.Authenticate("alice", testPassword); err != nil {
		t.Errorf("expected an unlimited gate to admit: %v", err)
	}
}

// Resizing while requests are in flight must not strand a slot: an in-flight
// caller releases to the gate it acquired from, not to whatever replaced it.
func TestSetHashLimitIsSafeWhileInFlight(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")
	svc.SetHashLimit(2, time.Second)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = svc.Authenticate("alice", "wrong-password")
		}()
	}
	for i := 0; i < 5; i++ {
		svc.SetHashLimit(1+i%3, time.Second)
		time.Sleep(2 * time.Millisecond)
	}
	wg.Wait()

	// The gate must still admit afterwards; a stranded slot would show up as a
	// permanent refusal here.
	svc.SetHashLimit(2, time.Second)
	if _, err := svc.Authenticate("alice", testPassword); err != nil {
		t.Errorf("gate was left in a broken state after resizing: %v", err)
	}
}

// ---- credential cache invalidation ------------------------------------------
//
// These pin the invariants that make the cache safe. The cache key binds the
// stored password hash, and the user snapshot is consulted before the cache, so
// invalidation is structural rather than something a caller must remember to
// trigger. Each of these must hold regardless of whether a reload happens to
// clear the cache.

// A rotated password must stop working the instant it is rotated, cached or not.
func TestPasswordChangeInvalidatesCachedCredential(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")

	// Prime the cache with a genuine verification.
	if _, err := svc.Authenticate("alice", testPassword); err != nil {
		t.Fatalf("priming call: %v", err)
	}

	const rotated = "a-completely-different-password"
	if err := svc.SetPassword("alice", rotated); err != nil {
		t.Fatalf("set password: %v", err)
	}

	if _, err := svc.Authenticate("alice", testPassword); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("the old password still authenticated after rotation: %v", err)
	}
	if _, err := svc.Authenticate("alice", rotated); err != nil {
		t.Errorf("the new password was rejected: %v", err)
	}
}

// Deleting a user must revoke them immediately, cached or not.
func TestDeletedUserCannotUseCachedCredential(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")

	if _, err := svc.Authenticate("alice", testPassword); err != nil {
		t.Fatalf("priming call: %v", err)
	}
	if err := svc.DeleteUser("alice"); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	if _, err := svc.Authenticate("alice", testPassword); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("a deleted user authenticated from cache: %v", err)
	}
}

// Recreating a name must not resurrect the old credential: bcrypt salts anew, so
// the hash differs and the old cache key cannot match.
func TestRecreatedUserDoesNotInheritCachedCredential(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")

	if _, err := svc.Authenticate("alice", testPassword); err != nil {
		t.Fatalf("priming call: %v", err)
	}
	if err := svc.DeleteUser("alice"); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if err := svc.CreateUser("alice", "a-brand-new-password", nil); err != nil {
		t.Fatalf("recreate user: %v", err)
	}

	if _, err := svc.Authenticate("alice", testPassword); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("the previous incarnation's password still worked: %v", err)
	}
	if _, err := svc.Authenticate("alice", "a-brand-new-password"); err != nil {
		t.Errorf("the new password was rejected: %v", err)
	}
}

// The cache covers authentication only. Authorization is resolved per request,
// so a revoked grant takes effect immediately even while the credential is warm.
func TestCachedCredentialDoesNotCacheAuthorization(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")
	if err := svc.SetUserGrants("alice", []auth.Grant{
		{Backend: "loki", Action: "read", TenantIDs: []string{env.a}},
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.Authenticate("alice", testPassword); err != nil {
		t.Fatalf("priming call: %v", err)
	}
	if _, ok := svc.AccessFor("alice", "loki", "read"); !ok {
		t.Fatal("expected the grant to be in force")
	}

	if err := svc.SetUserGrants("alice", nil); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// The credential is still cached and still valid...
	if _, err := svc.Authenticate("alice", testPassword); err != nil {
		t.Errorf("expected the credential to still authenticate: %v", err)
	}
	// ...but the revoked grant must not survive with it.
	if _, ok := svc.AccessFor("alice", "loki", "read"); ok {
		t.Error("a revoked grant was still authorized")
	}
}

// The behaviour change: a reload refreshes users and policy without evicting
// credentials that are still valid. Previously every reload -- one every 30
// seconds at the default interval -- put every caller back through bcrypt.
func TestCachedCredentialSurvivesReload(t *testing.T) {
	env := newTestEnv(t)
	svc := env.svc
	mustUser(t, svc, "alice")

	measure := func() time.Duration {
		start := time.Now()
		if _, err := svc.Authenticate("alice", testPassword); err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		return time.Since(start)
	}

	cold := measure()
	warm := measure()
	if warm > cold/4 {
		t.Fatalf("expected the second call to be served from cache: cold %v, warm %v", cold, warm)
	}

	if err := svc.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// The cache must still be warm. The bound is loose so the test cannot flake
	// on a busy machine, while still failing by orders of magnitude if the
	// reload has evicted the entry and the call pays bcrypt again.
	if after := measure(); after > cold/4 {
		t.Errorf("a reload evicted a still-valid credential: cold %v, after reload %v", cold, after)
	}
}
