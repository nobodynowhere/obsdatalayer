package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/casbin/casbin/v3"
	"github.com/casbin/casbin/v3/model"
	gormadapter "github.com/casbin/gorm-adapter/v3"
	"github.com/gofrs/uuid/v5"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"obsdatalayer/internal/authlimit"
	dbstore "obsdatalayer/internal/db"
	"obsdatalayer/internal/tenant"
)

// modelText is the Casbin RBAC model.
//
// A policy line is: subject, object, action, tenants
// where object is a backend name ("loki"/"mimir"/"tempo"), "*", or the
// admin-plane object "admin"; and tenants is a "|"-joined tenant ID list.
//
// The matcher's wildcard clause excludes the admin object on purpose: a grant
// of ("*", "*") covers every backend but never the admin plane, which must be
// granted explicitly as ("admin", "access").
const modelText = `
[request_definition]
r = sub, obj, act

[policy_definition]
p = sub, obj, act, tenants

[role_definition]
g = _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = g(r.sub, p.sub) && (r.obj == p.obj || (p.obj == "*" && r.obj != "admin")) && (p.act == "*" || r.act == p.act)
`

// Casbin keeps every subject in one flat namespace, so a user and a role
// sharing a name would be the same subject: deleting the user would strip the
// role's grants from everyone holding it. Subjects are therefore prefixed by
// kind on the way in and unprefixed on the way out.
const (
	subjectUserPrefix = "u:"
	subjectRolePrefix = "r:"
)

func userSubject(name string) string { return subjectUserPrefix + name }
func roleSubject(name string) string { return subjectRolePrefix + name }

func unprefix(subject, prefix string) (string, bool) {
	if !strings.HasPrefix(subject, prefix) {
		return "", false
	}
	return subject[len(prefix):], true
}

// ErrInvalidGrant marks a grant that is malformed or names an unknown tenant.
// It is distinct from ErrNotFound so callers can answer 400 rather than 404.
var ErrInvalidGrant = errors.New("invalid grant")

// Authorizer is the subset of Service the HTTP layer depends on.
type Authorizer interface {
	Authenticate(name, password string) (*User, error)
	// AuthenticateContext is Authenticate bound to a request, so a caller that
	// disconnects stops occupying a password-hashing slot.
	AuthenticateContext(ctx context.Context, name, password string) (*User, error)
	// AuthenticateAPIKey resolves a bearer token to its owning user.
	AuthenticateAPIKey(token string) (*User, error)
	AccessFor(name, backend, action string) (Access, bool)
	// AccessDecision is AccessFor with the reason for a refusal, so a 403 can
	// be logged with something more useful than "denied".
	AccessDecision(name, backend, action string) AccessDecision
	CanAdmin(name string) bool
}

// Access is the resolved authorization result for one backend/action request.
type Access struct {
	TenantIDs      []string
	LabelSelectors []string
}

// AccessDenyReason names why authorization refused a request. It is written to
// the denial log line, so the values are stable strings an operator can grep
// and alert on rather than free text.
type AccessDenyReason string

const (
	// No grant matched the backend and action at all.
	AccessDenyNoMatchingGrant AccessDenyReason = "no_matching_grant"
	// A grant matched, but every tenant it names has since been deleted.
	AccessDenyNoLiveTenants AccessDenyReason = "no_live_tenants"
	// The action needs exactly one tenant -- write, tail, delete -- and the
	// grants resolved to several, with nothing in the request to choose between
	// them.
	AccessDenyAmbiguousTenant AccessDenyReason = "ambiguous_tenant"
	// The resolved read label policies disagree, so there is no single
	// selector that satisfies all of them.
	AccessDenyReadPolicy AccessDenyReason = "read_policy"
	// The read label policy could not be read from the database. Fails closed:
	// an unreadable policy is not an absent one.
	AccessDenyReadPolicyLookup AccessDenyReason = "read_policy_lookup"
)

// AccessDecision is the resolved authorization result plus the reason a
// request was refused when Allowed is false.
type AccessDecision struct {
	Access      Access
	Allowed     bool
	DenyReason  AccessDenyReason
	TenantCount int
}

