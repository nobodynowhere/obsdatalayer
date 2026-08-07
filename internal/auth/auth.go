// Package auth implements bcrypt-file-based user authentication and
// policy-driven tenant ID assignment for the observability gateway.
package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// ---- types -----------------------------------------------------------------

// UserFile is the parsed contents of an auth YAML file.
type UserFile struct {
	Users []*User `yaml:"users"`
	// byName is populated by Load/New for O(1) lookup.
	byName map[string]*User
}

// User represents a single gateway credential with its access policies.
type User struct {
	Name           string   `yaml:"name"`
	PasswordBcrypt string   `yaml:"password_bcrypt"`
	Admin          bool     `yaml:"admin"`
	Policies       []Policy `yaml:"policies"`
}

// Policy grants access to one or more backends with specific actions,
// mapping to a set of tenant IDs injected as X-Scope-OrgID.
type Policy struct {
	// Backends lists backend names this policy applies to ("loki", "mimir",
	// "tempo") or ["*"] to match all backends.
	Backends []string `yaml:"backends"`
	// Actions lists the permitted operations: "read" and/or "write".
	Actions []string `yaml:"actions"`
	// TenantIDs are the X-Scope-OrgID values to inject. For write
	// operations only TenantIDs[0] is used; for reads all are joined with "|".
	TenantIDs []string `yaml:"tenant_ids"`
}

// ---- context ---------------------------------------------------------------

type contextKey struct{}

// RequestAuth carries the resolved auth result for a single HTTP request.
type RequestAuth struct {
	Username  string
	TenantIDs []string // resolved for this request's backend + action
	IsRead    bool
}

// WithRequestAuth returns a copy of ctx carrying ra.
func WithRequestAuth(ctx context.Context, ra *RequestAuth) context.Context {
	return context.WithValue(ctx, contextKey{}, ra)
}

// FromContext returns the RequestAuth stored in ctx, or nil if none.
func FromContext(ctx context.Context) *RequestAuth {
	ra, _ := ctx.Value(contextKey{}).(*RequestAuth)
	return ra
}

// ---- loading & validation --------------------------------------------------

var (
	validBackends = map[string]bool{"loki": true, "mimir": true, "tempo": true, "*": true}
	validActions  = map[string]bool{"read": true, "write": true}
)

// Load reads and validates an auth YAML file.
func Load(path string) (*UserFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auth: read %s: %w", path, err)
	}

	var uf UserFile
	if err := yaml.Unmarshal(data, &uf); err != nil {
		return nil, fmt.Errorf("auth: parse %s: %w", path, err)
	}

	if err := buildUserFile(&uf); err != nil {
		return nil, err
	}
	return &uf, nil
}

// New creates a validated UserFile from a slice of users without reading a file.
// Useful for testing and programmatic construction.
func New(users []*User) (*UserFile, error) {
	uf := &UserFile{Users: users}
	if err := buildUserFile(uf); err != nil {
		return nil, err
	}
	return uf, nil
}

// buildUserFile validates uf and populates the byName index.
func buildUserFile(uf *UserFile) error {
	if len(uf.Users) == 0 {
		return fmt.Errorf("auth: no users defined")
	}

	names := make(map[string]struct{}, len(uf.Users))
	for i, u := range uf.Users {
		if u.Name == "" {
			return fmt.Errorf("auth: user[%d] has no name", i)
		}
		if _, dup := names[u.Name]; dup {
			return fmt.Errorf("auth: duplicate user name %q", u.Name)
		}
		names[u.Name] = struct{}{}

		if u.PasswordBcrypt == "" {
			return fmt.Errorf("auth: user %q has no password_bcrypt", u.Name)
		}
		if !strings.HasPrefix(u.PasswordBcrypt, "$2") {
			return fmt.Errorf("auth: user %q password_bcrypt does not look like a bcrypt hash", u.Name)
		}

		for j, p := range u.Policies {
			for _, b := range p.Backends {
				if !validBackends[b] {
					return fmt.Errorf("auth: user %q policy[%d] has unknown backend %q", u.Name, j, b)
				}
			}
			for _, a := range p.Actions {
				if !validActions[a] {
					return fmt.Errorf("auth: user %q policy[%d] has unknown action %q", u.Name, j, a)
				}
			}
			if len(p.TenantIDs) == 0 {
				return fmt.Errorf("auth: user %q policy[%d] has no tenant_ids", u.Name, j)
			}
		}
	}

	uf.byName = make(map[string]*User, len(uf.Users))
	for _, u := range uf.Users {
		uf.byName[u.Name] = u
	}
	return nil
}

// ---- authentication --------------------------------------------------------

// ErrInvalidCredentials is returned by Authenticate on any auth failure.
// The same error is used regardless of whether the user exists or the
// password is wrong, to prevent username enumeration.
var ErrInvalidCredentials = errors.New("invalid credentials")

// Authenticate verifies username + plaintext password against stored bcrypt
// hashes. Returns ErrInvalidCredentials on any failure.
func Authenticate(uf *UserFile, name, password string) (*User, error) {
	u, ok := uf.byName[name]
	if !ok {
		// Run bcrypt anyway to prevent timing-based username enumeration.
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$invalidsaltinvalidsaltinvalid.."), []byte(password))
		return nil, ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordBcrypt), []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
	}
	return u, nil
}

// ---- policy matching -------------------------------------------------------

// TenantIDsFor returns the merged, deduplicated, sorted tenant IDs for the
// given backend and action across all matching policies. ok is false when no
// policy matches (caller should return 403).
func (u *User) TenantIDsFor(backend, action string) ([]string, bool) {
	seen := make(map[string]struct{})
	var ids []string
	for _, p := range u.Policies {
		if !p.matchesBackend(backend) || !p.matchesAction(action) {
			continue
		}
		for _, tid := range p.TenantIDs {
			if _, exists := seen[tid]; !exists {
				seen[tid] = struct{}{}
				ids = append(ids, tid)
			}
		}
	}
	if len(ids) == 0 {
		return nil, false
	}
	sort.Strings(ids)
	return ids, true
}

func (p *Policy) matchesBackend(backend string) bool {
	for _, b := range p.Backends {
		if b == "*" || b == backend {
			return true
		}
	}
	return false
}

func (p *Policy) matchesAction(action string) bool {
	for _, a := range p.Actions {
		if a == action {
			return true
		}
	}
	return false
}
