// Package authtest provides an in-memory auth.Authorizer for tests that need
// to exercise HTTP handlers without standing up a database and policy store.
package authtest

import (
	"context"

	"obsdatalayer/internal/auth"
)

// Stub implements auth.Authorizer with fixed, in-memory answers.
type Stub struct {
	// Username and Password are the single credential pair that authenticates.
	Username string
	Password string
	// Tenants is returned for any backend/action in Allow.
	Tenants []string
	// LabelSelectors is returned for Mimir read requests in Allow.
	LabelSelectors []string
	// Allow lists "backend:action" pairs the user may perform. A nil Allow
	// permits every backend and action.
	Allow map[string]bool
	// Admin controls the result of CanAdmin.
	Admin bool
}

var _ auth.Authorizer = (*Stub)(nil)

// New returns a Stub for "testuser"/"testpass" with full data-plane access to
// tenant "test-tenant" and no admin rights.
func New() *Stub {
	return &Stub{
		Username: "testuser",
		Password: "testpass",
		Tenants:  []string{"test-tenant"},
	}
}

// NewAdmin returns a Stub like New but with admin-plane access.
func NewAdmin() *Stub {
	s := New()
	s.Admin = true
	return s
}

// Authenticate implements auth.Authorizer.
func (s *Stub) Authenticate(name, password string) (*auth.User, error) {
	if name != s.Username || password != s.Password {
		return nil, auth.ErrInvalidCredentials
	}
	return &auth.User{Name: name}, nil
}

// AuthenticateContext implements auth.Authorizer. The stub does no hashing, so
// there is no slot to wait for and the context is unused.
func (s *Stub) AuthenticateContext(_ context.Context, name, password string) (*auth.User, error) {
	return s.Authenticate(name, password)
}

// AccessFor implements auth.Authorizer.
func (s *Stub) AccessFor(name, backend, action string) (auth.Access, bool) {
	if name != s.Username {
		return auth.Access{}, false
	}
	if s.Allow != nil && !s.Allow[backend+":"+action] {
		return auth.Access{}, false
	}
	if len(s.Tenants) == 0 {
		return auth.Access{}, false
	}
	access := auth.Access{TenantIDs: s.Tenants}
	if backend == "mimir" && action == auth.ActionRead {
		access.LabelSelectors = s.LabelSelectors
	}
	return access, true
}

// CanAdmin implements auth.Authorizer.
func (s *Stub) CanAdmin(name string) bool {
	return s.Admin && name == s.Username
}

// Header returns the HTTP Basic Authorization value for the stub's credentials.
func (s *Stub) Header() string {
	return BasicHeader(s.Username, s.Password)
}
