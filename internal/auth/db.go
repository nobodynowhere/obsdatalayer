package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/gofrs/uuid/v5"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	dbstore "obsdatalayer/internal/db"
)

// LoadFromDB loads all users and policies from the database.
func LoadFromDB(db *gorm.DB) (*UserFile, error) {
	var dbUsers []dbstore.User
	if err := db.
		Preload("Policies.Backends").
		Preload("Policies.Actions").
		Preload("Policies.TenantIDs").
		Find(&dbUsers).Error; err != nil {
		return nil, fmt.Errorf("auth: load users: %w", err)
	}

	users := make([]*User, len(dbUsers))
	for i, u := range dbUsers {
		policies := make([]Policy, len(u.Policies))
		for j, p := range u.Policies {
			backends := make([]string, len(p.Backends))
			for k, b := range p.Backends {
				backends[k] = b.Backend
			}
			actions := make([]string, len(p.Actions))
			for k, a := range p.Actions {
				actions[k] = a.Action
			}
			tenantIDs := make([]string, len(p.TenantIDs))
			for k, t := range p.TenantIDs {
				tenantIDs[k] = t.TenantID
			}
			policies[j] = Policy{
				Backends:  backends,
				Actions:   actions,
				TenantIDs: tenantIDs,
			}
		}
		users[i] = &User{
			Name:           u.Name,
			PasswordBcrypt: u.PasswordBcrypt,
			Admin:          u.Admin,
			Policies:       policies,
		}
	}

	return New(users)
}

// SaveUserFile writes every user in uf into the database.
func SaveUserFile(db *gorm.DB, uf *UserFile) error {
	for _, u := range uf.Users {
		if err := SaveUser(db, u); err != nil {
			return err
		}
	}
	return nil
}

// SaveUser writes a single user and its policies into the database.
func SaveUser(db *gorm.DB, u *User) error {
	userID, err := uuid.NewV4()
	if err != nil {
		return fmt.Errorf("generate user id: %w", err)
	}

	return db.Transaction(func(tx *gorm.DB) error {
		dbUser := dbstore.User{
			ID:             userID,
			Name:           u.Name,
			PasswordBcrypt: u.PasswordBcrypt,
			Admin:          u.Admin,
		}
		if err := tx.Create(&dbUser).Error; err != nil {
			return fmt.Errorf("create user %q: %w", u.Name, err)
		}

		for _, p := range u.Policies {
			if err := savePolicy(tx, userID, &p); err != nil {
				return err
			}
		}
		return nil
	})
}

func savePolicy(tx *gorm.DB, userID uuid.UUID, p *Policy) error {
	policyID, err := uuid.NewV4()
	if err != nil {
		return fmt.Errorf("generate policy id: %w", err)
	}
	dbPolicy := dbstore.Policy{
		ID:     policyID,
		UserID: userID,
	}
	if err := tx.Create(&dbPolicy).Error; err != nil {
		return fmt.Errorf("create policy: %w", err)
	}

	for _, b := range p.Backends {
		if err := savePolicyBackend(tx, policyID, b); err != nil {
			return err
		}
	}
	for _, a := range p.Actions {
		if err := savePolicyAction(tx, policyID, a); err != nil {
			return err
		}
	}
	for _, t := range p.TenantIDs {
		if err := savePolicyTenantID(tx, policyID, t); err != nil {
			return err
		}
	}

	return nil
}

func savePolicyBackend(tx *gorm.DB, policyID uuid.UUID, backend string) error {
	id, err := uuid.NewV4()
	if err != nil {
		return fmt.Errorf("generate backend id: %w", err)
	}
	b := dbstore.PolicyBackend{ID: id, PolicyID: policyID, Backend: backend}
	if err := tx.Create(&b).Error; err != nil {
		return fmt.Errorf("create policy backend: %w", err)
	}
	return nil
}

func savePolicyAction(tx *gorm.DB, policyID uuid.UUID, action string) error {
	id, err := uuid.NewV4()
	if err != nil {
		return fmt.Errorf("generate action id: %w", err)
	}
	a := dbstore.PolicyAction{ID: id, PolicyID: policyID, Action: action}
	if err := tx.Create(&a).Error; err != nil {
		return fmt.Errorf("create policy action: %w", err)
	}
	return nil
}

func savePolicyTenantID(tx *gorm.DB, policyID uuid.UUID, tenantID string) error {
	id, err := uuid.NewV4()
	if err != nil {
		return fmt.Errorf("generate tenant id: %w", err)
	}
	t := dbstore.PolicyTenantID{ID: id, PolicyID: policyID, TenantID: tenantID}
	if err := tx.Create(&t).Error; err != nil {
		return fmt.Errorf("create policy tenant id: %w", err)
	}
	return nil
}

// EnsureAdminUser creates an initial admin user if no users exist in the database.
// It returns true and the generated credentials when a user is created.
func EnsureAdminUser(db *gorm.DB) (created bool, username, password string, err error) {
	var count int64
	if err := db.Model(&dbstore.User{}).Count(&count).Error; err != nil {
		return false, "", "", fmt.Errorf("count users: %w", err)
	}
	if count > 0 {
		return false, "", "", nil
	}

	password, err = generatePassword(24)
	if err != nil {
		return false, "", "", fmt.Errorf("generate password: %w", err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return false, "", "", fmt.Errorf("hash password: %w", err)
	}

	admin := &User{
		Name:           "admin",
		PasswordBcrypt: string(hash),
		Admin:          true,
		Policies: []Policy{{
			Backends:  []string{"*"},
			Actions:   []string{"read", "write"},
			TenantIDs: []string{"admin"},
		}},
	}

	if err := SaveUser(db, admin); err != nil {
		// If another replica won the race, treat as not created.
		if strings.Contains(err.Error(), "duplicate") || errors.Is(err, gorm.ErrDuplicatedKey) {
			return false, "", "", nil
		}
		return false, "", "", fmt.Errorf("save admin user: %w", err)
	}

	return true, admin.Name, password, nil
}

func generatePassword(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
