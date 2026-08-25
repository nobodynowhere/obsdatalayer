package config_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"obsdatalayer/internal/authlimit"
	"obsdatalayer/internal/config"
	"obsdatalayer/internal/db"
)

func openTestConfigDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	gormDB, err := db.Open(db.Config{Type: "sqlite", Path: dsn})
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

	cfg, err := config.LoadFromDB(gormDB, nil)
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
		BasicAuth: "user:pass", TenantID: "", SkipTLSVerify: true,
		Labels: &config.LabelsConfig{
			Filter: &config.FilterConfig{Mode: "allowlist", Names: []string{"env", "cluster"}},
			Inject: map[string]string{"env": "prod"},
		},
	}
	if err := config.CreateInstance(gormDB, original, nil, nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	cfg, err := config.LoadFromDB(gormDB, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got, ok := cfg.ByName["loki-prod"]
	if !ok {
		t.Fatal("expected loki-prod in the loaded config")
	}
	if got.URL != original.URL || got.BasicAuth != original.BasicAuth || got.SkipTLSVerify != original.SkipTLSVerify {
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
	if err := config.CreateInstance(gormDB, i, nil, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	err := config.CreateInstance(gormDB, inst("loki-prod", "loki", "http://other.local"), nil, nil)
	if !errors.Is(err, config.ErrExists) {
		t.Errorf("expected ErrExists, got %v", err)
	}
}

func TestCreateInstanceValidates(t *testing.T) {
	gormDB := openTestConfigDB(t)
	// A bare path is not a usable upstream URL.
	if err := config.CreateInstance(gormDB, inst("bad", "loki", "/not-a-url"), nil, nil); err == nil {
		t.Error("expected validation to reject the instance before writing")
	}
	cfg, err := config.LoadFromDB(gormDB, nil)
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
	if err := config.CreateInstance(gormDB, original, nil, nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	updated := &config.InstanceConfig{
		Name: "mimir-prod", Backend: "mimir",
		PushURLs: []config.PushTarget{{URL: "https://c.local", SkipTLSVerify: true}},
	}
	if err := config.UpdateInstance(gormDB, "mimir-prod", updated, nil, nil); err != nil {
		t.Fatalf("update: %v", err)
	}

	cfg, err := config.LoadFromDB(gormDB, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := cfg.ByName["mimir-prod"]
	if len(got.PushURLs) != 1 || got.PushURLs[0].URL != "https://c.local" || !got.PushURLs[0].SkipTLSVerify {
		t.Errorf("expected push targets to be replaced, got %+v", got.PushURLs)
	}
	if got.Labels != nil {
		t.Errorf("expected labels to be removed, got %+v", got.Labels)
	}
}

func TestUpdateMissingInstance(t *testing.T) {
	gormDB := openTestConfigDB(t)
	err := config.UpdateInstance(gormDB, "nope", inst("nope", "loki", "http://x.local"), nil, nil)
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
	if err := config.CreateInstance(gormDB, i, nil, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := config.DeleteInstance(gormDB, "loki-prod"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	cfg, err := config.LoadFromDB(gormDB, nil)
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

	cfg, err := config.LoadFromDB(gormDB, nil)
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

// ---- authentication throttle settings ---------------------------------------

func TestAuthLimitDefaultsOnFreshDatabase(t *testing.T) {
	gormDB := openTestConfigDB(t)

	cfg, err := config.LoadFromDB(gormDB, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	al := cfg.Gateway.AuthLimit
	if !al.ThrottleEnabled() {
		t.Error("expected the throttle to default to on")
	}
	if al.FailureThreshold != authlimit.DefaultFailureThreshold {
		t.Errorf("failure threshold = %d, want %d", al.FailureThreshold, authlimit.DefaultFailureThreshold)
	}
	if al.FailureWindow.Duration() != authlimit.DefaultFailureWindow {
		t.Errorf("failure window = %v, want %v", al.FailureWindow.Duration(), authlimit.DefaultFailureWindow)
	}
	if al.HashConcurrency() <= 0 {
		t.Errorf("expected a positive default hashing cap, got %d", al.HashConcurrency())
	}
}

func TestAuthLimitRoundTrips(t *testing.T) {
	gormDB := openTestConfigDB(t)

	disabled := false
	g := config.GatewayConfig{
		MaxBodyBytes:   1024,
		LogLevel:       "info",
		ReloadInterval: config.Duration(30 * time.Second),
		Timeouts: config.TimeoutConfig{
			Query: config.Duration(10 * time.Second),
			Push:  config.Duration(20 * time.Second),
		},
		AuthLimit: config.AuthLimitConfig{
			Enabled:             &disabled,
			FailureThreshold:    9,
			FailureWindow:       config.Duration(2 * time.Minute),
			BlockDuration:       config.Duration(3 * time.Minute),
			MaxBlockDuration:    config.Duration(30 * time.Minute),
			MaxConcurrentHashes: 6,
			HashWait:            config.Duration(750 * time.Millisecond),
		},
	}
	if err := config.SaveSettings(gormDB, g); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	cfg, err := config.LoadFromDB(gormDB, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := cfg.Gateway.AuthLimit
	if got.ThrottleEnabled() {
		t.Error("expected the throttle to have been persisted as disabled")
	}
	if got.FailureThreshold != 9 {
		t.Errorf("failure threshold = %d, want 9", got.FailureThreshold)
	}
	if got.BlockDuration.Duration() != 3*time.Minute {
		t.Errorf("block duration = %v, want 3m", got.BlockDuration.Duration())
	}
	if got.HashConcurrency() != 6 {
		t.Errorf("hash concurrency = %d, want 6", got.HashConcurrency())
	}
	if got.HashWait.Duration() != 750*time.Millisecond {
		t.Errorf("hash wait = %v, want 750ms", got.HashWait.Duration())
	}
}

// The upgrade case, and the reason auth_limit_enabled is nullable: a settings
// row written before these columns existed reads them as NULL and empty. That
// must yield a working throttle, not a silently disabled one -- a security fix
// that turns itself off on every existing install is not a fix.
func TestAuthLimitDefaultsOnPreUpgradeRow(t *testing.T) {
	gormDB := openTestConfigDB(t)

	// Reproduce a row from before the feature: flag NULL, everything else zero.
	if err := gormDB.Table("gateway_settings").Where("1 = 1").Updates(map[string]any{
		"auth_limit_enabled":         nil,
		"auth_failure_threshold":     0,
		"auth_failure_window":        "",
		"auth_block_duration":        "",
		"auth_max_block_duration":    "",
		"auth_max_concurrent_hashes": 0,
		"auth_hash_wait":             "",
	}).Error; err != nil {
		t.Fatalf("simulate pre-upgrade row: %v", err)
	}

	cfg, err := config.LoadFromDB(gormDB, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	al := cfg.Gateway.AuthLimit
	if !al.ThrottleEnabled() {
		t.Error("an upgraded gateway came up with the throttle disabled")
	}
	if al.FailureThreshold != authlimit.DefaultFailureThreshold {
		t.Errorf("failure threshold = %d, want the default %d",
			al.FailureThreshold, authlimit.DefaultFailureThreshold)
	}
	if al.HashConcurrency() <= 0 {
		t.Errorf("expected a working hashing cap, got %d", al.HashConcurrency())
	}
	// And the limiter it renders must actually throttle.
	if !al.Limiter().Enabled {
		t.Error("rendered limiter config is disabled")
	}
}

// An explicit false must survive: "off" is a decision, not an absence.
func TestAuthLimitExplicitDisableIsHonoured(t *testing.T) {
	gormDB := openTestConfigDB(t)

	cfg, err := config.LoadFromDB(gormDB, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	g := cfg.Gateway
	disabled := false
	g.AuthLimit.Enabled = &disabled
	if err := config.SaveSettings(gormDB, g); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	reloaded, err := config.LoadFromDB(gormDB, nil)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Gateway.AuthLimit.ThrottleEnabled() {
		t.Error("an explicit disable was lost")
	}
}

func TestAuthLimitRejectsBlockBeyondCap(t *testing.T) {
	gormDB := openTestConfigDB(t)

	cfg, err := config.LoadFromDB(gormDB, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	g := cfg.Gateway
	g.AuthLimit.BlockDuration = config.Duration(time.Hour)
	g.AuthLimit.MaxBlockDuration = config.Duration(time.Minute)

	if err := config.SaveSettings(gormDB, g); err == nil {
		t.Fatal("expected a block duration above the cap to be rejected")
	}
}

// ---- push target ordering ---------------------------------------------------
//
// Order is meaningful: writes go to every target, but reads try them in order
// and prefer the first. An operator reordering the list expects that to decide
// which upstream is queried, so the order has to survive the database.

func fanoutInst(name string, urls ...string) *config.InstanceConfig {
	targets := make([]config.PushTarget, len(urls))
	for i, u := range urls {
		targets[i] = config.PushTarget{URL: u}
	}
	return &config.InstanceConfig{Name: name, Backend: "mimir", FanOutMode: "all", PushURLs: targets}
}

func targetURLs(inst *config.InstanceConfig) []string {
	out := make([]string, len(inst.PushURLs))
	for i, t := range inst.PushURLs {
		out[i] = t.URL
	}
	return out
}

func TestPushTargetOrderSurvivesRoundTrip(t *testing.T) {
	gormDB := openTestConfigDB(t)

	// Deliberately not alphabetical, and not the order random UUID primary keys
	// would produce, so a missing ORDER BY shows up rather than passing by luck.
	want := []string{"http://z.local", "http://a.local", "http://m.local"}
	if err := config.CreateInstance(gormDB, fanoutInst("mimir-ha", want...), nil, nil); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	cfg, err := config.LoadFromDB(gormDB, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := targetURLs(cfg.Instances[0])
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("target order = %v, want %v", got, want)
		}
	}
}

func TestPushTargetGroupSurvivesRoundTrip(t *testing.T) {
	gormDB := openTestConfigDB(t)

	inst := fanoutInst("tempo-prod", "http://tempo-otlp.local", "http://tempo-api.local")
	inst.Backend = "tempo"
	inst.PushURLs[0].Group = config.TargetGroupOTLPHTTP
	inst.PushURLs[1].Group = config.TargetGroupQuery
	if err := config.CreateInstance(gormDB, inst, nil, nil); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	cfg, err := config.LoadFromDB(gormDB, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	targets := cfg.Instances[0].PushURLs
	if targets[0].Group != config.TargetGroupOTLPHTTP || targets[1].Group != config.TargetGroupQuery {
		t.Fatalf("target groups = %q, %q", targets[0].Group, targets[1].Group)
	}
	if read := cfg.Instances[0].GetReadTargets(); len(read) != 1 || read[0].URL != "http://tempo-api.local" {
		t.Fatalf("read targets = %+v", read)
	}
	if otlp := cfg.Instances[0].GetTargets(config.TargetGroupOTLPHTTP); len(otlp) != 1 || otlp[0].URL != "http://tempo-otlp.local" {
		t.Fatalf("OTLP targets = %+v", otlp)
	}
}

func TestMigrateAddsTargetGroupToLegacyPushTargets(t *testing.T) {
	gormDB := openTestConfigDB(t)

	inst := fanoutInst("tempo-prod", "http://tempo.local")
	inst.Backend = "tempo"
	if err := config.CreateInstance(gormDB, inst, nil, nil); err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := gormDB.Migrator().DropColumn(&db.PushTarget{}, "target_group"); err != nil {
		t.Fatalf("drop target_group: %v", err)
	}
	if gormDB.Migrator().HasColumn(&db.PushTarget{}, "target_group") {
		t.Fatal("precondition failed: target_group column still exists")
	}

	if err := db.Migrate(gormDB); err != nil {
		t.Fatalf("migrate legacy db: %v", err)
	}
	if !gormDB.Migrator().HasColumn(&db.PushTarget{}, "target_group") {
		t.Fatal("target_group column was not added")
	}
	cfg, err := config.LoadFromDB(gormDB, nil)
	if err != nil {
		t.Fatalf("load migrated db: %v", err)
	}
	targets := cfg.Instances[0].PushURLs
	if len(targets) != 1 || targets[0].Group != "" {
		t.Fatalf("legacy target group = %+v", targets)
	}
	if read := cfg.Instances[0].GetReadTargets(); len(read) != 1 || read[0].URL != "http://tempo.local" {
		t.Fatalf("legacy target should remain read fallback, got %+v", read)
	}
	if otlp := cfg.Instances[0].GetTargets(config.TargetGroupOTLPHTTP); len(otlp) != 1 || otlp[0].URL != "http://tempo.local" {
		t.Fatalf("legacy target should remain ingest fallback, got %+v", otlp)
	}
}

// Reordering through the admin API must actually change which target is first,
// which is what decides where reads go.
func TestPushTargetOrderCanBeChanged(t *testing.T) {
	gormDB := openTestConfigDB(t)

	if err := config.CreateInstance(gormDB, fanoutInst("mimir-ha",
		"http://first.local", "http://second.local"), nil, nil); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	// Swap them, exactly as the UI's move-up control does.
	reordered := fanoutInst("mimir-ha", "http://second.local", "http://first.local")
	if err := config.UpdateInstance(gormDB, "mimir-ha", reordered, nil, nil); err != nil {
		t.Fatalf("update instance: %v", err)
	}

	cfg, err := config.LoadFromDB(gormDB, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := targetURLs(cfg.Instances[0])
	if got[0] != "http://second.local" {
		t.Errorf("after reordering, first target = %q, want http://second.local (order = %v)", got[0], got)
	}
	// And the read path must agree with the stored order.
	if read := cfg.Instances[0].GetReadTargets(); read[0].URL != "http://second.local" {
		t.Errorf("read prefers %q, want the newly promoted target", read[0].URL)
	}
}

// Position is persisted per instance, so two instances do not interleave.
func TestPushTargetOrderIsPerInstance(t *testing.T) {
	gormDB := openTestConfigDB(t)

	if err := config.CreateInstance(gormDB, fanoutInst("a", "http://a1.local", "http://a2.local"), nil, nil); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if err := config.CreateInstance(gormDB, fanoutInst("b", "http://b1.local", "http://b2.local"), nil, nil); err != nil {
		t.Fatalf("create b: %v", err)
	}

	cfg, err := config.LoadFromDB(gormDB, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, inst := range cfg.Instances {
		got := targetURLs(inst)
		if len(got) != 2 || got[0] != "http://"+inst.Name+"1.local" {
			t.Errorf("instance %q targets = %v, want its own two in order", inst.Name, got)
		}
	}
}

// ---- per-target timeout -----------------------------------------------------

func TestTargetTimeoutRoundTrips(t *testing.T) {
	gormDB := openTestConfigDB(t)

	inst := fanoutInst("mimir-ha", "http://a.local", "http://b.local")
	inst.PushURLs[0].TimeoutSeconds = 12
	// The second deliberately leaves it unset.
	if err := config.CreateInstance(gormDB, inst, nil, nil); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	cfg, err := config.LoadFromDB(gormDB, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	targets := cfg.Instances[0].PushURLs
	if targets[0].TimeoutSeconds != 12 {
		t.Errorf("first target timeout = %d, want 12", targets[0].TimeoutSeconds)
	}
	if targets[1].TimeoutSeconds != 0 {
		t.Errorf("second target timeout = %d, want 0 (defer to the default)", targets[1].TimeoutSeconds)
	}

	// And the effective value resolves against the supplied default.
	if got := targets[0].Timeout(30 * time.Second); got != 12*time.Second {
		t.Errorf("effective timeout = %v, want 12s", got)
	}
	if got := targets[1].Timeout(30 * time.Second); got != 30*time.Second {
		t.Errorf("effective fallback = %v, want the 30s default", got)
	}
}

func TestDefaultTargetTimeoutDefaultsAndRoundTrips(t *testing.T) {
	gormDB := openTestConfigDB(t)

	cfg, err := config.LoadFromDB(gormDB, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Gateway.DefaultTargetTimeout.Duration(); got != 30*time.Second {
		t.Errorf("default = %v, want 30s", got)
	}

	g := cfg.Gateway
	g.DefaultTargetTimeout = config.Duration(45 * time.Second)
	if err := config.SaveSettings(gormDB, g); err != nil {
		t.Fatalf("save: %v", err)
	}
	reloaded, err := config.LoadFromDB(gormDB, nil)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloaded.Gateway.DefaultTargetTimeout.Duration(); got != 45*time.Second {
		t.Errorf("reloaded default = %v, want 45s", got)
	}
}

// A settings row written before the column existed reads as empty and must take
// the default rather than zero, which would mean "no bound at all".
func TestDefaultTargetTimeoutOnPreUpgradeRow(t *testing.T) {
	gormDB := openTestConfigDB(t)

	if err := gormDB.Table("gateway_settings").Where("1 = 1").
		Update("default_target_timeout", "").Error; err != nil {
		t.Fatalf("simulate pre-upgrade row: %v", err)
	}

	cfg, err := config.LoadFromDB(gormDB, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Gateway.DefaultTargetTimeout.Duration(); got != 30*time.Second {
		t.Errorf("pre-upgrade row gave %v, want the 30s default", got)
	}
}

func TestNegativeTargetTimeoutIsRejected(t *testing.T) {
	gormDB := openTestConfigDB(t)

	inst := fanoutInst("mimir-ha", "http://a.local")
	inst.PushURLs[0].TimeoutSeconds = -5
	if err := config.CreateInstance(gormDB, inst, nil, nil); err == nil {
		t.Error("expected a negative per-target timeout to be rejected")
	}
}

// Off is the default, and an explicit on survives a round trip. The gauge that
// exposes backend URLs to unauthenticated callers must never arrive switched on
// by accident.
func TestMetricsUnauthenticatedDefaultsOffAndRoundTrips(t *testing.T) {
	gormDB := openTestConfigDB(t)

	cfg, err := config.LoadFromDB(gormDB, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Gateway.MetricsUnauthenticated {
		t.Fatal("a fresh database served /metrics without authentication")
	}

	g := cfg.Gateway
	g.MetricsUnauthenticated = true
	if err := config.SaveSettings(gormDB, g); err != nil {
		t.Fatalf("save: %v", err)
	}
	cfg, err = config.LoadFromDB(gormDB, nil)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !cfg.Gateway.MetricsUnauthenticated {
		t.Error("metrics_unauthenticated did not survive the round trip")
	}

	// And back off again. This is the direction a struct-valued Updates would
	// silently drop, leaving /metrics open after the operator had closed it.
	g = cfg.Gateway
	g.MetricsUnauthenticated = false
	if err := config.SaveSettings(gormDB, g); err != nil {
		t.Fatalf("save off: %v", err)
	}
	cfg, err = config.LoadFromDB(gormDB, nil)
	if err != nil {
		t.Fatalf("reload off: %v", err)
	}
	if cfg.Gateway.MetricsUnauthenticated {
		t.Error("turning metrics_unauthenticated back off did not persist")
	}
}

// A row written before the column existed reads as NULL. It must load as false
// -- authentication required -- rather than failing the load or, worse,
// upgrading a gateway straight into serving /metrics anonymously.
func TestMetricsUnauthenticatedDefaultsOnPreUpgradeRow(t *testing.T) {
	gormDB := openTestConfigDB(t)

	if err := gormDB.Table("gateway_settings").Where("1 = 1").
		Updates(map[string]any{"metrics_unauthenticated": nil}).Error; err != nil {
		t.Fatalf("simulate pre-upgrade row: %v", err)
	}

	cfg, err := config.LoadFromDB(gormDB, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Gateway.MetricsUnauthenticated {
		t.Error("an upgraded gateway came up serving /metrics without authentication")
	}
}
