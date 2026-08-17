package auth_test

import (
	"errors"
	"fmt"
	"sort"
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
	gormDB, err := db.Open(db.DSN{Type: "sqlite", DSN: dsn})
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
		{"valid wildcard", grant("*", "*", uuidA), false},
		{"valid admin", auth.Grant{Backend: "admin", Action: "access"}, false},
		{"unknown backend", grant("kafka", "read", uuidA), true},
		{"unknown action", grant("loki", "delete", uuidA), true},
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
	gormDB, err := db.Open(db.DSN{Type: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("reopen test db: %v", err)
	}
	return gormDB
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
