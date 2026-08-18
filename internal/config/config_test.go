package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"obsdatalayer/internal/config"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// loadInstances runs instances through the same validation the admin API
// applies before writing them to the database. Config is no longer authored as
// a YAML document, so validation is exercised on the structs directly.
func loadInstances(instances ...*config.InstanceConfig) (*config.Config, error) {
	return config.New(&config.Config{Instances: instances})
}

func inst(name, backend, url string) *config.InstanceConfig {
	return &config.InstanceConfig{Name: name, Backend: backend, URL: url}
}

// ---- defaults ---------------------------------------------------------------

func TestDefaultsApplied(t *testing.T) {
	cfg, err := loadInstances(inst("loki-prod", "loki", "http://loki.local"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if cfg.Gateway.Timeouts.Query.Duration() != 30*time.Second {
		t.Errorf("expected query timeout 30s, got %v", cfg.Gateway.Timeouts.Query.Duration())
	}
	if cfg.Gateway.Timeouts.Push.Duration() != 60*time.Second {
		t.Errorf("expected push timeout 60s, got %v", cfg.Gateway.Timeouts.Push.Duration())
	}
	if cfg.Gateway.MaxBodyBytes != 32*1024*1024 {
		t.Errorf("expected 32 MiB body limit, got %d", cfg.Gateway.MaxBodyBytes)
	}
	if cfg.Gateway.LogLevel != "info" {
		t.Errorf("expected log level info, got %q", cfg.Gateway.LogLevel)
	}
	if cfg.Gateway.ReloadInterval.Duration() != 30*time.Second {
		t.Errorf("expected reload interval 30s, got %v", cfg.Gateway.ReloadInterval.Duration())
	}
	if _, ok := cfg.ByName["loki-prod"]; !ok {
		t.Error("expected ByName to be populated")
	}
}

// A freshly installed gateway has no instances until an operator adds them.
// Rejecting that state would make removing the last instance unrecoverable.
func TestEmptyInstanceListIsValid(t *testing.T) {
	cfg, err := loadInstances()
	if err != nil {
		t.Fatalf("an empty instance list must be valid, got: %v", err)
	}
	if len(cfg.Instances) != 0 {
		t.Errorf("expected no instances, got %d", len(cfg.Instances))
	}
}

func TestFanOutModeDefaultsToAny(t *testing.T) {
	i := &config.InstanceConfig{
		Name: "loki-prod", Backend: "loki",
		PushURLs: []config.PushTarget{{URL: "http://a.local"}, {URL: "http://b.local"}},
	}
	cfg, err := loadInstances(i)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if cfg.Instances[0].FanOutMode != "any" {
		t.Errorf("expected fan_out_mode 'any', got %q", cfg.Instances[0].FanOutMode)
	}
}

// ---- gateway validation -----------------------------------------------------

func TestGatewayValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*config.GatewayConfig)
		wantErr bool
	}{
		{"defaults", func(*config.GatewayConfig) {}, false},
		{"negative body limit", func(g *config.GatewayConfig) { g.MaxBodyBytes = -1 }, true},
		{"bad log level", func(g *config.GatewayConfig) { g.LogLevel = "chatty" }, true},
		{"valid log level", func(g *config.GatewayConfig) { g.LogLevel = "debug" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			tc.mutate(&cfg.Gateway)
			_, err := config.New(cfg)
			if tc.wantErr && err == nil {
				t.Error("expected an error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected no error, got: %v", err)
			}
		})
	}
}

// ---- instance validation ----------------------------------------------------