// Service owns authentication (bcrypt over the users table) and authorization
// (Casbin over the casbin_rule table). Both share the gateway's database.
type Service struct {
	db      *gorm.DB
	enf     *casbin.SyncedEnforcer
	tenants *tenant.Store
	users   atomic.Pointer[map[string]*User]
	cache   *credentialCache

	// apiKeys is the authentication index for bearer credentials, refreshed by
	// Reload so issuing or revoking a key takes effect immediately.
	apiKeys *apiKeyIndex

	// hashGate bounds concurrent bcrypt work. It is swapped wholesale on a
	// settings reload rather than resized: an in-flight caller holds the gate
	// it acquired from and releases back to that same one, so a resize can
	// never leave a slot stranded in the new gate.
	hashGate atomic.Pointer[authlimit.Gate]
}

var _ Authorizer = (*Service)(nil)

// NewService builds the enforcer over db and loads the current user snapshot.
// tenants is consulted whenever a grant is written or evaluated, so a grant can
// never name a tenant the gateway does not know about.
func NewService(db *gorm.DB, tenants *tenant.Store) (*Service, error) {
	adapter, err := gormadapter.NewAdapterByDB(db)
	if err != nil {
		return nil, fmt.Errorf("auth: casbin adapter: %w", err)
	}
	m, err := model.NewModelFromString(modelText)
	if err != nil {
		return nil, fmt.Errorf("auth: casbin model: %w", err)
	}
	enf, err := casbin.NewSyncedEnforcer(m, adapter)
	if err != nil {
		return nil, fmt.Errorf("auth: casbin enforcer: %w", err)
	}
	if err := enf.LoadPolicy(); err != nil {
		return nil, fmt.Errorf("auth: load policy: %w", err)
	}

	s := &Service{db: db, enf: enf, tenants: tenants, cache: newCredentialCache(time.Minute), apiKeys: newAPIKeyIndex()}
	s.SetHashLimit(authlimit.DefaultMaxConcurrentHashes(), authlimit.DefaultHashWait)
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// SetHashLimit replaces the concurrency bound on password hashing. A max of
// zero or less removes the bound. Safe to call while requests are in flight.
func (s *Service) SetHashLimit(max int, wait time.Duration) {
	s.hashGate.Store(authlimit.NewGate(max, wait))
}

// HashLimit reports the configured hashing concurrency, or zero when unbounded.
func (s *Service) HashLimit() int {
	return s.hashGate.Load().Cap()
}

// Reload refreshes the user snapshot and the policy set from the database.
func (s *Service) Reload() error {
	var rows []dbstore.User
	if err := s.db.Find(&rows).Error; err != nil {
		return fmt.Errorf("auth: load users: %w", err)
	}
	idx := make(map[string]*User, len(rows))
	for _, r := range rows {
		idx[r.Name] = &User{Name: r.Name, PasswordBcrypt: r.PasswordBcrypt}
	}
	s.users.Store(&idx)
	// The credential cache is deliberately not cleared here. Invalidation is
	// structural -- see credentialCache -- and wiping it on every reload put
	// every valid caller back through bcrypt twice a minute at the default
	// reload interval, which is precisely when they most need to stay out of
	// the hashing queue an attacker is filling.

	if err := s.loadAPIKeys(); err != nil {
		return err
	}

	if err := s.enf.LoadPolicy(); err != nil {
		return fmt.Errorf("auth: reload policy: %w", err)
	}
	policies, _ := s.enf.GetPolicy()
	groupings, _ := s.enf.GetGroupingPolicy()
	slog.Debug("auth reloaded",
		"users", len(idx), "policies", len(policies), "role_bindings", len(groupings))
	return nil
}

// Tenants exposes the tenant registry backing this service.
func (s *Service) Tenants() *tenant.Store { return s.tenants }

// ---- authentication ---------------------------------------------------------

// Authenticate verifies a username and plaintext password. Unknown usernames
// still run a real bcrypt comparison so they are not cheaply enumerable.
func (s *Service) Authenticate(name, password string) (*User, error) {
	return s.AuthenticateContext(context.Background(), name, password)
}

// AuthenticateContext is Authenticate with a context, so a caller that has gone
// away stops waiting for a hashing slot.
//
// The cache is consulted before the gate is taken. A caller presenting a
// credential that was recently verified therefore never queues behind an
// attacker's hashing, which is what keeps a flood of bad credentials from
// denying service to callers holding good ones.
func (s *Service) AuthenticateContext(ctx context.Context, name, password string) (*User, error) {
	idx := *s.users.Load()
	u, ok := idx[name]
	if ok && s.cache.Valid(name, password, u.PasswordBcrypt) {
		return u, nil
	}

	gate := s.hashGate.Load()
	if !gate.Acquire(ctx) {
		return nil, ErrHashLimitReached
	}
	defer gate.Release()

	if !ok {
		// Compare against a real hash so the unknown-user path costs the same
		// as the known-user path. See dummyHash. The gate is held across this
		// too, or the equalizer would be undone by the gate itself.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return nil, ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordBcrypt), []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
	}
	s.cache.Store(name, password, u.PasswordBcrypt)
	return u, nil
}

// credentialCache remembers credentials that have already passed bcrypt, so a
// repeat caller does not pay tens of milliseconds of CPU on every request.
//
// Its safety does not rest on anything remembering to invalidate it. The key
// binds the stored password hash, and Valid is always called with the hash from
// the current user snapshot, so a rotated password yields a different key and
// can never match a stale entry. A deleted user fails the snapshot lookup before
// the cache is consulted at all. And the cache covers authentication only --
// authorization is resolved per request by AccessFor -- so a revoked grant takes
// effect immediately regardless of what is cached here.
//
// Only a successful compare populates it, so a caller guessing passwords cannot
// grow it.
type credentialCache struct {
	mu        sync.Mutex
	ttl       time.Duration
	entries   map[string]time.Time
	lastSweep time.Time
}

func newCredentialCache(ttl time.Duration) *credentialCache {
	return &credentialCache{ttl: ttl, entries: make(map[string]time.Time), lastSweep: time.Now()}
}

func (c *credentialCache) Valid(name, password, passwordHash string) bool {
	key := credentialCacheKey(name, password, passwordHash)
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	expires, ok := c.entries[key]
	if !ok || !now.Before(expires) {
		delete(c.entries, key)
		return false
	}
	return true
}

func (c *credentialCache) Store(name, password, passwordHash string) {
	key := credentialCacheKey(name, password, passwordHash)
	now := time.Now()
	c.mu.Lock()
	c.sweepLocked(now)
	c.entries[key] = now.Add(c.ttl)
	c.mu.Unlock()
}

// sweepLocked drops expired entries. Valid only ever evicts the key it looks
// up, so an entry that is never looked up again -- the usual fate of an entry
// orphaned by a password rotation -- would otherwise live for the lifetime of
// the process. Sweeping once per TTL bounds the map to the credentials actually
// used in the last window.
func (c *credentialCache) sweepLocked(now time.Time) {
	if now.Sub(c.lastSweep) < c.ttl {
		return
	}
	c.lastSweep = now
	for key, expires := range c.entries {
		if !now.Before(expires) {
			delete(c.entries, key)
		}
	}
}

// size reports the number of live entries. Used by tests to assert the sweep
// actually bounds the map.
func (c *credentialCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func credentialCacheKey(name, password, passwordHash string) string {
	sum := sha256.Sum256([]byte(password))
	return name + "\x00" + passwordHash + "\x00" + hex.EncodeToString(sum[:])
}

// ---- authorization ----------------------------------------------------------

// AccessFor returns the merged tenant IDs and read policy selectors the user
// may use for the given backend and action. ok is false when no policy matches,
// which callers should surface as 403.
func (s *Service) AccessFor(name, backend, action string) (Access, bool) {
	decision := s.AccessDecision(name, backend, action)
	return decision.Access, decision.Allowed
}

// AccessDecision returns AccessFor's decision with a refusal reason suitable
// for structured operator logs.
func (s *Service) AccessDecision(name, backend, action string) AccessDecision {
	perms, err := s.enf.GetImplicitPermissionsForUser(userSubject(name))
	if err != nil {
		return AccessDecision{DenyReason: AccessDenyNoMatchingGrant}
	}
	resolved, reason := s.resolveGrants(perms, backend, action)
	if reason != "" {
		return AccessDecision{DenyReason: reason}
	}
	if !resolved.matched {
		return AccessDecision{DenyReason: AccessDenyNoMatchingGrant}
	}
	ids := mergeTenants(resolved.tenantSets)

	// Drop references whose tenant has since been deleted: sending a stale
	// UUID upstream would scope the request to a tenant that no longer exists.
	live := ids[:0]
	for _, id := range ids {
		if s.tenants.Exists(id) {
			live = append(live, id)
		}
	}
	if len(live) == 0 {
		// A matching grant with no usable tenants cannot produce an
		// X-Scope-OrgID, so there is nothing safe to forward.
		return AccessDecision{DenyReason: AccessDenyNoLiveTenants}
	}
	if ActionRequiresSingleTenant(action) && len(live) != 1 {
		return AccessDecision{DenyReason: AccessDenyAmbiguousTenant, TenantCount: len(live)}
	}

	access := Access{TenantIDs: live}
	if SupportsReadLabelSelector(backend, action) {
		policies := resolved.readPolicies
		if action == ActionTail {
			// A tail grant only pins the tenant; it does not widen what the
			// caller may see inside it. Resolving the read policy as well and
			// folding it in keeps a tail from streaming log lines the same
			// caller's read policy would have filtered out. The extra policy
			// lookups are affordable: a tail is one WebSocket handshake, not a
			// query loop.
			readGrants, readReason := s.resolveGrants(perms, backend, ActionRead)
			if readReason != "" {
				return AccessDecision{DenyReason: readReason, TenantCount: len(live)}
			}
			policies = narrowTailPolicies(policies, readGrants.readPolicies, live)
		}
		selectors, ok := effectiveReadSelectors(live, policies)
		if !ok {
			return AccessDecision{DenyReason: AccessDenyReadPolicy, TenantCount: len(live)}
		}
		access.LabelSelectors = selectors
	}
	return AccessDecision{Access: access, Allowed: true, TenantCount: len(live)}
}

// resolvedGrants is what one pass over a user's policy rows yields for a
// backend and action: whether anything matched at all, the tenant sets to
// merge, and the read label policy each tenant carries.
type resolvedGrants struct {
	matched      bool
	tenantSets   [][]string
	readPolicies map[string]*readPolicyState
}

// resolveGrants collects the rows matching backend and action. A non-empty
// reason means the resolution failed outright and the caller must deny.
func (s *Service) resolveGrants(perms [][]string, backend, action string) (resolvedGrants, AccessDenyReason) {
	out := resolvedGrants{readPolicies: make(map[string]*readPolicyState)}
	for _, row := range perms {
		if len(row) < 3 {
			continue
		}
		if !objectMatches(row[1], backend) || !actionMatches(row[2], action) {
			continue
		}
		out.matched = true
		var tenants []string
		if len(row) >= 4 {
			tenants = decodeTenants(row[3])
			out.tenantSets = append(out.tenantSets, tenants)
		}
		if !SupportsReadLabelSelector(backend, action) {
			continue
		}
		selector, ok := s.grantReadPolicySelector(row, tenants)
		if !ok {
			return resolvedGrants{}, AccessDenyReadPolicyLookup
		}
		for _, tenantID := range tenants {
			state := out.readPolicies[tenantID]
			if state == nil {
				state = &readPolicyState{selectors: make(map[string]struct{})}
				out.readPolicies[tenantID] = state
			}
			if selector == "" {
				state.unrestricted = true
			} else {
				state.selectors[selector] = struct{}{}
			}
		}
	}
	return out, ""
}

// narrowTailPolicies folds each tenant's read policy into its tail policy.
//
// Within one action, a grant carrying no selector widens access: two read
// grants for the same tenant, one restricted and one not, leave the tenant
// unrestricted. Across these two actions that rule would be a hole -- a tail
// grant added with no selector would hand back, live, exactly the log lines
// the caller's read policy excludes. So here the read policy wins: a
// restricted read makes the tail restricted too, whatever the tail grant says.
//
// Two different selectors are left for effectiveReadSelectors to refuse. There
// is one selector to send upstream and no way to express "both must hold", so
// a tail policy that disagrees with the read policy is denied rather than
// resolved in either direction.
func narrowTailPolicies(tail, read map[string]*readPolicyState, tenantIDs []string) map[string]*readPolicyState {
	out := make(map[string]*readPolicyState, len(tenantIDs))
	for _, id := range tenantIDs {
		readState := read[id]
		if readState == nil || readState.unrestricted {
			out[id] = tail[id]
			continue
		}
		merged := &readPolicyState{selectors: make(map[string]struct{}, len(readState.selectors)+1)}
		for selector := range readState.selectors {
			merged.selectors[selector] = struct{}{}
		}
		if tailState := tail[id]; tailState != nil {
			for selector := range tailState.selectors {
				merged.selectors[selector] = struct{}{}
			}
		}
		out[id] = merged
	}
	return out
}

// TenantIDsFor returns the merged tenant IDs the user may use for the given
// backend and action. It is retained for callers and tests that do not need
// Mimir read label policies.
func (s *Service) TenantIDsFor(name, backend, action string) ([]string, bool) {
	access, ok := s.AccessFor(name, backend, action)
	if !ok {
		return nil, false
	}
	return access.TenantIDs, true
}

type readPolicyState struct {
	unrestricted bool
	selectors    map[string]struct{}
}

func effectiveReadSelectors(tenantIDs []string, policies map[string]*readPolicyState) ([]string, bool) {
	var effective string
	hasRestricted, hasUnrestricted := false, false
	for _, id := range tenantIDs {
		state := policies[id]
		if state == nil {
			return nil, false
		}
		if state.unrestricted {
			hasUnrestricted = true
			continue
		}
		if len(state.selectors) != 1 {
			return nil, false
		}
		var next string
		for selector := range state.selectors {
			next = selector
		}
		hasRestricted = true
		if effective == "" {
			effective = next
			continue
		}
		if effective != next {
			return nil, false
		}
	}
	if hasRestricted && hasUnrestricted {
		return nil, false
	}
	if effective == "" {
		return nil, true
	}
	return []string{effective}, true
}

func (s *Service) grantReadPolicySelector(policyRow, tenants []string) (string, bool) {
	if len(policyRow) < 4 || !SupportsReadLabelSelector(policyRow[1], policyRow[2]) {
		return "", true
	}
	var row dbstore.GrantReadPolicy
	err := s.db.Where("subject = ? AND backend = ? AND action = ? AND tenant_key = ?",
		policyRow[0], policyRow[1], policyRow[2], encodeTenants(tenants)).
		First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", true
		}
		return "", false
	}
	return strings.TrimSpace(row.LabelSelector), true
}

