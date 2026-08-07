package auth_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"obsdatalayer/internal/auth"
)

// ---- helpers ---------------------------------------------------------------

func writeAuthFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	return path
}

func hashpw(t *testing.T, password string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	return string(h)
}

// newTestUserFile creates an *auth.UserFile directly (no file I/O) with a
// single user "alice"/"secret" that has full wildcard access to tenant-a.
func newTestUserFile(t *testing.T) *auth.UserFile {
	t.Helper()
	hash := hashpw(t, "secret")
	uf, err := auth.New([]*auth.User{{
		Name:           "alice",
		PasswordBcrypt: hash,
		Policies: []auth.Policy{{
			Backends:  []string{"*"},
			Actions:   []string{"read", "write"},
			TenantIDs: []string{"tenant-a"},
		}},
	}})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	return uf
}

// ---- Load tests ------------------------------------------------------------

func TestLoadValidFile(t *testing.T) {
	hash := hashpw(t, "secret")
	yaml := fmt.Sprintf(`users:
  - name: alice
    password_bcrypt: %s
    policies:
      - backends: ["loki"]
        actions: ["read"]
        tenant_ids: ["tenant-a"]
`, hash)
	path := writeAuthFile(t, yaml)

	uf, err := auth.Load(path)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(uf.Users) != 1 {
		t.Fatalf("expected 1 user, got %d", len(uf.Users))
	}
	if uf.Users[0].Name != "alice" {
		t.Errorf("expected name 'alice', got %q", uf.Users[0].Name)
	}
}

func TestLoadEmptyUsers(t *testing.T) {
	path := writeAuthFile(t, "users: []\n")
	_, err := auth.Load(path)
	if err == nil {
		t.Fatal("expected error for empty users list")
	}
}

func TestLoadNoName(t *testing.T) {
	hash := hashpw(t, "secret")
	yaml := fmt.Sprintf(`users:
  - password_bcrypt: %s
    policies:
      - backends: ["loki"]
        actions: ["read"]
        tenant_ids: ["t"]
`, hash)
	path := writeAuthFile(t, yaml)
	_, err := auth.Load(path)
	if err == nil {
		t.Fatal("expected error for user with no name")
	}
}

func TestLoadDuplicateName(t *testing.T) {
	hash := hashpw(t, "secret")
	yaml := fmt.Sprintf(`users:
  - name: alice
    password_bcrypt: %s
    policies:
      - backends: ["loki"]
        actions: ["read"]
        tenant_ids: ["t"]
  - name: alice
    password_bcrypt: %s
    policies:
      - backends: ["loki"]
        actions: ["read"]
        tenant_ids: ["t"]
`, hash, hash)
	path := writeAuthFile(t, yaml)
	_, err := auth.Load(path)
	if err == nil {
		t.Fatal("expected error for duplicate user name")
	}
}

func TestLoadNoBcryptHash(t *testing.T) {
	yaml := `users:
  - name: alice
    policies:
      - backends: ["loki"]
        actions: ["read"]
        tenant_ids: ["t"]
`
	path := writeAuthFile(t, yaml)
	_, err := auth.Load(path)
	if err == nil {
		t.Fatal("expected error for missing password_bcrypt")
	}
}

func TestLoadInvalidBcryptPrefix(t *testing.T) {
	yaml := `users:
  - name: alice
    password_bcrypt: "notabcrypt"
    policies:
      - backends: ["loki"]
        actions: ["read"]
        tenant_ids: ["t"]
`
	path := writeAuthFile(t, yaml)
	_, err := auth.Load(path)
	if err == nil {
		t.Fatal("expected error for invalid bcrypt prefix")
	}
}

func TestLoadUnknownBackend(t *testing.T) {
	hash := hashpw(t, "secret")
	yaml := fmt.Sprintf(`users:
  - name: alice
    password_bcrypt: %s
    policies:
      - backends: ["kafka"]
        actions: ["read"]
        tenant_ids: ["t"]
`, hash)
	path := writeAuthFile(t, yaml)
	_, err := auth.Load(path)
	if err == nil {
		t.Fatal("expected error for unknown backend")
	}
}

func TestLoadUnknownAction(t *testing.T) {
	hash := hashpw(t, "secret")
	yaml := fmt.Sprintf(`users:
  - name: alice
    password_bcrypt: %s
    policies:
      - backends: ["loki"]
        actions: ["delete"]
        tenant_ids: ["t"]
`, hash)
	path := writeAuthFile(t, yaml)
	_, err := auth.Load(path)
	if err == nil {
		t.Fatal("expected error for unknown action")
	}
}

func TestLoadEmptyTenantIDs(t *testing.T) {
	hash := hashpw(t, "secret")
	yaml := fmt.Sprintf(`users:
  - name: alice
    password_bcrypt: %s
    policies:
      - backends: ["loki"]
        actions: ["read"]
        tenant_ids: []
`, hash)
	path := writeAuthFile(t, yaml)
	_, err := auth.Load(path)
	if err == nil {
		t.Fatal("expected error for empty tenant_ids")
	}
}

