package auth_test

import (
	"testing"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/db"
)

func openTestAuthDB(t *testing.T) *gorm.DB {
	t.Helper()
	gormDB, err := db.Open(db.DSN{Type: "sqlite", DSN: "file::memory:?cache=shared"})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.Migrate(gormDB); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}
	return gormDB
}

func TestAuthDBRoundTrip(t *testing.T) {
	gormDB := openTestAuthDB(t)

	hash, err := bcrypt.GenerateFromPassword([]byte("testpass"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	uf, err := auth.New([]*auth.User{{
		Name:           "testuser",
		PasswordBcrypt: string(hash),
		Admin:          true,
		Policies: []auth.Policy{{
			Backends:  []string{"*"},
			Actions:   []string{"read", "write"},
			TenantIDs: []string{"test-tenant"},
		}},
	}})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	if err := auth.SaveUserFile(gormDB, uf); err != nil {
		t.Fatalf("SaveUserFile: %v", err)
	}

	holder, err := auth.NewAuthHolder(gormDB)
	if err != nil {
		t.Fatalf("NewAuthHolder: %v", err)
	}

	loaded := holder.Get()
	if loaded == nil {
		t.Fatal("holder.Get returned nil")
	}
	if len(loaded.Users) != 1 {
		t.Fatalf("expected 1 user, got %d", len(loaded.Users))
	}
	if loaded.Users[0].Name != "testuser" {
		t.Errorf("expected user testuser, got %q", loaded.Users[0].Name)
	}

	u, err := auth.Authenticate(loaded, "testuser", "testpass")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if u == nil {
		t.Fatal("expected user, got nil")
	}
}
