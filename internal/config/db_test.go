package config_test

import (
	"testing"

	"gorm.io/gorm"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/db"
)

func openTestConfigDB(t *testing.T) *gorm.DB {
	t.Helper()
	gormDB, err := db.Open(db.DSN{Type: "sqlite", DSN: "file::memory:?cache=shared"})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.Migrate(gormDB); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}
	return gormDB
}

func TestConfigDBRoundTrip(t *testing.T) {
	gormDB := openTestConfigDB(t)

	cfg, err := config.LoadYAML([]byte(baseConfig))
	if err != nil {
		t.Fatalf("load yaml: %v", err)
	}

	if err := config.SeedFromConfig(gormDB, cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	bootstrap := &config.Bootstrap{
		Gateway: config.GatewayBootstrap{Port: 9090, AdminPort: 9091},
	}
	loaded, err := config.LoadFromDB(gormDB, bootstrap)
	if err != nil {
		t.Fatalf("load from db: %v", err)
	}

	if loaded.Gateway.Port != 9090 {
		t.Errorf("expected port 9090, got %d", loaded.Gateway.Port)
	}
	if loaded.Gateway.AdminPort != 9091 {
		t.Errorf("expected admin port 9091, got %d", loaded.Gateway.AdminPort)
	}
	if loaded.Gateway.MaxBodyBytes != cfg.Gateway.MaxBodyBytes {
		t.Errorf("max_body_bytes mismatch: expected %d, got %d", cfg.Gateway.MaxBodyBytes, loaded.Gateway.MaxBodyBytes)
	}

	if len(loaded.Instances) != len(cfg.Instances) {
		t.Fatalf("expected %d instances, got %d", len(cfg.Instances), len(loaded.Instances))
	}

	for i, inst := range loaded.Instances {
		orig := cfg.Instances[i]
		if inst.Name != orig.Name || inst.Backend != orig.Backend {
			t.Errorf("instance %d mismatch: got %s/%s, expected %s/%s", i, inst.Name, inst.Backend, orig.Name, orig.Backend)
		}
		if _, ok := loaded.ByName[inst.Name]; !ok {
			t.Errorf("ByName missing %q", inst.Name)
		}
	}
}
