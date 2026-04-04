package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"obsdatalayer/internal/config"
)

func writeConfig(t *testing.T, yaml string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const baseConfig = `
version: 1
gateway:
  port: 9090
  token: "secret"
instances:
  - name: loki-prod
    backend: loki
    url: http://loki.local
`

func TestValidMinimalConfig(t *testing.T) {
	path := writeConfig(t, baseConfig)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if cfg.Gateway.Port != 9090 {
		t.Errorf("expected port 9090, got %d", cfg.Gateway.Port)
	}
	if cfg.Gateway.Token != "secret" {
		t.Errorf("expected token 'secret', got %q", cfg.Gateway.Token)
	}
	if len(cfg.Instances) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(cfg.Instances))
	}
	if cfg.ByName == nil {
		t.Fatal("expected ByName map to be populated")
	}
	inst, ok := cfg.ByName["loki-prod"]
	if !ok {
		t.Fatal("expected ByName to contain 'loki-prod'")
	}
	if inst.Backend != "loki" {
		t.Errorf("expected backend 'loki', got %q", inst.Backend)
	}
}

func TestPortDefault(t *testing.T) {
	yaml := `
version: 1
gateway:
  token: "secret"
instances:
  - name: loki-prod
    backend: loki
    url: http://loki.local
`
	path := writeConfig(t, yaml)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if cfg.Gateway.Port != 8080 {
		t.Errorf("expected default port 8080, got %d", cfg.Gateway.Port)
	}
}

func TestTimeoutDefaults(t *testing.T) {
	path := writeConfig(t, baseConfig)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if cfg.Gateway.Timeouts.Query.Duration() != 30*time.Second {
		t.Errorf("expected query timeout 30s, got %v", cfg.Gateway.Timeouts.Query.Duration())
	}
	if cfg.Gateway.Timeouts.Push.Duration() != 60*time.Second {
		t.Errorf("expected push timeout 60s, got %v", cfg.Gateway.Timeouts.Push.Duration())
	}
}

func TestFanOutModeDefaultsToAny(t *testing.T) {
	yaml := `
version: 1
gateway:
  port: 9090
  token: "secret"
instances:
  - name: loki-prod
    backend: loki
    push_urls:
      - url: http://loki-a.local
      - url: http://loki-b.local
`
	path := writeConfig(t, yaml)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	inst := cfg.ByName["loki-prod"]
	if inst.FanOutMode != "any" {
		t.Errorf("expected fan_out_mode 'any', got %q", inst.FanOutMode)
	}
}

func TestVersionZeroError(t *testing.T) {
	yaml := `
version: 0
gateway:
  port: 9090
  token: "secret"
instances:
  - name: loki-prod
    backend: loki
    url: http://loki.local
`
	path := writeConfig(t, yaml)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for version=0")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("expected error about version, got: %v", err)
	}
}

func TestVersionTwoError(t *testing.T) {
	yaml := `
version: 2
gateway:
  port: 9090
  token: "secret"
instances:
  - name: loki-prod
    backend: loki
    url: http://loki.local
`
	path := writeConfig(t, yaml)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for version=2")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("expected error about version, got: %v", err)
	}
}

func TestVersionAbsentError(t *testing.T) {
	yaml := `
gateway:
  port: 9090
  token: "secret"
instances:
  - name: loki-prod
    backend: loki
    url: http://loki.local
`
	path := writeConfig(t, yaml)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error when version field is absent")
	}
}

func TestNoInstancesError(t *testing.T) {
	yaml := `
version: 1
gateway:
  port: 9090
  token: "secret"
instances: []
`
	path := writeConfig(t, yaml)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for no instances")
	}
	if !strings.Contains(err.Error(), "no instances") {
		t.Errorf("expected error about no instances, got: %v", err)
	}
}

func TestDuplicateInstanceNameError(t *testing.T) {
	yaml := `
version: 1
gateway:
  port: 9090
  token: "secret"
instances:
  - name: loki-prod
    backend: loki
    url: http://loki.local
  - name: loki-prod
    backend: loki
    url: http://loki2.local
`
	path := writeConfig(t, yaml)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for duplicate instance name")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected error about duplicate, got: %v", err)
	}
}

func TestInvalidInstanceNameSpace(t *testing.T) {
	yaml := `
version: 1
gateway:
  port: 9090
  token: "secret"
instances:
  - name: "loki prod"
    backend: loki
    url: http://loki.local
`
	path := writeConfig(t, yaml)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for instance name with space")
	}
}

func TestInvalidInstanceNameDot(t *testing.T) {
	yaml := `
version: 1
gateway:
  port: 9090
  token: "secret"
instances:
  - name: "loki.prod"
    backend: loki
    url: http://loki.local
`
	path := writeConfig(t, yaml)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for instance name with dot")
	}
}

