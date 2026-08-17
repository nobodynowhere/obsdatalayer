package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/goccy/go-yaml"
	"obsdatalayer/internal/db"
)

// GatewayBootstrap contains only the values needed to start the gateway process.
// All other gateway configuration lives in the database.
type GatewayBootstrap struct {
	Port      int `yaml:"port"`
	AdminPort int `yaml:"admin_port"`
}

// Bootstrap is the minimal startup file used to open the database and the
// process listeners. The database is the source of truth for all runtime config.
type Bootstrap struct {
	DB             db.DSN          `yaml:"db"`
	Gateway        GatewayBootstrap `yaml:"gateway"`
	LogLevel       string          `yaml:"log_level"`
	ReloadInterval Duration        `yaml:"reload_interval"`
	Seed           yaml.RawMessage `yaml:"seed"`
	SeedFile       string          `yaml:"seed_file"`
}

// LoadBootstrap reads the bootstrap YAML file at path and returns the parsed Bootstrap.
// If SeedFile is provided and Seed is not, SeedFile is read relative to path's directory.
func LoadBootstrap(path string) (*Bootstrap, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read bootstrap %s: %w", path, err)
	}

	var b Bootstrap
	if err := yaml.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("parse bootstrap %s: %w", path, err)
	}

	if b.SeedFile != "" && len(b.Seed) == 0 {
		dir := filepath.Dir(path)
		seedPath := b.SeedFile
		if !filepath.IsAbs(seedPath) {
			seedPath = filepath.Join(dir, seedPath)
		}
		seedData, err := os.ReadFile(seedPath)
		if err != nil {
			return nil, fmt.Errorf("read seed file %s: %w", seedPath, err)
		}
		b.Seed = seedData
	}

	return &b, nil
}
