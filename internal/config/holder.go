package config

import (
	"errors"
	"sync"

	"gorm.io/gorm"
)

// ConfigHolder provides thread-safe access to the active Config and supports
// atomic reload from the backing file or database without restarting the gateway.
type ConfigHolder struct {
	mu        sync.RWMutex
	cfg       *Config
	db        *gorm.DB
	bootstrap *Bootstrap
	source    string
}

// NewHolder wraps cfg in a ConfigHolder without a database source.
// Reload is a no-op for holders created this way.
func NewHolder(cfg *Config, source string) *ConfigHolder {
	return &ConfigHolder{cfg: cfg, source: source}
}

// NewDBHolder loads the active config from db and returns a holder that can reload from it.
func NewDBHolder(db *gorm.DB, bootstrap *Bootstrap, source string) (*ConfigHolder, error) {
	cfg, err := LoadFromDB(db, bootstrap)
	if err != nil {
		return nil, err
	}
	return &ConfigHolder{cfg: cfg, db: db, bootstrap: bootstrap, source: source}, nil
}

// Get returns the current Config. Callers must not modify the returned value.
func (h *ConfigHolder) Get() *Config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cfg
}

// Path returns the source identifier for the config (file path or DB DSN description).
func (h *ConfigHolder) Path() string { return h.source }

// Reload re-reads the config from the database, validates it, and atomically
// replaces the current config if valid. Returns the new config on success.
func (h *ConfigHolder) Reload() (*Config, error) {
	if h.db == nil {
		return nil, errors.New("config holder has no database source")
	}
	cfg, err := LoadFromDB(h.db, h.bootstrap)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.cfg = cfg
	h.mu.Unlock()
	return cfg, nil
}