// CanAdmin reports whether the user may reach the admin plane.
func (s *Service) CanAdmin(name string) bool {
	ok, err := s.enf.Enforce(userSubject(name), ObjectAdmin, ActionAccess)
	return err == nil && ok
}

// ---- users ------------------------------------------------------------------

// UserInfo is the API view of a user.
type UserInfo struct {
	Name   string   `json:"name"`
	Roles  []string `json:"roles"`
	Grants []Grant  `json:"grants"`
	Admin  bool     `json:"admin"`
}

// ErrNotFound is returned when a named user or role does not exist.
var ErrNotFound = errors.New("not found")

// ErrExists is returned when creating a user or role whose name is taken.
var ErrExists = errors.New("already exists")

// ListUsers returns every user with their roles and direct grants.
func (s *Service) ListUsers() ([]UserInfo, error) {
	var rows []dbstore.User
	if err := s.db.Order("name").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	out := make([]UserInfo, 0, len(rows))
	for _, r := range rows {
		info, err := s.userInfo(r.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

// GetUser returns one user, or ErrNotFound.
func (s *Service) GetUser(name string) (UserInfo, error) {
	var row dbstore.User
	if err := s.db.Where("name = ?", name).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return UserInfo{}, ErrNotFound
		}
		return UserInfo{}, fmt.Errorf("get user: %w", err)
	}
	return s.userInfo(name)
}

func (s *Service) userInfo(name string) (UserInfo, error) {
	raw, err := s.enf.GetRolesForUser(userSubject(name))
	if err != nil {
		return UserInfo{}, fmt.Errorf("roles for %q: %w", name, err)
	}
	roles := make([]string, 0, len(raw))
	for _, r := range raw {
		if role, ok := unprefix(r, subjectRolePrefix); ok {
			roles = append(roles, role)
		}
	}
	sort.Strings(roles)
	grants, err := s.grantsFor(userSubject(name))
	if err != nil {
		return UserInfo{}, err
	}
	return UserInfo{
		Name:   name,
		Roles:  roles,
		Grants: grants,
		Admin:  s.CanAdmin(name),
	}, nil
}

// CreateUser adds a user with the given password and role assignments.
func (s *Service) CreateUser(name, password string, roles []string) error {
	if name == "" {
		return errors.New("user name is required")
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	if err := s.assertRolesExist(roles); err != nil {
		return err
	}

	id, err := uuid.NewV4()
	if err != nil {
		return fmt.Errorf("generate user id: %w", err)
	}
	row := dbstore.User{ID: id, Name: name, PasswordBcrypt: hash}
	if err := s.db.Create(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return ErrExists
		}
		return fmt.Errorf("create user %q: %w", name, err)
	}

	if err := s.SetUserRoles(name, roles); err != nil {
		return err
	}
	return s.Reload()
}

// DeleteUser removes a user together with all of its policy and role bindings.
func (s *Service) DeleteUser(name string) error {
	res := s.db.Where("name = ?", name).Delete(&dbstore.User{})
	if res.Error != nil {
		return fmt.Errorf("delete user %q: %w", name, res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	if _, err := s.enf.DeleteUser(userSubject(name)); err != nil {
		return fmt.Errorf("delete user policies %q: %w", name, err)
	}
	if err := deleteAPIKeysForUser(s.db, name); err != nil {
		return err
	}
	if err := s.db.Where("subject = ?", userSubject(name)).Delete(&dbstore.GrantReadPolicy{}).Error; err != nil {
		return fmt.Errorf("delete user read policies %q: %w", name, err)
	}
	return s.Reload()
}

// SetPassword replaces a user's password.
func (s *Service) SetPassword(name, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	res := s.db.Model(&dbstore.User{}).Where("name = ?", name).Update("password_bcrypt", hash)
	if res.Error != nil {
		return fmt.Errorf("set password for %q: %w", name, res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return s.Reload()
}

// SetUserRoles replaces the user's role assignments.
func (s *Service) SetUserRoles(name string, roles []string) error {
	if _, err := s.GetUser(name); err != nil {
		return err
	}
	if err := s.assertRolesExist(roles); err != nil {
		return err
	}
	if _, err := s.enf.DeleteRolesForUser(userSubject(name)); err != nil {
		return fmt.Errorf("clear roles for %q: %w", name, err)
	}
	for _, r := range roles {
		if _, err := s.enf.AddGroupingPolicy(userSubject(name), roleSubject(r)); err != nil {
			return fmt.Errorf("grant role %q to %q: %w", r, name, err)
		}
	}
	return nil
}

// SetUserGrants replaces the user's direct (non-role) grants.
func (s *Service) SetUserGrants(name string, grants []Grant) error {
	if _, err := s.GetUser(name); err != nil {
		return err
	}
	return s.replaceGrants(userSubject(name), grants)
}

// ---- roles ------------------------------------------------------------------

// RoleInfo is the API view of a role.
type RoleInfo struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Grants      []Grant  `json:"grants"`
	Members     []string `json:"members"`
}

// ListRoles returns every role with its grants and members.
func (s *Service) ListRoles() ([]RoleInfo, error) {
	var rows []dbstore.Role
	if err := s.db.Order("name").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}
	out := make([]RoleInfo, 0, len(rows))
	for _, r := range rows {
		info, err := s.roleInfo(r)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

// GetRole returns one role, or ErrNotFound.
func (s *Service) GetRole(name string) (RoleInfo, error) {
	row, err := s.findRole(name)
	if err != nil {
		return RoleInfo{}, err
	}
	return s.roleInfo(row)
}

func (s *Service) roleInfo(row dbstore.Role) (RoleInfo, error) {
	grants, err := s.grantsFor(roleSubject(row.Name))
	if err != nil {
		return RoleInfo{}, err
	}
	raw, err := s.enf.GetUsersForRole(roleSubject(row.Name))
	if err != nil {
		// Casbin returns an error when a role has no members.
		raw = nil
	}
	members := make([]string, 0, len(raw))
	for _, m := range raw {
		if user, ok := unprefix(m, subjectUserPrefix); ok {
			members = append(members, user)
		}
	}
	sort.Strings(members)
	return RoleInfo{
		Name:        row.Name,
		Description: row.Description,
		Grants:      grants,
		Members:     members,
	}, nil
}

// CreateRole adds a role and its grants.
func (s *Service) CreateRole(name, description string, grants []Grant) error {
	if name == "" {
		return errors.New("role name is required")
	}
	for _, g := range grants {
		if err := g.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidGrant, err)
		}
		if err := s.tenants.ValidateAll(g.TenantIDs); err != nil {
			return fmt.Errorf("%w: on backend %q: %v", ErrInvalidGrant, g.Backend, err)
		}
	}
	if err := validateRoleGrantSet(grants); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidGrant, err)
	}
	id, err := uuid.NewV4()
	if err != nil {
		return fmt.Errorf("generate role id: %w", err)
	}
	row := dbstore.Role{ID: id, Name: name, Description: description}
	if err := s.db.Create(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return ErrExists
		}
		return fmt.Errorf("create role %q: %w", name, err)
	}
	return s.replaceGrants(roleSubject(name), grants)
}

