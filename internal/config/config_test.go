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
	if cfg.Gateway.Server.ReadHeaderTimeout.Duration() != 5*time.Second {
		t.Errorf("expected read header timeout 5s, got %v", cfg.Gateway.Server.ReadHeaderTimeout.Duration())
	}
	if cfg.Gateway.Server.IdleTimeout.Duration() != 120*time.Second {
		t.Errorf("expected idle timeout 120s, got %v", cfg.Gateway.Server.IdleTimeout.Duration())
	}
	if cfg.Gateway.Transport.MaxIdleConns != 10000 {
		t.Errorf("expected 10000 upstream idle connections, got %d", cfg.Gateway.Transport.MaxIdleConns)
	}
	if cfg.Gateway.Transport.MaxIdleConnsPerHost != 10000 {
		t.Errorf("expected 10000 upstream idle connections per host, got %d", cfg.Gateway.Transport.MaxIdleConnsPerHost)
	}
	if cfg.Gateway.Transport.MaxConnsPerHost != 0 {
		t.Errorf("expected unlimited upstream active connections per host, got %d", cfg.Gateway.Transport.MaxConnsPerHost)
	}
	if cfg.Gateway.Transport.IdleConnTimeout.Duration() != 90*time.Second {
		t.Errorf("expected upstream idle timeout 90s, got %v", cfg.Gateway.Transport.IdleConnTimeout.Duration())
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
		{"negative server timeout", func(g *config.GatewayConfig) { g.Server.ReadHeaderTimeout = -1 }, true},
		{"negative transport limit", func(g *config.GatewayConfig) { g.Transport.MaxIdleConns = -1 }, true},
		{"negative active transport limit", func(g *config.GatewayConfig) { g.Transport.MaxConnsPerHost = -1 }, true},
		{"negative transport idle timeout", func(g *config.GatewayConfig) { g.Transport.IdleConnTimeout = -1 }, true},
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
			"tempo push_urls valid",
			&config.InstanceConfig{Name: "t", Backend: "tempo",
				PushURLs: []config.PushTarget{{URL: "http://t.local"}}},
			"",
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
			{URL: "http://a.local", BasicAuth: "override:secret"},
			{URL: "http://b.local"},
		},
	}
	targets := i.GetPushTargets()
	if len(targets) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(targets))
	}
	// The credential is a per-target override; the tenant is the instance's on
	// every target, because a target has no tenant of its own to override with.
	if targets[0].BasicAuth != "override:secret" || targets[0].TenantID != "default-tenant" || !targets[0].SkipTLSVerify {
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

// This once asserted that a tenant declared on a push target was validated too.
// Targets no longer carry one, so what has to stay covered is the fan-out form
// itself: an instance configured with push_urls rather than a single url still
// has its own tenant reference checked against the registry.
func TestValidateTenantsCoversFanOutInstances(t *testing.T) {
	i := &config.InstanceConfig{
		Name: "mimir-prod", Backend: "mimir",
		TenantID: "6f1d2c9e-9d3a-4c1b-8f47-2b0a5e7c1d34",
		PushURLs: []config.PushTarget{
			{URL: "http://mimir-a.local"},
			{URL: "http://mimir-b.local"},
		},
	}
	cfg, err := loadInstances(i)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := cfg.ValidateTenants(fakeRegistry{}); err == nil {
		t.Error("expected the instance tenant reference to be validated")
	}
	known := fakeRegistry{"6f1d2c9e-9d3a-4c1b-8f47-2b0a5e7c1d34": true}
	if err := cfg.ValidateTenants(known); err != nil {
		t.Errorf("a known tenant should validate, got: %v", err)
	}
}

// ---- target groups ----------------------------------------------------------

// TestTargetGroupsSplitTheSurfaces is the point of groups: Tempo serves ingest
// receivers and its HTTP API on different listeners, so one instance has to be
// able to address them without a proxy in front merging every port.
func TestTargetGroupsSplitTheSurfaces(t *testing.T) {
	i := &config.InstanceConfig{
		Name: "tempo-prod", Backend: "tempo",
		PushURLs: []config.PushTarget{
			{URL: "http://tempo-otlp.local:4318", Group: config.TargetGroupOTLPHTTP},
			{URL: "http://tempo-grpc.local:4317", Group: config.TargetGroupOTLPGRPC},
			{URL: "http://tempo-jaeger.local:14268", Group: config.TargetGroupJaeger},
			{URL: "http://tempo-zipkin.local:9411", Group: config.TargetGroupZipkin},
			{URL: "http://tempo-api.local:3200", Group: config.TargetGroupQuery},
		},
	}

	otlp := i.GetTargets(config.TargetGroupOTLPHTTP)
	if len(otlp) != 1 || otlp[0].URL != "http://tempo-otlp.local:4318" {
		t.Errorf("OTLP targets should be the OTLP receivers only, got %+v", otlp)
	}
	otlpGRPC := i.GetTargets(config.TargetGroupOTLPGRPC)
	if len(otlpGRPC) != 1 || otlpGRPC[0].URL != "http://tempo-grpc.local:4317" {
		t.Errorf("OTLP gRPC targets should be the OTLP gRPC receivers only, got %+v", otlpGRPC)
	}
	jaeger := i.GetTargets(config.TargetGroupJaeger)
	if len(jaeger) != 1 || jaeger[0].URL != "http://tempo-jaeger.local:14268" {
		t.Errorf("Jaeger targets should be the Jaeger receivers only, got %+v", jaeger)
	}
	zipkin := i.GetTargets(config.TargetGroupZipkin)
	if len(zipkin) != 1 || zipkin[0].URL != "http://tempo-zipkin.local:9411" {
		t.Errorf("Zipkin targets should be the Zipkin receivers only, got %+v", zipkin)
	}
	read := i.GetReadTargets()
	if len(read) != 1 || read[0].URL != "http://tempo-api.local:3200" {
		t.Errorf("read targets should be the HTTP API only, got %+v", read)
	}
	if q := i.GetQueryTarget(); q.URL != "http://tempo-api.local:3200" {
		t.Errorf("query target should follow the query role, got %+v", q)
	}
}

// TestTargetsWithoutGroupsAreUnchanged is the compatibility guarantee. Every
// instance configured before groups existed declared no group at all, and must
// keep sending every HTTP surface to the same targets unless a specific group
// is introduced.
func TestTargetsWithoutGroupsAreUnchanged(t *testing.T) {
	i := &config.InstanceConfig{
		Name: "loki-prod", Backend: "loki",
		PushURLs: []config.PushTarget{{URL: "http://a.local"}, {URL: "http://b.local"}},
	}
	push, read := i.GetPushTargets(), i.GetReadTargets()
	if len(push) != 2 || len(read) != 2 {
		t.Fatalf("expected both surfaces to keep all targets, got %d push and %d read", len(push), len(read))
	}
	for n := range push {
		if push[n].URL != read[n].URL {
			t.Errorf("target %d differs between surfaces: %q vs %q", n, push[n].URL, read[n].URL)
		}
	}
	otlp := i.GetTargets(config.TargetGroupOTLPHTTP)
	if len(otlp) != 2 || otlp[0].URL != "http://a.local" || otlp[1].URL != "http://b.local" {
		t.Errorf("ungrouped targets should serve OTLP too, got %+v", otlp)
	}
}

// TestQueryOnlyInstanceHasNoIngestTarget covers the configuration that is legal
// but cannot ingest. The gateway answers at request time rather than refusing
// the configuration, so a read-only instance is expressible.
func TestQueryOnlyInstanceHasNoIngestTarget(t *testing.T) {
	i := &config.InstanceConfig{
		Name: "tempo-ro", Backend: "tempo",
		PushURLs: []config.PushTarget{{URL: "http://tempo-api.local:3200", Group: config.TargetGroupQuery}},
	}
	if push := i.GetPushTargets(); len(push) != 0 {
		t.Errorf("a query-only instance must have no push target, got %+v", push)
	}
	if read := i.GetReadTargets(); len(read) != 1 {
		t.Errorf("expected the query target to serve reads, got %+v", read)
	}
}

func TestQueryTargetsFallBackToPushTargets(t *testing.T) {
	i := &config.InstanceConfig{
		Name: "tempo-prod", Backend: "tempo",
		PushURLs: []config.PushTarget{{URL: "http://tempo-distributor.local:4318", Group: config.TargetGroupPush}},
	}
	targets := i.GetTargets(config.TargetGroupQuery)
	if len(targets) != 1 || targets[0].URL != "http://tempo-distributor.local:4318" {
		t.Errorf("query targets should fall back to generic push targets, got %+v", targets)
	}
	if read := i.GetReadTargets(); len(read) != 1 || read[0].URL != targets[0].URL {
		t.Errorf("read targets should match GetTargets(query), got %+v", read)
	}
}

func TestGenericPushGroupIsIngestFallback(t *testing.T) {
	i := &config.InstanceConfig{
		Name: "tempo-prod", Backend: "tempo",
		PushURLs: []config.PushTarget{{URL: "http://tempo-distributor.local:4318", Group: config.TargetGroupPush}},
	}
	otlp := i.GetTargets(config.TargetGroupOTLPHTTP)
	if len(otlp) != 1 || otlp[0].URL != "http://tempo-distributor.local:4318" {
		t.Errorf("specific ingest groups should fall back to generic push, got %+v", otlp)
	}
}

func TestUnknownTargetGroupIsRejected(t *testing.T) {
	i := &config.InstanceConfig{
		Name: "tempo-prod", Backend: "tempo",
		PushURLs: []config.PushTarget{{URL: "http://a.local", Group: "ingest"}},
	}
	_, err := loadInstances(i)
	if err == nil {
		t.Fatal("expected an unknown group to be rejected")
	}
	if !strings.Contains(err.Error(), "push_urls[0]") || !strings.Contains(err.Error(), "ingest") {
		t.Errorf("expected the error to identify the target and the bad group, got: %v", err)
	}
}

// ---- backend-specific groups ------------------------------------------------

// Groups name receiver surfaces, and the three backends do not have the same
// ones. Tempo's distributor is a set of OpenTelemetry receivers on their own
// ports; Mimir and Loki serve their OTLP paths on the same listener as their
// native push paths, so a receiver-specific group there names a surface that
// does not exist and would never be routed to.
func TestGroupsAreBackendSpecific(t *testing.T) {
	cases := []struct {
		backend, group string
		allowed        bool
	}{
		{"tempo", config.TargetGroupOTLPHTTP, true},
		{"tempo", config.TargetGroupOTLPGRPC, true},
		{"tempo", config.TargetGroupJaeger, true},
		{"tempo", config.TargetGroupZipkin, true},
		{"tempo", config.TargetGroupQuery, true},
		{"mimir", config.TargetGroupQuery, true},
		{"mimir", config.TargetGroupPush, true},
		{"mimir", config.TargetGroupOTLPGRPC, false},
		{"mimir", config.TargetGroupOTLPHTTP, false},
		{"mimir", config.TargetGroupJaeger, false},
		{"loki", config.TargetGroupOTLPGRPC, false},
		{"loki", config.TargetGroupOTLPHTTP, false},
		{"loki", config.TargetGroupZipkin, false},
		// The legacy fallback is valid everywhere and is not a choice.
		{"loki", "", true},
		{"mimir", "", true},
		{"tempo", "", true},
	}
	for _, tc := range cases {
		if got := config.BackendAllowsGroup(tc.backend, tc.group); got != tc.allowed {
			t.Errorf("BackendAllowsGroup(%q, %q) = %v, want %v", tc.backend, tc.group, got, tc.allowed)
		}
	}
}

func TestReceiverGroupOnMimirIsRejected(t *testing.T) {
	i := &config.InstanceConfig{
		Name: "mimir-prod", Backend: "mimir",
		PushURLs: []config.PushTarget{{URL: "http://mimir.local", Group: config.TargetGroupJaeger}},
	}
	_, err := loadInstances(i)
	if err == nil {
		t.Fatal("expected a Jaeger group on a Mimir instance to be rejected")
	}
	// The message has to name the offending target and what is allowed, or an
	// operator cannot tell a typo from an unsupported surface.
	for _, want := range []string{"push_urls[0]", "jaeger", "mimir", "query"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// ---- tenancy is an instance property ----------------------------------------

// Targets carry the instance's tenant, always. With target groups an instance's
// targets can be different surfaces of one backend, where two tenants would
// mean writing as one and reading as the other -- so the config has no way to
// say it.
func TestEveryTargetCarriesTheInstanceTenant(t *testing.T) {
	i := &config.InstanceConfig{
		Name: "tempo-prod", Backend: "tempo", TenantID: "acme",
		PushURLs: []config.PushTarget{
			{URL: "http://tempo.local:4318", Group: config.TargetGroupOTLPHTTP},
			{URL: "http://tempo.local:3200", Group: config.TargetGroupQuery},
		},
	}
	for _, group := range []string{config.TargetGroupOTLPHTTP, config.TargetGroupQuery} {
		for _, target := range i.GetTargets(group) {
			if target.TenantID != "acme" {
				t.Errorf("group %q target %q carried tenant %q, want the instance's", group, target.URL, target.TenantID)
			}
		}
	}
	if q := i.GetQueryTarget(); q.TenantID != "acme" {
		t.Errorf("query target carried tenant %q, want the instance's", q.TenantID)
	}
}