func TestInvalidInstanceNameSlash(t *testing.T) {
	yaml := `
version: 1
gateway:
  port: 9090
  token: "secret"
instances:
  - name: "loki/prod"
    backend: loki
    url: http://loki.local
`
	path := writeConfig(t, yaml)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for instance name with slash")
	}
}

func TestUnknownBackendError(t *testing.T) {
	yaml := `
version: 1
gateway:
  port: 9090
  token: "secret"
instances:
  - name: kafka-prod
    backend: kafka
    url: http://kafka.local
`
	path := writeConfig(t, yaml)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for unknown backend 'kafka'")
	}
	if !strings.Contains(err.Error(), "kafka") {
		t.Errorf("expected error mentioning 'kafka', got: %v", err)
	}
}

func TestAuthPassthroughWithBasicAuthError(t *testing.T) {
	yaml := `
version: 1
gateway:
  port: 9090
  token: "secret"
instances:
  - name: loki-prod
    backend: loki
    url: http://loki.local
    auth_passthrough: true
    basic_auth: "user:pass"
`
	path := writeConfig(t, yaml)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for auth_passthrough + basic_auth")
	}
	if !strings.Contains(err.Error(), "auth_passthrough") {
		t.Errorf("expected error about auth_passthrough, got: %v", err)
	}
}

func TestAuthPassthroughWithTenantIDError(t *testing.T) {
	yaml := `
version: 1
gateway:
  port: 9090
  token: "secret"
instances:
  - name: loki-prod
    backend: loki
    url: http://loki.local
    auth_passthrough: true
    tenant_id: "my-tenant"
`
	path := writeConfig(t, yaml)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for auth_passthrough + tenant_id")
	}
}

func TestBothURLAndPushURLsError(t *testing.T) {
	yaml := `
version: 1
gateway:
  port: 9090
  token: "secret"
instances:
  - name: loki-prod
    backend: loki
    url: http://loki.local
    push_urls:
      - url: http://loki-b.local
`
	path := writeConfig(t, yaml)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for both url and push_urls")
	}
	if !strings.Contains(err.Error(), "both") {
		t.Errorf("expected error about both url and push_urls, got: %v", err)
	}
}

func TestNeitherURLNorPushURLsError(t *testing.T) {
	yaml := `
version: 1
gateway:
  port: 9090
  token: "secret"
instances:
  - name: loki-prod
    backend: loki
`
	path := writeConfig(t, yaml)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for neither url nor push_urls")
	}
	if !strings.Contains(err.Error(), "neither") {
		t.Errorf("expected error about neither url nor push_urls, got: %v", err)
	}
}

func TestPushURLsOnTempoError(t *testing.T) {
	yaml := `
version: 1
gateway:
  port: 9090
  token: "secret"
instances:
  - name: tempo-prod
    backend: tempo
    push_urls:
      - url: http://tempo.local
`
	path := writeConfig(t, yaml)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for push_urls on tempo")
	}
	if !strings.Contains(err.Error(), "tempo") {
		t.Errorf("expected error about tempo backend, got: %v", err)
	}
}

func TestFanOutModeWithoutPushURLsError(t *testing.T) {
	yaml := `
version: 1
gateway:
  port: 9090
  token: "secret"
instances:
  - name: loki-prod
    backend: loki
    url: http://loki.local
    fan_out_mode: any
`
	path := writeConfig(t, yaml)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for fan_out_mode without push_urls")
	}
	if !strings.Contains(err.Error(), "fan_out_mode") {
		t.Errorf("expected error about fan_out_mode, got: %v", err)
	}
}

func TestFanOutModeBadValueError(t *testing.T) {
	yaml := `
version: 1
gateway:
  port: 9090
  token: "secret"
instances:
  - name: loki-prod
    backend: loki
    fan_out_mode: bad
    push_urls:
      - url: http://loki-a.local
`
	path := writeConfig(t, yaml)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for fan_out_mode='bad'")
	}
	if !strings.Contains(err.Error(), "fan_out_mode") {
		t.Errorf("expected error about fan_out_mode, got: %v", err)
	}
}

func TestLabelsOnTempoError(t *testing.T) {
	yaml := `
version: 1
gateway:
  port: 9090
  token: "secret"
instances:
  - name: tempo-prod
    backend: tempo
    url: http://tempo.local
    labels:
      inject:
        env: prod
`
	path := writeConfig(t, yaml)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for labels on tempo")
	}
	if !strings.Contains(err.Error(), "tempo") {
		t.Errorf("expected error about tempo backend, got: %v", err)
	}
}

func TestLabelsFilterModeRandomError(t *testing.T) {
	yaml := `
version: 1
gateway:
  port: 9090
  token: "secret"
instances:
  - name: loki-prod
    backend: loki
    url: http://loki.local
    labels:
      filter:
        mode: random
        names: [app]
`
	path := writeConfig(t, yaml)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for filter.mode='random'")
	}
	if !strings.Contains(err.Error(), "allowlist or denylist") {
		t.Errorf("expected error about allowlist or denylist, got: %v", err)
	}
}