func TestInstanceValidation(t *testing.T) {
	cases := []struct {
		name     string
		instance *config.InstanceConfig
		wantErr  string
	}{
		{"valid", inst("loki-prod", "loki", "http://loki.local"), ""},
		{"name with space", inst("loki prod", "loki", "http://loki.local"), "must match"},
		{"name with dot", inst("loki.prod", "loki", "http://loki.local"), "must match"},
		{"name with slash", inst("loki/prod", "loki", "http://loki.local"), "must match"},
		{"unknown backend", inst("kafka-prod", "kafka", "http://kafka.local"), "kafka"},
		{"neither url nor push_urls", &config.InstanceConfig{Name: "x", Backend: "loki"}, "neither"},
		{
			"both url and push_urls",
			&config.InstanceConfig{Name: "x", Backend: "loki", URL: "http://a.local",
				PushURLs: []config.PushTarget{{URL: "http://b.local"}}},
			"both",
		},
		{
			"push_urls on tempo",
			&config.InstanceConfig{Name: "t", Backend: "tempo",
				PushURLs: []config.PushTarget{{URL: "http://t.local"}}},
			"tempo",
		},
		{
			"labels on tempo",
			&config.InstanceConfig{Name: "t", Backend: "tempo", URL: "http://t.local",
				Labels: &config.LabelsConfig{Inject: map[string]string{"env": "prod"}}},
			"tempo",
		},
		{
			"fan_out_mode without push_urls",
			&config.InstanceConfig{Name: "x", Backend: "loki", URL: "http://a.local", FanOutMode: "any"},
			"fan_out_mode",
		},
		{
			"invalid fan_out_mode",
			&config.InstanceConfig{Name: "x", Backend: "loki", FanOutMode: "bad",
				PushURLs: []config.PushTarget{{URL: "http://a.local"}}},
			"fan_out_mode",
		},
		{
			"invalid filter mode",
			&config.InstanceConfig{Name: "x", Backend: "loki", URL: "http://a.local",
				Labels: &config.LabelsConfig{Filter: &config.FilterConfig{Mode: "random", Names: []string{"app"}}}},
			"allowlist or denylist",
		},
		{
			"push target missing url",
			&config.InstanceConfig{Name: "x", Backend: "loki", PushURLs: []config.PushTarget{{URL: ""}}},
			"missing url",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadInstances(tc.instance)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestDuplicateInstanceName(t *testing.T) {
	_, err := loadInstances(
		inst("loki-prod", "loki", "http://a.local"),
		inst("loki-prod", "loki", "http://b.local"),
	)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected a duplicate-name error, got: %v", err)
	}
}

func TestFanOutModeAllIsValid(t *testing.T) {
	i := &config.InstanceConfig{
		Name: "loki-prod", Backend: "loki", FanOutMode: "all",
		PushURLs: []config.PushTarget{{URL: "http://a.local"}, {URL: "http://b.local"}},
	}
	if _, err := loadInstances(i); err != nil {
		t.Errorf("expected fan_out_mode 'all' to be valid, got: %v", err)
	}
}

// ---- upstream URL validation ------------------------------------------------

// url.ParseRequestURI on its own accepts bare paths, protocol-relative URLs and
// arbitrary schemes. Those parse cleanly and then fail at request time, so
// validation requires an http/https scheme and a host.
func TestUpstreamURLValidation(t *testing.T) {
	cases := []struct {
		url     string
		wantErr bool
	}{
		{"http://loki.local", false},
		{"https://loki.local:3100", false},
		{"http://127.0.0.1:3100/prefix", false},
		{"loki.local", true},
		{"/just/a/path", true},
		{"//host/path", true},
		{"ftp://loki.local", true},
		{"not a url", true},
		{"http://", true},
		{"http://loki.local?a=b", true},
		{"http://loki.local#frag", true},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			_, err := loadInstances(inst("loki-prod", "loki", tc.url))
			if tc.wantErr && err == nil {
				t.Errorf("expected %q to be rejected", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected %q to be accepted, got: %v", tc.url, err)
			}
		})
	}
}

func TestPushTargetURLValidation(t *testing.T) {
	i := &config.InstanceConfig{
		Name: "loki-prod", Backend: "loki",
		PushURLs: []config.PushTarget{{URL: "http://loki-a.local"}, {URL: "/not-absolute"}},
	}
	_, err := loadInstances(i)
	if err == nil {
		t.Fatal("expected a bare-path push target to be rejected")
	}
	if !strings.Contains(err.Error(), "push_urls[1]") {
		t.Errorf("expected the error to identify the offending target, got: %v", err)
	}
}

// ---- target resolution ------------------------------------------------------

func TestGetPushTargetsPerTargetOverride(t *testing.T) {
	i := &config.InstanceConfig{
		Name: "loki-prod", Backend: "loki",
		BasicAuth: "default:pass", TenantID: "default-tenant", SkipTLSVerify: true,
		PushURLs: []config.PushTarget{
			{URL: "http://a.local", BasicAuth: "override:secret", TenantID: "tenant-a"},
			{URL: "http://b.local"},
		},
	}
	targets := i.GetPushTargets()
	if len(targets) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(targets))
	}
	if targets[0].BasicAuth != "override:secret" || targets[0].TenantID != "tenant-a" || !targets[0].SkipTLSVerify {
		t.Errorf("expected per-target override, got %+v", targets[0])
	}
	if targets[1].BasicAuth != "default:pass" || targets[1].TenantID != "default-tenant" || !targets[1].SkipTLSVerify {
		t.Errorf("expected fallback to instance defaults, got %+v", targets[1])
	}
}

