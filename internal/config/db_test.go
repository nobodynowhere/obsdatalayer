package config_test

import (
	"errors"
	"fmt"
	"testing"

	"gorm.io/gorm"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/db"
)

func openTestConfigDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	gormDB, err := db.Open(db.DSN{Type: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.Migrate(gormDB); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gormDB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := config.EnsureSettings(gormDB); err != nil {
		t.Fatalf("ensure settings: %v", err)
	}
	return gormDB
}

// A fresh database is immediately usable: defaults are written once and the
// instance list starts empty.
func TestEnsureSettingsCreatesDefaults(t *testing.T) {
	gormDB := openTestConfigDB(t)

	cfg, err := config.LoadFromDB(gormDB)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Gateway.MaxBodyBytes != 32*1024*1024 {
		t.Errorf("expected default body limit, got %d", cfg.Gateway.MaxBodyBytes)
	}
	if cfg.Gateway.LogLevel != "info" {
		t.Errorf("expected default log level, got %q", cfg.Gateway.LogLevel)
	}
	if len(cfg.Instances) != 0 {
		t.Errorf("expected no instances on a fresh database, got %d", len(cfg.Instances))
	}
}

func TestEnsureSettingsIsIdempotent(t *testing.T) {
	gormDB := openTestConfigDB(t)
	if err := config.EnsureSettings(gormDB); err != nil {
		t.Fatalf("second EnsureSettings: %v", err)
	}
	var count int64
	if err := gormDB.Model(&db.GatewaySetting{}).Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("expected exactly one settings row, got %d", count)
	}
}