// DeleteRole removes a role, its grants, and its membership bindings.
func (s *Service) DeleteRole(name string) error {
	res := s.db.Where("name = ?", name).Delete(&dbstore.Role{})
	if res.Error != nil {
		return fmt.Errorf("delete role %q: %w", name, res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	if _, err := s.enf.DeleteRole(roleSubject(name)); err != nil {
		return fmt.Errorf("delete role policies %q: %w", name, err)
	}
	if err := s.db.Where("subject = ?", roleSubject(name)).Delete(&dbstore.GrantReadPolicy{}).Error; err != nil {
		return fmt.Errorf("delete role read policies %q: %w", name, err)
	}
	return s.Reload()
}

// SetRoleGrants replaces a role's grants.
func (s *Service) SetRoleGrants(name string, grants []Grant) error {
	if _, err := s.findRole(name); err != nil {
		return err
	}
	if err := validateRoleGrantSet(grants); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidGrant, err)
	}
	return s.replaceGrants(roleSubject(name), grants)
}

func (s *Service) findRole(name string) (dbstore.Role, error) {
	var row dbstore.Role
	if err := s.db.Where("name = ?", name).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return dbstore.Role{}, ErrNotFound
		}
		return dbstore.Role{}, fmt.Errorf("get role: %w", err)
	}
	return row, nil
}

