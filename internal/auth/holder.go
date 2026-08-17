package auth

import (
	"sync/atomic"

	"gorm.io/gorm"
)

// Source provides a snapshot of the current user database.
// Both *UserFile and *AuthHolder implement Source.
type Source interface {
	Get() *UserFile
}

// Get returns the static UserFile. Implements Source.
func (uf *UserFile) Get() *UserFile { return uf }

// AuthHolder holds the current in-memory snapshot of users and supports reload.
type AuthHolder struct {
	uf atomic.Pointer[UserFile]
	db *gorm.DB
}

// NewAuthHolder loads users from db and returns a reloadable holder.
func NewAuthHolder(db *gorm.DB) (*AuthHolder, error) {
	uf, err := LoadFromDB(db)
	if err != nil {
		return nil, err
	}
	h := &AuthHolder{db: db}
	h.uf.Store(uf)
	return h, nil
}

// Get returns the current auth snapshot. Implements Source.
func (h *AuthHolder) Get() *UserFile { return h.uf.Load() }

// Reload replaces the in-memory snapshot with the latest database contents.
func (h *AuthHolder) Reload() error {
	uf, err := LoadFromDB(h.db)
	if err != nil {
		return err
	}
	h.uf.Store(uf)
	return nil
}
