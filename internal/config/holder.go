package config

import (
	"errors"
	"log/slog"
	"sync"

	"gorm.io/gorm"
)

// ConfigHolder provides thread-safe access to the active Config and supports
// atomic reload from the backing file or database without restarting the gateway.
type ConfigHolder struct {
	mu     sync.RWMutex
	cfg    *Config
	db     *gorm.DB
	source string
}

// NewHolder wraps cfg in a ConfigHolder without a database source.
// Reload is a no-op for holders created this way.
func NewHolder(cfg *Config, source string) *ConfigHolder {
	return &ConfigHolder{cfg: cfg, source: source}
}

// NewDBHolder loads the active config from db and returns a holder that can reload from it.
func NewDBHolder(db *gorm.DB, source string) (*ConfigHolder, error) {
	cfg, err := LoadFromDB(db)
	if err != nil {
		return nil, err
	}
	return &ConfigHolder{cfg: cfg, db: db, source: source}, nil
}

// Get returns the current Config. Callers must not modify the returned value.
func (h *ConfigHolder) Get() *Config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cfg
}

// Path returns the source identifier for the config.
func (h *ConfigHolder) Path() string { return h.source }

// Stage re-reads and validates the config from the database without publishing
// it. Callers that must coordinate several reloads can stage each source first
// and only Publish once every one of them has succeeded.
func (h *ConfigHolder) Stage() (*Config, error) {
	if h.db == nil {
		return nil, errors.New("config holder has no database source")
	}
	cfg, err := LoadFromDB(h.db)
	if err != nil {
		return nil, err
	}
	slog.Debug("staged config from database",
		"instances", len(cfg.Instances),
		"max_body_bytes", cfg.Gateway.MaxBodyBytes,
		"log_level", cfg.Gateway.LogLevel)
	return cfg, nil
}

// Publish atomically installs a config previously returned by Stage.
func (h *ConfigHolder) Publish(cfg *Config) {
	h.mu.Lock()
	h.cfg = cfg
	h.mu.Unlock()
}

// Reload stages and publishes in one step. Prefer Stage plus Publish when the
// reload has to succeed or fail together with another subsystem.
func (h *ConfigHolder) Reload() (*Config, error) {
	cfg, err := h.Stage()
	if err != nil {
		return nil, err
	}
	h.Publish(cfg)
	return cfg, nil
}
