package config

import "sync"

// ConfigHolder provides thread-safe access to the active Config and supports
// atomic reload from the backing file without restarting the gateway.
type ConfigHolder struct {
	mu   sync.RWMutex
	cfg  *Config
	path string
}

// NewHolder wraps cfg in a ConfigHolder backed by the file at path.
func NewHolder(cfg *Config, path string) *ConfigHolder {
	return &ConfigHolder{cfg: cfg, path: path}
}

// Get returns the current Config. Callers must not modify the returned value.
func (h *ConfigHolder) Get() *Config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cfg
}

// Path returns the config file path.
func (h *ConfigHolder) Path() string { return h.path }

// Reload re-reads the config file, validates it, and atomically replaces the
// current config if valid. Returns the new config on success.
func (h *ConfigHolder) Reload() (*Config, error) {
	cfg, err := Load(h.path)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.cfg = cfg
	h.mu.Unlock()
	return cfg, nil
}
