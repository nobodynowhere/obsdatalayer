package tenant_test

import (
	"errors"
	"fmt"
	"testing"

	"obsdatalayer/internal/db"
	"obsdatalayer/internal/tenant"
)

func newStore(t *testing.T) *tenant.Store {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	gormDB, err := db.Open(db.DSN{Type: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(gormDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gormDB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	store, err := tenant.NewStore(gormDB)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return store
}

func intp(v int) *int { return &v }

func TestCreateGeneratesUUID(t *testing.T) {
	store := newStore(t)

	created, err := store.Create("", "acme", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected a generated UUID")
	}
	if created.GrafanaID != nil {
		t.Errorf("expected grafana_id to be unset, got %v", *created.GrafanaID)
	}
	if !store.Exists(created.ID) {
		t.Error("expected the tenant to be registered")
	}
}

func TestCreateWithExplicitUUID(t *testing.T) {
	store := newStore(t)
	const id = "6f1d2c9e-9d3a-4c1b-8f47-2b0a5e7c1d34"

	created, err := store.Create(id, "acme", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID != id {
		t.Errorf("expected %s, got %s", id, created.ID)
	}
}

func TestCreateRejectsBadUUID(t *testing.T) {
	store := newStore(t)
	if _, err := store.Create("not-a-uuid", "acme", nil); err == nil {
		t.Error("expected an error for a malformed UUID")
	}
}

func TestCreateRejectsEmptyName(t *testing.T) {
	store := newStore(t)
	if _, err := store.Create("", "   ", nil); err == nil {
		t.Error("expected an error for a blank name")
	}
}

func TestCreateDuplicateName(t *testing.T) {
	store := newStore(t)
	if _, err := store.Create("", "acme", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.Create("", "acme", nil); !errors.Is(err, tenant.ErrExists) {
		t.Errorf("expected ErrExists, got %v", err)
	}
}

// GrafanaID is nullable precisely so that tenants without one do not collide on
// the unique index. Two unassigned tenants must coexist.
func TestMultipleTenantsWithoutGrafanaID(t *testing.T) {
	store := newStore(t)
	if _, err := store.Create("", "acme", nil); err != nil {
		t.Fatalf("create acme: %v", err)
	}
	if _, err := store.Create("", "globex", nil); err != nil {
		t.Fatalf("create globex: %v", err)
	}
	if len(store.List()) != 2 {
		t.Errorf("expected 2 tenants, got %d", len(store.List()))
	}
}

func TestDuplicateGrafanaIDRejected(t *testing.T) {
	store := newStore(t)
	if _, err := store.Create("", "acme", intp(7)); err != nil {
		t.Fatalf("create acme: %v", err)
	}
	if _, err := store.Create("", "globex", intp(7)); !errors.Is(err, tenant.ErrExists) {
		t.Errorf("expected ErrExists for a duplicate grafana_id, got %v", err)
	}
}

func TestGrafanaIDMustBePositive(t *testing.T) {
	store := newStore(t)
	if _, err := store.Create("", "acme", intp(0)); err == nil {
		t.Error("expected zero grafana_id to be rejected")
	}
	if _, err := store.Create("", "globex", intp(-1)); err == nil {
		t.Error("expected negative grafana_id to be rejected")
	}
}

func TestResolveByNameAndUUID(t *testing.T) {
	store := newStore(t)
	created, err := store.Create("", "acme", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	byName, err := store.Resolve("acme")
	if err != nil {
		t.Fatalf("resolve by name: %v", err)
	}
	if byName != created.ID {
		t.Errorf("expected %s, got %s", created.ID, byName)
	}

	byID, err := store.Resolve(created.ID)
	if err != nil {
		t.Fatalf("resolve by uuid: %v", err)
	}
	if byID != created.ID {
		t.Errorf("expected %s, got %s", created.ID, byID)
	}

	if _, err := store.Resolve("nope"); !errors.Is(err, tenant.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// The UUID is what has already been written into grants and sent upstream, so
// a rename must not change it.
func TestUpdateKeepsUUIDStable(t *testing.T) {
	store := newStore(t)
	created, err := store.Create("", "acme", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	updated, err := store.Update(created.ID, "acme-renamed", intp(12))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.ID != created.ID {
		t.Errorf("UUID must be immutable: was %s, now %s", created.ID, updated.ID)
	}
	if updated.Name != "acme-renamed" {
		t.Errorf("expected renamed, got %q", updated.Name)
	}
	if updated.GrafanaID == nil || *updated.GrafanaID != 12 {
		t.Errorf("expected grafana_id 12, got %v", updated.GrafanaID)
	}
	if _, ok := store.ByName("acme"); ok {
		t.Error("old name should no longer resolve")
	}
}

func TestUpdateMissing(t *testing.T) {
	store := newStore(t)
	if _, err := store.Update("6f1d2c9e-9d3a-4c1b-8f47-2b0a5e7c1d34", "x", nil); !errors.Is(err, tenant.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestDelete(t *testing.T) {
	store := newStore(t)
	created, err := store.Create("", "acme", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.Delete(created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if store.Exists(created.ID) {
		t.Error("expected the tenant to be gone")
	}
	if err := store.Delete(created.ID); !errors.Is(err, tenant.ErrNotFound) {
		t.Errorf("expected ErrNotFound on second delete, got %v", err)
	}
}

func TestValidateAllNamesMissingRefs(t *testing.T) {
	store := newStore(t)
	created, err := store.Create("", "acme", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := store.ValidateAll([]string{created.ID, ""}); err != nil {
		t.Errorf("expected known ids and blanks to pass, got %v", err)
	}

	err = store.ValidateAll([]string{created.ID, "missing-one", "missing-two"})
	if err == nil {
		t.Fatal("expected an error naming the unknown tenants")
	}
	if !errors.Is(err, tenant.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
	for _, want := range []string{"missing-one", "missing-two"} {
		if !contains(err.Error(), want) {
			t.Errorf("expected error to name %q, got %v", want, err)
		}
	}
}

func TestEnsureTenantIsIdempotent(t *testing.T) {
	store := newStore(t)
	if err := store.EnsureTenant("", "acme", nil); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if err := store.EnsureTenant("", "acme", nil); err != nil {
		t.Fatalf("second ensure should be a no-op: %v", err)
	}
	if len(store.List()) != 1 {
		t.Errorf("expected 1 tenant, got %d", len(store.List()))
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
