// Package tenant owns tenant identity. A tenant has three identifiers:
//
//   - ID        a UUID; this is the value injected upstream as X-Scope-OrgID
//   - Name      a human-readable label used in the admin API and logs
//   - GrafanaID Grafana's numeric tenant id. Reserved for the future Grafana
//     traffic proxy and unused today, so it is optional; tenants without one
//     are left unset rather than defaulted to zero.
//
// Authorization grants and instance configs reference a tenant by ID only, so
// renaming a tenant or changing its Grafana id never repoints where data lands.
package tenant

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/gofrs/uuid/v5"
	"gorm.io/gorm"

	dbstore "obsdatalayer/internal/db"
)

// Tenant is a resolved tenant identity.
type Tenant struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// GrafanaID is nil until a Grafana tenant is associated. It is not used in
	// any authorization or routing decision yet.
	GrafanaID *int `json:"grafana_id,omitempty"`
}

// Errors returned by Store.
var (
	ErrNotFound = errors.New("tenant not found")
	ErrExists   = errors.New("tenant already exists")
	ErrInUse    = errors.New("tenant is still referenced")
)

// Store is a reloadable, read-optimized view of the tenants table.
type Store struct {
	db    *gorm.DB
	index atomic.Pointer[index]
}

type index struct {
	byID   map[string]Tenant
	byName map[string]Tenant
	sorted []Tenant
}

// NewStore loads the tenant table into memory.
func NewStore(db *gorm.DB) (*Store, error) {
	s := &Store{db: db}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload refreshes the in-memory snapshot from the database.
func (s *Store) Reload() error {
	var rows []dbstore.Tenant
	if err := s.db.Order("name").Find(&rows).Error; err != nil {
		return fmt.Errorf("tenant: load: %w", err)
	}
	idx := &index{
		byID:   make(map[string]Tenant, len(rows)),
		byName: make(map[string]Tenant, len(rows)),
		sorted: make([]Tenant, 0, len(rows)),
	}
	for _, r := range rows {
		t := Tenant{ID: r.ID.String(), Name: r.Name, GrafanaID: r.GrafanaID}
		idx.byID[t.ID] = t
		idx.byName[t.Name] = t
		idx.sorted = append(idx.sorted, t)
	}
	s.index.Store(idx)
	slog.Debug("tenant registry reloaded", "tenants", len(idx.sorted))
	return nil
}

// Get returns the tenant with the given UUID.
func (s *Store) Get(id string) (Tenant, bool) {
	t, ok := s.index.Load().byID[id]
	return t, ok
}

// ByName returns the tenant with the given name.
func (s *Store) ByName(name string) (Tenant, bool) {
	t, ok := s.index.Load().byName[name]
	return t, ok
}

// Exists reports whether a tenant with the given UUID is registered.
func (s *Store) Exists(id string) bool {
	_, ok := s.index.Load().byID[id]
	return ok
}

// List returns all tenants ordered by name.
func (s *Store) List() []Tenant {
	src := s.index.Load().sorted
	out := make([]Tenant, len(src))
	copy(out, src)
	return out
}

// Resolve maps a reference to a tenant UUID. A UUID is returned as-is once
// confirmed to exist; any other value is looked up as a name. This is used when
// seeding from YAML so operators can write readable names; at runtime all
// references are already UUIDs.
func (s *Store) Resolve(ref string) (string, error) {
	if s.Exists(ref) {
		return ref, nil
	}
	if t, ok := s.ByName(ref); ok {
		return t.ID, nil
	}
	return "", fmt.Errorf("%w: %q", ErrNotFound, ref)
}

// Create registers a tenant. If id is empty a UUID is generated.
// grafanaID may be nil; it is reserved for future Grafana proxying.
func (s *Store) Create(id, name string, grafanaID *int) (Tenant, error) {
	if strings.TrimSpace(name) == "" {
		return Tenant{}, errors.New("tenant name is required")
	}
	if err := validateGrafanaID(grafanaID); err != nil {
		return Tenant{}, err
	}

	var tid uuid.UUID
	var err error
	if id == "" {
		tid, err = uuid.NewV4()
		if err != nil {
			return Tenant{}, fmt.Errorf("generate tenant id: %w", err)
		}
	} else {
		tid, err = uuid.FromString(id)
		if err != nil {
			return Tenant{}, fmt.Errorf("tenant id %q is not a valid UUID: %w", id, err)
		}
	}

	row := dbstore.Tenant{ID: tid, Name: name, GrafanaID: grafanaID}
	if err := s.db.Create(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return Tenant{}, ErrExists
		}
		return Tenant{}, fmt.Errorf("create tenant %q: %w", name, err)
	}
	if err := s.Reload(); err != nil {
		return Tenant{}, err
	}
	t, _ := s.Get(tid.String())
	return t, nil
}

// EnsureTenant creates a tenant unless one with the same name already exists,
// making seeding safe to re-run. It implements config.TenantResolver.
func (s *Store) EnsureTenant(id, name string, grafanaID *int) error {
	if _, ok := s.ByName(name); ok {
		return nil
	}
	if id != "" && s.Exists(id) {
		return nil
	}
	_, err := s.Create(id, name, grafanaID)
	if errors.Is(err, ErrExists) {
		return nil
	}
	return err
}

// Update changes a tenant's name and Grafana id. The UUID is immutable, since
// it is the value already written into grants and sent upstream.
func (s *Store) Update(id, name string, grafanaID *int) (Tenant, error) {
	if !s.Exists(id) {
		return Tenant{}, ErrNotFound
	}
	if strings.TrimSpace(name) == "" {
		return Tenant{}, errors.New("tenant name is required")
	}
	if err := validateGrafanaID(grafanaID); err != nil {
		return Tenant{}, err
	}
	res := s.db.Model(&dbstore.Tenant{}).Where("id = ?", id).
		Updates(map[string]any{"name": name, "grafana_id": grafanaID})
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrDuplicatedKey) {
			return Tenant{}, ErrExists
		}
		return Tenant{}, fmt.Errorf("update tenant %q: %w", id, res.Error)
	}
	if err := s.Reload(); err != nil {
		return Tenant{}, err
	}
	t, _ := s.Get(id)
	return t, nil
}

// Delete removes a tenant. Callers must first confirm nothing references it.
func (s *Store) Delete(id string) error {
	res := s.db.Where("id = ?", id).Delete(&dbstore.Tenant{})
	if res.Error != nil {
		return fmt.Errorf("delete tenant %q: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return s.Reload()
}

// validateGrafanaID accepts nil (unassigned) or a positive integer.
func validateGrafanaID(id *int) error {
	if id == nil {
		return nil
	}
	if *id <= 0 {
		return errors.New("grafana_id must be a positive integer when set")
	}
	return nil
}

// ValidateAll returns an error naming every reference that is not a registered
// tenant. Used to reject config and grants that would otherwise scope requests
// to a tenant the gateway knows nothing about.
func (s *Store) ValidateAll(refs []string) error {
	var missing []string
	seen := make(map[string]struct{}, len(refs))
	for _, r := range refs {
		if r == "" || s.Exists(r) {
			continue
		}
		if _, dup := seen[r]; dup {
			continue
		}
		seen[r] = struct{}{}
		missing = append(missing, r)
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("%w: %s", ErrNotFound, strings.Join(missing, ", "))
}
