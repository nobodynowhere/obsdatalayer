package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"gorm.io/gorm"

	dbstore "obsdatalayer/internal/db"
)

// BootstrapResult reports what EnsureBootstrapAdmin did.
type BootstrapResult struct {
	Created  bool
	Username string
	Password string
}

// EnsureBootstrapAdmin creates the "admin" role and, if the database has no
// users at all, an initial admin account with a generated password.
//
// The role is created idempotently so an operator who deletes every user can
// still be bootstrapped again on the next start.
func (s *Service) EnsureBootstrapAdmin() (BootstrapResult, error) {
	if err := s.ensureAdminRole(); err != nil {
		return BootstrapResult{}, err
	}

	var count int64
	if err := s.db.Model(&dbstore.User{}).Count(&count).Error; err != nil {
		return BootstrapResult{}, fmt.Errorf("count users: %w", err)
	}
	if count > 0 {
		return BootstrapResult{}, nil
	}

	password, err := generatePassword(24)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("generate password: %w", err)
	}

	if err := s.CreateUser("admin", password, []string{RoleAdmin}); err != nil {
		// Another replica won the race; that is a success for our purposes.
		if errors.Is(err, ErrExists) {
			return BootstrapResult{}, nil
		}
		return BootstrapResult{}, fmt.Errorf("create admin user: %w", err)
	}

	return BootstrapResult{Created: true, Username: "admin", Password: password}, nil
}

// ensureAdminRole creates the admin role and its admin-plane grant if absent.
func (s *Service) ensureAdminRole() error {
	_, err := s.findRole(RoleAdmin)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	grants := []Grant{{Backend: ObjectAdmin, Action: ActionAccess}}
	if err := s.CreateRole(RoleAdmin, "Full administrative access to the gateway admin API", grants); err != nil {
		if errors.Is(err, ErrExists) || errors.Is(err, gorm.ErrDuplicatedKey) {
			return nil
		}
		return fmt.Errorf("create admin role: %w", err)
	}
	return nil
}

func generatePassword(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