func TestInstanceRoundTrip(t *testing.T) {
	gormDB := openTestConfigDB(t)

	original := &config.InstanceConfig{
		Name: "loki-prod", Backend: "loki", URL: "http://loki.local",
		BasicAuth: "user:pass", TenantID: "",
		Labels: &config.LabelsConfig{
			Filter: &config.FilterConfig{Mode: "allowlist", Names: []string{"env", "cluster"}},
			Inject: map[string]string{"env": "prod"},
		},
	}
	if err := config.CreateInstance(gormDB, original, nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	cfg, err := config.LoadFromDB(gormDB)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got, ok := cfg.ByName["loki-prod"]
	if !ok {
		t.Fatal("expected loki-prod in the loaded config")
	}
	if got.URL != original.URL || got.BasicAuth != original.BasicAuth {
		t.Errorf("instance did not round-trip: %+v", got)
	}
	if got.Labels == nil || got.Labels.Filter == nil {
		t.Fatal("labels did not round-trip")
	}
	if len(got.Labels.Filter.Names) != 2 {
		t.Errorf("expected 2 filter names, got %v", got.Labels.Filter.Names)
	}
	if got.Labels.Inject["env"] != "prod" {
		t.Errorf("expected injected env=prod, got %v", got.Labels.Inject)
	}
}

func TestCreateInstanceRejectsDuplicate(t *testing.T) {
	gormDB := openTestConfigDB(t)
	i := inst("loki-prod", "loki", "http://loki.local")
	if err := config.CreateInstance(gormDB, i, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	err := config.CreateInstance(gormDB, inst("loki-prod", "loki", "http://other.local"), nil)
	if !errors.Is(err, config.ErrExists) {
		t.Errorf("expected ErrExists, got %v", err)
	}
}

func TestCreateInstanceValidates(t *testing.T) {
	gormDB := openTestConfigDB(t)
	// A bare path is not a usable upstream URL.
	if err := config.CreateInstance(gormDB, inst("bad", "loki", "/not-a-url"), nil); err == nil {
		t.Error("expected validation to reject the instance before writing")
	}
	cfg, err := config.LoadFromDB(gormDB)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Instances) != 0 {
		t.Error("a rejected instance must not reach the database")
	}
}

// Updating rebuilds child rows, so stale push targets and labels must not
// survive the replace.
func TestUpdateInstanceReplacesChildren(t *testing.T) {
	gormDB := openTestConfigDB(t)

	original := &config.InstanceConfig{
		Name: "mimir-prod", Backend: "mimir",
		PushURLs: []config.PushTarget{{URL: "http://a.local"}, {URL: "http://b.local"}},
		Labels:   &config.LabelsConfig{Inject: map[string]string{"env": "prod"}},
	}
	if err := config.CreateInstance(gormDB, original, nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	updated := &config.InstanceConfig{
		Name: "mimir-prod", Backend: "mimir",
		PushURLs: []config.PushTarget{{URL: "http://c.local"}},
	}
	if err := config.UpdateInstance(gormDB, "mimir-prod", updated, nil); err != nil {
		t.Fatalf("update: %v", err)
	}

	cfg, err := config.LoadFromDB(gormDB)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := cfg.ByName["mimir-prod"]
	if len(got.PushURLs) != 1 || got.PushURLs[0].URL != "http://c.local" {
		t.Errorf("expected push targets to be replaced, got %+v", got.PushURLs)
	}
	if got.Labels != nil {
		t.Errorf("expected labels to be removed, got %+v", got.Labels)
	}
}

func TestUpdateMissingInstance(t *testing.T) {
	gormDB := openTestConfigDB(t)
	err := config.UpdateInstance(gormDB, "nope", inst("nope", "loki", "http://x.local"), nil)
	if !errors.Is(err, config.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestDeleteInstance(t *testing.T) {
	gormDB := openTestConfigDB(t)
	i := &config.InstanceConfig{
		Name: "loki-prod", Backend: "loki",
		PushURLs: []config.PushTarget{{URL: "http://a.local"}},
		Labels:   &config.LabelsConfig{Inject: map[string]string{"env": "prod"}},
	}
	if err := config.CreateInstance(gormDB, i, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := config.DeleteInstance(gormDB, "loki-prod"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	cfg, err := config.LoadFromDB(gormDB)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Instances) != 0 {
		t.Errorf("expected the instance to be gone, got %d", len(cfg.Instances))
	}
	// Child rows must not be orphaned.
	var targets, groups int64
	gormDB.Model(&db.PushTarget{}).Count(&targets)
	gormDB.Model(&db.LabelsGroup{}).Count(&groups)
	if targets != 0 || groups != 0 {
		t.Errorf("expected child rows removed, got %d targets and %d label groups", targets, groups)
	}

	if err := config.DeleteInstance(gormDB, "loki-prod"); !errors.Is(err, config.ErrNotFound) {
		t.Errorf("expected ErrNotFound on second delete, got %v", err)
	}
}

func TestSaveSettings(t *testing.T) {
	gormDB := openTestConfigDB(t)

	g := config.GatewayConfig{MaxBodyBytes: 1024, LogLevel: "debug"}
	_ = g.Timeouts.Query.UnmarshalText([]byte("5s"))
	_ = g.Timeouts.Push.UnmarshalText([]byte("7s"))
	_ = g.ReloadInterval.UnmarshalText([]byte("90s"))

	if err := config.SaveSettings(gormDB, g); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	cfg, err := config.LoadFromDB(gormDB)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Gateway.MaxBodyBytes != 1024 {
		t.Errorf("expected 1024, got %d", cfg.Gateway.MaxBodyBytes)
	}
	if cfg.Gateway.LogLevel != "debug" {
		t.Errorf("expected debug, got %q", cfg.Gateway.LogLevel)
	}
	if cfg.Gateway.Timeouts.Query.Duration().String() != "5s" {
		t.Errorf("expected 5s query timeout, got %v", cfg.Gateway.Timeouts.Query.Duration())
	}
	if cfg.Gateway.ReloadInterval.Duration().String() != "1m30s" {
		t.Errorf("expected 90s reload interval, got %v", cfg.Gateway.ReloadInterval.Duration())
	}
}

func TestSaveSettingsRejectsBadLogLevel(t *testing.T) {
	gormDB := openTestConfigDB(t)
	g := config.GatewayConfig{LogLevel: "chatty"}
	if err := config.SaveSettings(gormDB, g); err == nil {
		t.Error("expected an invalid log level to be rejected")
	}
}