func TestGetPushTargetsSingleURL(t *testing.T) {
	i := &config.InstanceConfig{
		Name: "loki-prod", Backend: "loki", URL: "http://loki.local",
		BasicAuth: "user:pass", TenantID: "my-tenant", SkipTLSVerify: true,
	}
	targets := i.GetPushTargets()
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	if targets[0].URL != "http://loki.local" || targets[0].TenantID != "my-tenant" || !targets[0].SkipTLSVerify {
		t.Errorf("unexpected target %+v", targets[0])
	}
}

func TestGetQueryTargetUsesFirstPushTarget(t *testing.T) {
	i := &config.InstanceConfig{
		Name: "loki-prod", Backend: "loki",
		PushURLs: []config.PushTarget{{URL: "http://a.local"}, {URL: "http://b.local"}},
	}
	if got := i.GetQueryTarget().URL; got != "http://a.local" {
		t.Errorf("expected the first push target, got %q", got)
	}
}

// ---- tenant reference validation --------------------------------------------

type fakeRegistry map[string]bool

func (f fakeRegistry) ValidateAll(refs []string) error {
	for _, r := range refs {
		if r != "" && !f[r] {
			return fmt.Errorf("tenant not found: %s", r)
		}
	}
	return nil
}

func TestValidateTenantsRejectsUnknownReference(t *testing.T) {
	const id = "6f1d2c9e-9d3a-4c1b-8f47-2b0a5e7c1d34"
	i := inst("loki-prod", "loki", "http://loki.local")
	i.TenantID = id
	cfg, err := loadInstances(i)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if err := cfg.ValidateTenants(fakeRegistry{}); err == nil {
		t.Error("expected an unregistered tenant reference to be rejected")
	}
	if err := cfg.ValidateTenants(fakeRegistry{id: true}); err != nil {
		t.Errorf("expected a registered tenant to pass, got: %v", err)
	}
	if err := cfg.ValidateTenants(nil); err != nil {
		t.Errorf("a nil registry should skip validation, got: %v", err)
	}
}

func TestValidateTenantsCoversPushTargets(t *testing.T) {
	i := &config.InstanceConfig{
		Name: "mimir-prod", Backend: "mimir",
		PushURLs: []config.PushTarget{
			{URL: "http://mimir-a.local", TenantID: "6f1d2c9e-9d3a-4c1b-8f47-2b0a5e7c1d34"},
		},
	}
	cfg, err := loadInstances(i)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := cfg.ValidateTenants(fakeRegistry{}); err == nil {
		t.Error("expected push target tenant references to be validated too")
	}
}
