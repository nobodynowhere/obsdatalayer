package db

import (
	"github.com/gofrs/uuid/v5"
)

// GatewaySetting stores a single row of global gateway configuration.
// Version is the user-visible config schema version.
type GatewaySetting struct {
	ID           uuid.UUID `gorm:"type:text;primaryKey"`
	Version      int
	MaxBodyBytes int64
	QueryTimeout string
	PushTimeout  string
}

// Instance is a configured backend tenant.
type Instance struct {
	ID         uuid.UUID `gorm:"type:text;primaryKey"`
	Name       string    `gorm:"uniqueIndex"`
	Backend    string
	URL        string
	FanOutMode string
	BasicAuth  string
	TenantID   string

	LabelsGroup *LabelsGroup `gorm:"foreignKey:InstanceID;constraint:OnDelete:CASCADE;"`
	PushTargets []PushTarget `gorm:"foreignKey:InstanceID;constraint:OnDelete:CASCADE;"`
}

// PushTarget is an explicit fan-out target for an instance.
type PushTarget struct {
	ID         uuid.UUID `gorm:"type:text;primaryKey"`
	InstanceID uuid.UUID `gorm:"type:text;index"`
	URL        string
	BasicAuth  string
	TenantID   string
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

// User is an account with one or more policies.
type User struct {
	ID             uuid.UUID `gorm:"type:text;primaryKey"`
	Name           string    `gorm:"uniqueIndex"`
	PasswordBcrypt string
	Admin          bool

	Policies []Policy `gorm:"foreignKey:UserID;constraint:OnDelete:CASCADE;"`
}

// Policy is a set of backends, actions and tenants a user may access.
type Policy struct {
	ID     uuid.UUID `gorm:"type:text;primaryKey"`
	UserID uuid.UUID `gorm:"type:text;index"`

	Backends  []PolicyBackend  `gorm:"foreignKey:PolicyID;constraint:OnDelete:CASCADE;"`
	Actions   []PolicyAction   `gorm:"foreignKey:PolicyID;constraint:OnDelete:CASCADE;"`
	TenantIDs []PolicyTenantID `gorm:"foreignKey:PolicyID;constraint:OnDelete:CASCADE;"`
}

// PolicyBackend is one backend allowed by a policy.
type PolicyBackend struct {
	ID       uuid.UUID `gorm:"type:text;primaryKey"`
	PolicyID uuid.UUID `gorm:"type:text;index"`
	Backend  string
}

// PolicyAction is one action allowed by a policy.
type PolicyAction struct {
	ID       uuid.UUID `gorm:"type:text;primaryKey"`
	PolicyID uuid.UUID `gorm:"type:text;index"`
	Action   string
}

// PolicyTenantID is one tenant allowed by a policy.
type PolicyTenantID struct {
	ID       uuid.UUID `gorm:"type:text;primaryKey"`
	PolicyID uuid.UUID `gorm:"type:text;index"`
	TenantID string
}