func (s *Service) assertRolesExist(roles []string) error {
	for _, r := range roles {
		if _, err := s.findRole(r); err != nil {
			if errors.Is(err, ErrNotFound) {
				return fmt.Errorf("role %q does not exist", r)
			}
			return err
		}
	}
	return nil
}

func validateRoleGrantSet(grants []Grant) error {
	var writeTenant string
	for _, g := range grants {
		if g.IsAdmin() || !ActionIsWrite(g.Action) {
			continue
		}
		if len(g.TenantIDs) != 1 {
			return fmt.Errorf("role write grant on backend %q must carry exactly one tenant_id", g.Backend)
		}
		tenantID := g.TenantIDs[0]
		if writeTenant == "" {
			writeTenant = tenantID
			continue
		}
		if tenantID != writeTenant {
			return errors.New("role write grants must target a single tenant")
		}
	}
	return nil
}

// ---- shared policy helpers --------------------------------------------------

// grantsFor returns the grants attached directly to a subject (not inherited).
func (s *Service) grantsFor(subject string) ([]Grant, error) {
	rows, err := s.enf.GetFilteredPolicy(0, subject)
	if err != nil {
		return nil, fmt.Errorf("policies for %q: %w", subject, err)
	}
	readPolicies, err := s.readPoliciesFor(subject)
	if err != nil {
		return nil, err
	}
	grants := make([]Grant, 0, len(rows))
	for _, row := range rows {
		if len(row) < 3 {
			continue
		}
		g := Grant{Backend: row[1], Action: row[2]}
		if len(row) >= 4 {
			g.TenantIDs = decodeTenants(row[3])
			g.ReadLabelSelector = readPolicies[grantReadPolicyKey(subject, g.Backend, g.Action, g.TenantIDs)]
		}
		grants = append(grants, g)
	}
	return grants, nil
}