func TestPushURLsTargetMissingURLError(t *testing.T) {
	yaml := `
version: 1
gateway:
  port: 9090
  token: "secret"
instances:
  - name: loki-prod
    backend: loki
    push_urls:
      - url: ""
`
	path := writeConfig(t, yaml)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for push_urls target missing url")
	}
	if !strings.Contains(err.Error(), "missing url") {
		t.Errorf("expected error about missing url, got: %v", err)
	}
}

func TestPerTargetBasicAuthWithAuthPassthroughError(t *testing.T) {
	yaml := `
version: 1
gateway:
  port: 9090
  token: "secret"
instances:
  - name: loki-prod
    backend: loki
    auth_passthrough: true
    push_urls:
      - url: http://loki-a.local
        basic_auth: "user:pass"
`
	path := writeConfig(t, yaml)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for per-target basic_auth with auth_passthrough")
	}
}

func TestPerTargetTenantIDWithAuthPassthroughError(t *testing.T) {
	yaml := `
version: 1
gateway:
  port: 9090
  token: "secret"
instances:
  - name: loki-prod
    backend: loki
    auth_passthrough: true
    push_urls:
      - url: http://loki-a.local
        tenant_id: "my-tenant"
`
	path := writeConfig(t, yaml)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for per-target tenant_id with auth_passthrough")
	}
}

func TestFanOutModeAllIsValid(t *testing.T) {
	yaml := `
version: 1
gateway:
  port: 9090
  token: "secret"
instances:
  - name: loki-prod
    backend: loki
    fan_out_mode: all
    push_urls:
      - url: http://loki-a.local
      - url: http://loki-b.local
`
	path := writeConfig(t, yaml)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	inst := cfg.ByName["loki-prod"]
	if inst.FanOutMode != "all" {
		t.Errorf("expected fan_out_mode 'all', got %q", inst.FanOutMode)
	}
}

func TestGetPushTargetsPerTargetAuthOverride(t *testing.T) {
	inst := &config.InstanceConfig{
		Name:      "loki-prod",
		Backend:   "loki",
		BasicAuth: "default:pass",
		TenantID:  "default-tenant",
		PushURLs: []config.PushTarget{
			{URL: "http://loki-a.local", BasicAuth: "override:secret", TenantID: "tenant-a"},
			{URL: "http://loki-b.local"},
		},
	}

	targets := inst.GetPushTargets()
	if len(targets) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(targets))
	}

	// First target should use per-target overrides
	if targets[0].BasicAuth != "override:secret" {
		t.Errorf("expected per-target basic_auth 'override:secret', got %q", targets[0].BasicAuth)
	}
	if targets[0].TenantID != "tenant-a" {
		t.Errorf("expected per-target tenant_id 'tenant-a', got %q", targets[0].TenantID)
	}
}

func TestGetPushTargetsFallbackToInstanceAuth(t *testing.T) {
	inst := &config.InstanceConfig{
		Name:      "loki-prod",
		Backend:   "loki",
		BasicAuth: "default:pass",
		TenantID:  "default-tenant",
		PushURLs: []config.PushTarget{
			{URL: "http://loki-b.local"},
		},
	}

	targets := inst.GetPushTargets()
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}

	// Should fall back to instance-level auth
	if targets[0].BasicAuth != "default:pass" {
		t.Errorf("expected fallback basic_auth 'default:pass', got %q", targets[0].BasicAuth)
	}
	if targets[0].TenantID != "default-tenant" {
		t.Errorf("expected fallback tenant_id 'default-tenant', got %q", targets[0].TenantID)
	}
}

func TestGetPushTargetsSingleURL(t *testing.T) {
	inst := &config.InstanceConfig{
		Name:      "loki-prod",
		Backend:   "loki",
		URL:       "http://loki.local",
		BasicAuth: "user:pass",
		TenantID:  "my-tenant",
	}

	targets := inst.GetPushTargets()
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	if targets[0].URL != "http://loki.local" {
		t.Errorf("expected URL 'http://loki.local', got %q", targets[0].URL)
	}
	if targets[0].BasicAuth != "user:pass" {
		t.Errorf("expected basic_auth 'user:pass', got %q", targets[0].BasicAuth)
	}
	if targets[0].TenantID != "my-tenant" {
		t.Errorf("expected tenant_id 'my-tenant', got %q", targets[0].TenantID)
	}
}

func TestGetQueryTargetReturnsPushTarget(t *testing.T) {
	inst := &config.InstanceConfig{
		Name:    "loki-prod",
		Backend: "loki",
		PushURLs: []config.PushTarget{
			{URL: "http://loki-a.local"},
			{URL: "http://loki-b.local"},
		},
	}

	qt := inst.GetQueryTarget()
	if qt.URL != "http://loki-a.local" {
		t.Errorf("expected query target URL 'http://loki-a.local', got %q", qt.URL)
	}
}