func TestLoadFileNotFound(t *testing.T) {
	_, err := auth.Load("/nonexistent/auth.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// ---- New tests -------------------------------------------------------------

func TestNewValid(t *testing.T) {
	_ = newTestUserFile(t) // panics on error
}

func TestNewEmptyUsers(t *testing.T) {
	_, err := auth.New(nil)
	if err == nil {
		t.Fatal("expected error for nil users")
	}
}

// ---- Authenticate tests ----------------------------------------------------

func TestAuthenticateSuccess(t *testing.T) {
	uf := newTestUserFile(t)
	user, err := auth.Authenticate(uf, "alice", "secret")
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if user.Name != "alice" {
		t.Errorf("expected name 'alice', got %q", user.Name)
	}
}

func TestAuthenticateWrongPassword(t *testing.T) {
	uf := newTestUserFile(t)
	_, err := auth.Authenticate(uf, "alice", "wrongpassword")
	if err != auth.ErrInvalidCredentials {
		t.Errorf("expected ErrInvalidCredentials, got: %v", err)
	}
}

func TestAuthenticateUnknownUser(t *testing.T) {
	uf := newTestUserFile(t)
	_, err := auth.Authenticate(uf, "bob", "secret")
	if err != auth.ErrInvalidCredentials {
		t.Errorf("expected ErrInvalidCredentials for unknown user, got: %v", err)
	}
}

// ---- TenantIDsFor tests ----------------------------------------------------

func TestTenantIDsForSpecificBackend(t *testing.T) {
	hash := hashpw(t, "pass")
	uf, err := auth.New([]*auth.User{{
		Name:           "alice",
		PasswordBcrypt: hash,
		Policies: []auth.Policy{
			{Backends: []string{"loki"}, Actions: []string{"read"}, TenantIDs: []string{"loki-tenant"}},
			{Backends: []string{"mimir"}, Actions: []string{"read"}, TenantIDs: []string{"mimir-tenant"}},
		},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	u := uf.Users[0]

	ids, ok := u.TenantIDsFor("loki", "read")
	if !ok {
		t.Fatal("expected ok=true for loki read")
	}
	if len(ids) != 1 || ids[0] != "loki-tenant" {
		t.Errorf("expected [loki-tenant], got %v", ids)
	}

	ids, ok = u.TenantIDsFor("mimir", "read")
	if !ok {
		t.Fatal("expected ok=true for mimir read")
	}
	if len(ids) != 1 || ids[0] != "mimir-tenant" {
		t.Errorf("expected [mimir-tenant], got %v", ids)
	}
}

func TestTenantIDsForWildcardBackend(t *testing.T) {
	hash := hashpw(t, "pass")
	uf, err := auth.New([]*auth.User{{
		Name:           "alice",
		PasswordBcrypt: hash,
		Policies: []auth.Policy{
			{Backends: []string{"*"}, Actions: []string{"read", "write"}, TenantIDs: []string{"all-tenant"}},
		},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	u := uf.Users[0]

	for _, backend := range []string{"loki", "mimir", "tempo"} {
		ids, ok := u.TenantIDsFor(backend, "read")
		if !ok {
			t.Errorf("expected ok=true for backend %q", backend)
			continue
		}
		if len(ids) != 1 || ids[0] != "all-tenant" {
			t.Errorf("backend %q: expected [all-tenant], got %v", backend, ids)
		}
	}
}

func TestTenantIDsForNoMatch(t *testing.T) {
	hash := hashpw(t, "pass")
	uf, err := auth.New([]*auth.User{{
		Name:           "alice",
		PasswordBcrypt: hash,
		Policies: []auth.Policy{
			{Backends: []string{"loki"}, Actions: []string{"read"}, TenantIDs: []string{"t"}},
		},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	u := uf.Users[0]

	_, ok := u.TenantIDsFor("loki", "write")
	if ok {
		t.Error("expected ok=false for loki write (not in policy)")
	}

	_, ok = u.TenantIDsFor("mimir", "read")
	if ok {
		t.Error("expected ok=false for mimir read (not in policy)")
	}
}

func TestTenantIDsForMultiplePoliciesMerged(t *testing.T) {
	hash := hashpw(t, "pass")
	uf, err := auth.New([]*auth.User{{
		Name:           "alice",
		PasswordBcrypt: hash,
		Policies: []auth.Policy{
			{Backends: []string{"loki"}, Actions: []string{"read"}, TenantIDs: []string{"tenant-a", "tenant-b"}},
			{Backends: []string{"loki"}, Actions: []string{"read"}, TenantIDs: []string{"tenant-b", "tenant-c"}},
		},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	u := uf.Users[0]

	ids, ok := u.TenantIDsFor("loki", "read")
	if !ok {
		t.Fatal("expected ok=true")
	}
	// Sorted and deduplicated: [tenant-a, tenant-b, tenant-c]
	if len(ids) != 3 {
		t.Fatalf("expected 3 unique tenant IDs, got %v", ids)
	}
	if ids[0] != "tenant-a" || ids[1] != "tenant-b" || ids[2] != "tenant-c" {
		t.Errorf("expected sorted [tenant-a, tenant-b, tenant-c], got %v", ids)
	}
}

func TestTenantIDsForSorted(t *testing.T) {
	hash := hashpw(t, "pass")
	uf, err := auth.New([]*auth.User{{
		Name:           "alice",
		PasswordBcrypt: hash,
		Policies: []auth.Policy{
			{Backends: []string{"loki"}, Actions: []string{"read"}, TenantIDs: []string{"zzz", "aaa", "mmm"}},
		},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	u := uf.Users[0]

	ids, ok := u.TenantIDsFor("loki", "read")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if ids[0] != "aaa" || ids[1] != "mmm" || ids[2] != "zzz" {
		t.Errorf("expected sorted [aaa, mmm, zzz], got %v", ids)
	}
}
