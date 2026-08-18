// Package auth implements gateway authentication (bcrypt credentials) and
// authorization (Casbin RBAC). Casbin is the single source of truth for
// permissions: a policy rule carries the subject, the backend it applies to,
// the action, and the tenant IDs to inject as X-Scope-OrgID.
package auth

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gofrs/uuid/v5"
	"golang.org/x/crypto/bcrypt"
)

// Authorization vocabulary.
//
// Data-plane objects are backend names; the admin plane is a distinct object so
// that a wildcard data grant can never confer administrative access.
const (
	BackendAny = "*"

	ActionRead  = "read"
	ActionWrite = "write"
	ActionAny   = "*"

	// ObjectAdmin / ActionAccess gate the admin listener.
	ObjectAdmin  = "admin"
	ActionAccess = "access"

	// RoleAdmin is the bootstrap role granted to the initial admin user.
	RoleAdmin = "admin"

	// tenantSep separates tenant IDs inside a single Casbin policy field. It
	// matches the X-Scope-OrgID multi-tenant separator.
	tenantSep = "|"
)

var (
	validBackends = map[string]bool{"loki": true, "mimir": true, "tempo": true, BackendAny: true}
	validActions  = map[string]bool{ActionRead: true, ActionWrite: true, ActionAny: true}
)

// User is a gateway account. The password hash is never serialized.
type User struct {
	Name           string `json:"name"`
	PasswordBcrypt string `json:"-"`
}

// Grant is one authorization rule: an action on a backend, plus the tenant IDs
// injected upstream when it matches.
type Grant struct {
	Backend           string   `json:"backend"`
	Action            string   `json:"action"`
	TenantIDs         []string `json:"tenant_ids,omitempty"`
	ReadLabelSelector string   `json:"read_label_selector,omitempty"`
}

// IsAdmin reports whether g grants admin-plane access rather than data access.
func (g Grant) IsAdmin() bool {
	return g.Backend == ObjectAdmin
}

// Validate checks a grant before it is written to the policy store.
func (g Grant) Validate() error {
	if g.IsAdmin() {
		if g.Action != ActionAccess {
			return fmt.Errorf("admin grant must use action %q, got %q", ActionAccess, g.Action)
		}
		if len(g.TenantIDs) > 0 {
			return errors.New("admin grant must not carry tenant_ids")
		}
		if strings.TrimSpace(g.ReadLabelSelector) != "" {
			return errors.New("admin grant must not carry read_label_selector")
		}
		return nil
	}
	if !validBackends[g.Backend] {
		return fmt.Errorf("unknown backend %q (must be loki, mimir, tempo, admin or *)", g.Backend)
	}
	if !validActions[g.Action] {
		return fmt.Errorf("unknown action %q (must be read, write or *)", g.Action)
	}
	if len(g.TenantIDs) == 0 {
		return fmt.Errorf("grant on backend %q action %q has no tenant_ids", g.Backend, g.Action)
	}
	if (g.Action == ActionWrite || g.Action == ActionAny) && len(g.TenantIDs) != 1 {
		return fmt.Errorf("grant on backend %q action %q must carry exactly one tenant_id", g.Backend, g.Action)
	}
	if selector := strings.TrimSpace(g.ReadLabelSelector); selector != "" {
		if g.Backend != "mimir" || g.Action != ActionRead {
			return errors.New("read_label_selector is only supported on mimir read grants")
		}
	}
	for _, t := range g.TenantIDs {
		if strings.TrimSpace(t) == "" {
			return errors.New("tenant_ids must not contain empty values")
		}
		// Grants reference tenants by UUID only. Names are resolved to UUIDs
		// before they ever reach a policy, so that renaming a tenant cannot
		// repoint an existing grant.
		if _, err := uuid.FromString(t); err != nil {
			return fmt.Errorf("tenant id %q is not a valid UUID: %w", t, err)
		}
	}
	return nil
}

// ---- request context --------------------------------------------------------

type contextKey struct{}

// RequestAuth carries the resolved auth result for a single HTTP request.
type RequestAuth struct {
	Username       string
	TenantIDs      []string // resolved for this request's backend + action
	LabelSelectors []string // resolved Mimir read policy selectors, if any
	IsRead         bool
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

// ---- credentials ------------------------------------------------------------

// ErrInvalidCredentials is returned for any authentication failure. The same
// error is used whether the user is unknown or the password is wrong.
var ErrInvalidCredentials = errors.New("invalid credentials")

// dummyHash is a real bcrypt hash at the same cost as stored credentials. It is
// compared against when the username does not exist so that the response time
// for an unknown user matches that of a known one.
//
// This must be a well-formed hash: bcrypt rejects a malformed one during
// parsing, before doing any key stretching, which would leave the unknown-user
// path orders of magnitude faster and make usernames trivially enumerable.
var dummyHash []byte

func init() {
	h, err := bcrypt.GenerateFromPassword([]byte("obsgateway-timing-equalizer"), bcrypt.DefaultCost)
	if err != nil {
		panic("auth: cannot generate dummy bcrypt hash: " + err.Error())
	}
	dummyHash = h
}

// HashPassword returns a bcrypt hash suitable for storage.
func HashPassword(password string) (string, error) {
	if len(password) < 12 {
		return "", errors.New("password must be at least 12 characters")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(h), nil
}

// ---- policy matching helpers ------------------------------------------------

// objectMatches reports whether a policy object covers the requested object.
// The "*" wildcard deliberately excludes the admin plane, so a broad data-plane
// grant can never escalate into administrative access.
func objectMatches(policyObj, requested string) bool {
	if policyObj == requested {
		return true
	}
	return policyObj == BackendAny && requested != ObjectAdmin
}

func actionMatches(policyAct, requested string) bool {
	return policyAct == ActionAny || policyAct == requested
}

// mergeTenants returns the sorted, deduplicated union of ids.
func mergeTenants(sets [][]string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, set := range sets {
		for _, id := range set {
			if id == "" {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func mergeLabelSelectors(sets [][]string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, set := range sets {
		for _, selector := range set {
			selector = strings.TrimSpace(selector)
			if selector == "" {
				continue
			}
			if _, dup := seen[selector]; dup {
				continue
			}
			seen[selector] = struct{}{}
			out = append(out, selector)
		}
	}
	sort.Strings(out)
	return out
}

// noTenants is stored in the tenants field of grants that carry none (admin
// grants). An empty string cannot be used: the Casbin storage adapter drops
// trailing empty columns, so the rule would load back with three fields and be
// rejected as malformed.
const noTenants = "-"

func encodeTenants(ids []string) string {
	if len(ids) == 0 {
		return noTenants
	}
	return strings.Join(ids, tenantSep)
}

func decodeTenants(s string) []string {
	if s == "" || s == noTenants {
		return nil
	}
	return strings.Split(s, tenantSep)
}