func (s *Service) readPoliciesFor(subject string) (map[string]string, error) {
	var rows []dbstore.GrantReadPolicy
	if err := s.db.Where("subject = ?", subject).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("read policies for %q: %w", subject, err)
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		key := grantReadPolicyKeyFromTenantKey(row.Subject, row.Backend, row.Action, row.TenantKey)
		out[key] = strings.TrimSpace(row.LabelSelector)
	}
	return out, nil
}

// replaceGrants swaps all direct grants for subject, then refreshes the snapshot.
func (s *Service) replaceGrants(subject string, grants []Grant) error {
	for _, g := range grants {
		if err := g.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidGrant, err)
		}
		if err := s.tenants.ValidateAll(g.TenantIDs); err != nil {
			return fmt.Errorf("%w: on backend %q: %v", ErrInvalidGrant, g.Backend, err)
		}
	}
	if _, err := s.enf.DeletePermissionsForUser(subject); err != nil {
		return fmt.Errorf("clear grants for %q: %w", subject, err)
	}
	if err := s.db.Where("subject = ?", subject).Delete(&dbstore.GrantReadPolicy{}).Error; err != nil {
		return fmt.Errorf("clear read policies for %q: %w", subject, err)
	}
	for _, g := range grants {
		if _, err := s.enf.AddPolicy(subject, g.Backend, g.Action, encodeTenants(g.TenantIDs)); err != nil {
			return fmt.Errorf("add grant for %q: %w", subject, err)
		}
		if selector := strings.TrimSpace(g.ReadLabelSelector); selector != "" {
			id, err := uuid.NewV4()
			if err != nil {
				return fmt.Errorf("generate read policy id: %w", err)
			}
			row := dbstore.GrantReadPolicy{
				ID:            id,
				Subject:       subject,
				Backend:       g.Backend,
				Action:        g.Action,
				TenantKey:     encodeTenants(g.TenantIDs),
				LabelSelector: selector,
			}
			if err := s.db.Create(&row).Error; err != nil {
				return fmt.Errorf("add read policy for %q: %w", subject, err)
			}
		}
	}
	return s.Reload()
}

func grantReadPolicyKey(subject, backend, action string, tenantIDs []string) string {
	return grantReadPolicyKeyFromTenantKey(subject, backend, action, encodeTenants(tenantIDs))
}

func grantReadPolicyKeyFromTenantKey(subject, backend, action, tenantKey string) string {
	return subject + "\x00" + backend + "\x00" + action + "\x00" + tenantKey
}
