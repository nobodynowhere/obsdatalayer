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

// EnsureBootstrapAdmin creates or repairs the "admin" role and ensures at least
// one user can reach the admin plane.
//
// The role and recovery user are managed idempotently so an operator who deletes
// or strips every admin can still be bootstrapped again on the next start.
func (s *Service) EnsureBootstrapAdmin() (BootstrapResult, error) {
	if err := s.ensureAdminRole(); err != nil {
		return BootstrapResult{}, err
	}

	var count int64
	if err := s.db.Model(&dbstore.User{}).Count(&count).Error; err != nil {
		return BootstrapResult{}, fmt.Errorf("count users: %w", err)
	}
	if count > 0 {
		users, err := s.ListUsers()
		if err != nil {
			return BootstrapResult{}, fmt.Errorf("list users: %w", err)
		}
		for _, user := range users {
			if user.Admin {
				return BootstrapResult{}, nil
			}
		}
		return s.bootstrapAdminUser()
	}

	return s.bootstrapAdminUser()
}

func (s *Service) bootstrapAdminUser() (BootstrapResult, error) {
	password, err := generatePassword(24)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("generate password: %w", err)
	}

	if err := s.CreateUser("admin", password, []string{RoleAdmin}); err == nil {
		return BootstrapResult{Created: true, Username: "admin", Password: password}, nil
	} else if !errors.Is(err, ErrExists) {
		return BootstrapResult{}, fmt.Errorf("create admin user: %w", err)
	}

	if err := s.SetPassword("admin", password); err != nil {
		return BootstrapResult{}, fmt.Errorf("reset admin password: %w", err)
	}
	if err := s.SetUserRoles("admin", []string{RoleAdmin}); err != nil {
		return BootstrapResult{}, fmt.Errorf("grant admin role: %w", err)
	}
	return BootstrapResult{Created: true, Username: "admin", Password: password}, nil
}

// ensureAdminRole creates the admin role and its admin-plane grant if absent.
func (s *Service) ensureAdminRole() error {
	role, err := s.findRole(RoleAdmin)
	if err == nil {
		info, err := s.roleInfo(role)
		if err != nil {
			return err
		}
		if hasAdminGrant(info.Grants) {
			return nil
		}
		grants := []Grant{{Backend: ObjectAdmin, Action: ActionAccess}}
		if err := s.SetRoleGrants(RoleAdmin, grants); err != nil {
			return fmt.Errorf("repair admin role: %w", err)
		}
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

func hasAdminGrant(grants []Grant) bool {
	for _, g := range grants {
		if g.Backend == ObjectAdmin && g.Action == ActionAccess {
			return true
		}
	}
	return false
}

func generatePassword(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
