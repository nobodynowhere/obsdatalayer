package db

import (
	"github.com/gofrs/uuid/v5"
)

// GatewaySetting stores the single row of global gateway configuration.
// Everything here is hot-reloadable; only the database connection and the
// listener addresses live outside the database, in the bootstrap file.
//
// Durations are stored as strings ("30s") so the row stays readable to an
// operator inspecting the database directly.
type GatewaySetting struct {
	ID             uuid.UUID `gorm:"type:text;primaryKey"`
	MaxBodyBytes   int64
	QueryTimeout   string
	PushTimeout    string
	LogLevel       string
	ReloadInterval string

	// Authentication throttling. AuthLimitEnabled is nullable on purpose: a row
	// written before these columns existed reads as NULL, which the config
	// layer takes as "default on". A plain bool would read as false and leave
	// an upgraded gateway silently unprotected.
	AuthLimitEnabled        *bool
	AuthFailureThreshold    int
	AuthFailureWindow       string
	AuthBlockDuration       string
	AuthMaxBlockDuration    string
	AuthMaxConcurrentHashes int
	AuthHashWait            string
}

// Instance is a configured backend tenant.
type Instance struct {
	ID            uuid.UUID `gorm:"type:text;primaryKey"`
	Name          string    `gorm:"uniqueIndex"`
	Backend       string
	URL           string
	FanOutMode    string
	BasicAuth     string
	TenantID      string
	SkipTLSVerify bool

	LabelsGroup *LabelsGroup `gorm:"foreignKey:InstanceID;constraint:OnDelete:CASCADE;"`
	PushTargets []PushTarget `gorm:"foreignKey:InstanceID;constraint:OnDelete:CASCADE;"`
}

// PushTarget is an explicit fan-out target for an instance.
//
// Position records where the target sits in the configured list. Without it the
// order rows come back in is whatever the database chooses, which is not the
// order the operator wrote: the primary key is a random UUID, so ordering by it
// is arbitrary, and nothing else in the row carries the sequence. That matters
// now that reads try targets in order -- the first target is the preferred one,
// and an operator reordering the list in the UI expects that to take effect.
type PushTarget struct {
	ID            uuid.UUID `gorm:"type:text;primaryKey"`
	InstanceID    uuid.UUID `gorm:"type:text;index"`
	Position      int
	URL           string
	BasicAuth     string
	TenantID      string
	SkipTLSVerify bool
}

// LabelsGroup stores the label filter/inject configuration for an instance.
type LabelsGroup struct {
	ID         uuid.UUID `gorm:"type:text;primaryKey"`
	InstanceID uuid.UUID `gorm:"type:text;uniqueIndex"`

	Filter  *Filter       `gorm:"foreignKey:LabelsGroupID;constraint:OnDelete:CASCADE;"`
	Injects []LabelInject `gorm:"foreignKey:LabelsGroupID;constraint:OnDelete:CASCADE;"`
}

// Filter stores an allowlist/denylist block.
type Filter struct {
	ID            uuid.UUID `gorm:"type:text;primaryKey"`
	LabelsGroupID uuid.UUID `gorm:"type:text;uniqueIndex"`
	Mode          string

	Names []FilterName `gorm:"foreignKey:FilterID;constraint:OnDelete:CASCADE;"`
}

// FilterName is one entry in a filter list.
type FilterName struct {
	ID       uuid.UUID `gorm:"type:text;primaryKey"`
	FilterID uuid.UUID `gorm:"type:text;index"`
	Name     string
}

// LabelInject is one injected label pair.
type LabelInject struct {
	ID            uuid.UUID `gorm:"type:text;primaryKey"`
	LabelsGroupID uuid.UUID `gorm:"type:text;index"`
	Key           string
	Value         string
}

// Tenant is a first-class tenant identity. ID is the value injected upstream
// as X-Scope-OrgID, so it is immutable once grants reference it.
//
// GrafanaID is nullable on purpose: it is reserved for the future Grafana
// traffic proxy, and a NULL keeps unassigned tenants from colliding on the
// unique index (NULLs do not compare equal in SQLite or Postgres).
type Tenant struct {
	ID        uuid.UUID `gorm:"type:text;primaryKey"`
	Name      string    `gorm:"uniqueIndex"`
	GrafanaID *int      `gorm:"uniqueIndex"`
}

// GrantReadPolicy stores optional read-time policy metadata for a Casbin grant
// without changing the Casbin policy row shape. Subject, backend, action and
// tenant_key match the four Casbin policy fields.
type GrantReadPolicy struct {
	ID            uuid.UUID `gorm:"type:text;primaryKey"`
	Subject       string    `gorm:"uniqueIndex:idx_grant_read_policy"`
	Backend       string    `gorm:"uniqueIndex:idx_grant_read_policy"`
	Action        string    `gorm:"uniqueIndex:idx_grant_read_policy"`
	TenantKey     string    `gorm:"uniqueIndex:idx_grant_read_policy"`
	LabelSelector string
}

// User is a gateway account. Authorization lives entirely in Casbin
// (the casbin_rule table managed by the gorm adapter), so this table holds
// only identity and the password hash.
type User struct {
	ID             uuid.UUID `gorm:"type:text;primaryKey"`
	Name           string    `gorm:"uniqueIndex"`
	PasswordBcrypt string
}

// Role makes roles first-class so they can be created, listed and described
// through the admin API even before any grants or members are attached.
// The grants themselves live in Casbin.
type Role struct {
	ID          uuid.UUID `gorm:"type:text;primaryKey"`
	Name        string    `gorm:"uniqueIndex"`
	Description string
}
