package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	"obsdatalayer/internal/authlimit"
)

var validName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Duration is a time.Duration that parses from YAML strings like "30s".
type Duration time.Duration

func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

func (d *Duration) UnmarshalYAML(b []byte) error {
	var s string
	if err := yaml.Unmarshal(b, &s); err != nil {
		return err
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(dur)
	return nil
}
func (d *Duration) UnmarshalText(text []byte) error {
	dur, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", string(text), err)
	}
	*d = Duration(dur)
	return nil
}

// MarshalYAML emits durations as human-readable strings like "30s".
func (d Duration) MarshalYAML() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

// Config is the runtime configuration read from the database. It is a
// projection of the database tables, not a user-authored document.
type Config struct {
	Gateway   GatewayConfig              `yaml:"gateway"`
	Instances []*InstanceConfig          `yaml:"instances"`
	ByName    map[string]*InstanceConfig `yaml:"-"`
}

// TenantRegistry validates that tenant references name registered tenants.
// Implemented by tenant.Store; kept as an interface so config does not depend
// on the tenant package's concrete type.
type TenantRegistry interface {
	ValidateAll(refs []string) error
}

// tenantRefs returns every tenant reference in the config.
func (c *Config) tenantRefs() []string {
	var refs []string
	for _, inst := range c.Instances {
		if inst.TenantID != "" {
			refs = append(refs, inst.TenantID)
		}
	}
	return refs
}

// ValidateTenants rejects a config that references an unregistered tenant.
func (c *Config) ValidateTenants(reg TenantRegistry) error {
	if reg == nil {
		return nil
	}
	if err := reg.ValidateAll(c.tenantRefs()); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}

// GatewayConfig is the runtime gateway configuration stored in the database.
// Listener addresses are deliberately absent: they are process-level concerns
// owned by the bootstrap file (see GatewayBootstrap), not hot-reloadable state.
type GatewayConfig struct {
	MaxBodyBytes   int64           `yaml:"max_body_bytes"`
	Timeouts       TimeoutConfig   `yaml:"timeouts"`
	Server         ServerConfig    `yaml:"server"`
	Transport      TransportConfig `yaml:"upstream_transport"`
	LogLevel       string          `yaml:"log_level"`
	ReloadInterval Duration        `yaml:"reload_interval"`
	AuthLimit      AuthLimitConfig `yaml:"auth_limit"`

	// DefaultTargetTimeout bounds one attempt against a target that does not
	// set its own timeout. It is the fallback for PushTarget.TimeoutSeconds.
	DefaultTargetTimeout Duration `yaml:"default_target_timeout"`

	// MetricsUnauthenticated drops the admin credential requirement from
	// GET /metrics on the admin port, so a Prometheus that cannot hold a
	// credential can still scrape it. Off by default, and deliberately so:
	// the exported series carry instance names and upstream target URLs.
	// Only /metrics is exempted -- every other admin route, /healthz
	// included, still requires an admin grant.
	MetricsUnauthenticated bool `yaml:"metrics_unauthenticated"`
}

// AuthLimitConfig bounds the cost an unauthenticated caller can impose.
// See package authlimit for why both a per-source throttle and a global
// hashing cap are needed.
type AuthLimitConfig struct {
	// Enabled controls the per-source failure throttle only. It is a pointer so
	// that a settings row predating this feature reads as nil and takes the
	// default rather than reading as false and silently disabling the defence.
	Enabled *bool `yaml:"enabled"`

	FailureThreshold int      `yaml:"failure_threshold"`
	FailureWindow    Duration `yaml:"failure_window"`
	BlockDuration    Duration `yaml:"block_duration"`
	MaxBlockDuration Duration `yaml:"max_block_duration"`

	// MaxConcurrentHashes caps concurrent password hashing across the process.
	// Zero takes the default; a negative value removes the cap.
	MaxConcurrentHashes int      `yaml:"max_concurrent_hashes"`
	HashWait            Duration `yaml:"hash_wait"`
}

// ThrottleEnabled reports whether per-source throttling is on, defaulting to on.
func (a AuthLimitConfig) ThrottleEnabled() bool {
	return a.Enabled == nil || *a.Enabled
}

// HashConcurrency resolves the configured hashing cap: zero means the default,
// negative means unlimited.
func (a AuthLimitConfig) HashConcurrency() int {
	if a.MaxConcurrentHashes < 0 {
		return 0
	}
	if a.MaxConcurrentHashes == 0 {
		return authlimit.DefaultMaxConcurrentHashes()
	}
	return a.MaxConcurrentHashes
}

// Limiter renders the config into the form the limiter takes.
func (a AuthLimitConfig) Limiter() authlimit.Config {
	return authlimit.Config{
		Enabled:          a.ThrottleEnabled(),
		FailureThreshold: a.FailureThreshold,
		FailureWindow:    a.FailureWindow.Duration(),
		BlockDuration:    a.BlockDuration.Duration(),
		MaxBlockDuration: a.MaxBlockDuration.Duration(),
	}
}

type TimeoutConfig struct {
	Query Duration `yaml:"query"`
	Push  Duration `yaml:"push"`
}

type ServerConfig struct {
	ReadHeaderTimeout Duration `yaml:"read_header_timeout"`
	IdleTimeout       Duration `yaml:"idle_timeout"`
}

type TransportConfig struct {
	MaxIdleConns        int      `yaml:"max_idle_conns"`
	MaxIdleConnsPerHost int      `yaml:"max_idle_conns_per_host"`
	MaxConnsPerHost     int      `yaml:"max_conns_per_host"`
	IdleConnTimeout     Duration `yaml:"idle_conn_timeout"`
}

type InstanceConfig struct {
	Name          string        `yaml:"name"`
	Backend       string        `yaml:"backend"`
	URL           string        `yaml:"url"`
	PushURLs      []PushTarget  `yaml:"push_urls"`
	FanOutMode    string        `yaml:"fan_out_mode"`
	BasicAuth     string        `yaml:"basic_auth"`
	TenantID      string        `yaml:"tenant_id"`
	SkipTLSVerify bool          `yaml:"skip_tls_verify"`
	Labels        *LabelsConfig `yaml:"labels"`
}

type PushTarget struct {
	URL       string `yaml:"url"`
	BasicAuth string `yaml:"basic_auth"`
	// TenantID is derived, not configured: resolveTarget fills it from the
	// instance, and it has no yaml tag because there is nothing for an operator
	// to set here. Tenancy is a property of the instance -- every target of an
	// instance addresses the same backend, and with target groups they may be
	// different surfaces of the same process, where two tenants would mean
	// writing as one and reading as the other.
	TenantID string `yaml:"-"`
	// Group selects the upstream surface this target serves. Empty means a
	// legacy target that can serve every HTTP surface unless a more specific
	// group is declared. Which groups are meaningful depends on the backend;
	// see GroupsForBackend.
	Group         string `yaml:"group"`
	SkipTLSVerify bool   `yaml:"skip_tls_verify"`

	// TimeoutSeconds bounds a single attempt against this target. Targets are
	// independent systems and need not answer at the same speed -- a local
	// cluster and a remote DR site behind one instance are not comparable -- so
	// the allowance belongs to the target rather than to the instance.
	//
	// Zero means "use the gateway's default_target_timeout".
	TimeoutSeconds int `yaml:"timeout_seconds"`
}

// Timeout returns the effective per-attempt timeout for this target, falling
// back to the supplied default when the target does not set one.
func (pt PushTarget) Timeout(def time.Duration) time.Duration {
	if pt.TimeoutSeconds > 0 {
		return time.Duration(pt.TimeoutSeconds) * time.Second
	}
	return def
}

type LabelsConfig struct {
	Filter *FilterConfig     `yaml:"filter"`
	Inject map[string]string `yaml:"inject"`
}

type FilterConfig struct {
	Mode  string   `yaml:"mode"`
	Names []string `yaml:"names"`
}

// New validates and finalizes a Config in preparation for use.
func New(cfg *Config) (*Config, error) {
	applyDefaults(cfg)
	if err := validate(cfg); err != nil {
		return nil, err
	}
	cfg.ByName = buildByName(cfg.Instances)
	return cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Gateway.MaxBodyBytes == 0 {
		cfg.Gateway.MaxBodyBytes = 32 * 1024 * 1024 // 32 MiB
	}
	if cfg.Gateway.Timeouts.Query == 0 {
		cfg.Gateway.Timeouts.Query = Duration(30 * time.Second)
	}
	if cfg.Gateway.Timeouts.Push == 0 {
		cfg.Gateway.Timeouts.Push = Duration(60 * time.Second)
	}
	if cfg.Gateway.Server.ReadHeaderTimeout == 0 {
		cfg.Gateway.Server.ReadHeaderTimeout = Duration(5 * time.Second)
	}
	if cfg.Gateway.Server.IdleTimeout == 0 {
		cfg.Gateway.Server.IdleTimeout = Duration(120 * time.Second)
	}
	if cfg.Gateway.Transport.MaxIdleConns == 0 {
		cfg.Gateway.Transport.MaxIdleConns = 10000
	}
	if cfg.Gateway.Transport.MaxIdleConnsPerHost == 0 {
		cfg.Gateway.Transport.MaxIdleConnsPerHost = 10000
	}
	if cfg.Gateway.Transport.IdleConnTimeout == 0 {
		cfg.Gateway.Transport.IdleConnTimeout = Duration(90 * time.Second)
	}
	if cfg.Gateway.LogLevel == "" {
		cfg.Gateway.LogLevel = "info"
	}
	if cfg.Gateway.ReloadInterval == 0 {
		cfg.Gateway.ReloadInterval = Duration(30 * time.Second)
	}
	al := &cfg.Gateway.AuthLimit
	if al.FailureThreshold == 0 {
		al.FailureThreshold = authlimit.DefaultFailureThreshold
	}
	if al.FailureWindow == 0 {
		al.FailureWindow = Duration(authlimit.DefaultFailureWindow)
	}
	if al.BlockDuration == 0 {
		al.BlockDuration = Duration(authlimit.DefaultBlockDuration)
	}
	if al.MaxBlockDuration == 0 {
		al.MaxBlockDuration = Duration(authlimit.DefaultMaxBlockDuration)
	}
	if al.HashWait == 0 {
		al.HashWait = Duration(authlimit.DefaultHashWait)
	}
	if cfg.Gateway.DefaultTargetTimeout == 0 {
		cfg.Gateway.DefaultTargetTimeout = Duration(30 * time.Second)
	}
	for _, inst := range cfg.Instances {
		if inst.FanOutMode == "" && len(inst.PushURLs) > 0 {
			inst.FanOutMode = "any"
		}
	}
}

func buildByName(instances []*InstanceConfig) map[string]*InstanceConfig {
	byName := make(map[string]*InstanceConfig, len(instances))
	for _, inst := range instances {
		byName[inst.Name] = inst
	}
	return byName
}

// validateGateway checks the settings that apply to the gateway as a whole.
func validateGateway(g *GatewayConfig) error {
	if g.MaxBodyBytes < 0 {
		return fmt.Errorf("config: max_body_bytes must not be negative")
	}
	if g.Timeouts.Query <= 0 || g.Timeouts.Push <= 0 {
		return fmt.Errorf("config: timeouts must be positive")
	}
	if g.Server.ReadHeaderTimeout <= 0 || g.Server.IdleTimeout <= 0 {
		return fmt.Errorf("config: server timeouts must be positive")
	}
	if g.Transport.MaxIdleConns < 0 || g.Transport.MaxIdleConnsPerHost < 0 || g.Transport.MaxConnsPerHost < 0 {
		return fmt.Errorf("config: upstream transport connection limits must not be negative")
	}
	if g.Transport.IdleConnTimeout <= 0 {
		return fmt.Errorf("config: upstream_transport.idle_conn_timeout must be positive")
	}
	if g.ReloadInterval <= 0 {
		return fmt.Errorf("config: reload_interval must be positive")
	}
	if g.DefaultTargetTimeout <= 0 {
		return fmt.Errorf("config: default_target_timeout must be positive")
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(g.LogLevel)); err != nil {
		return fmt.Errorf("config: invalid log_level %q (debug, info, warn or error)", g.LogLevel)
	}
	return validateAuthLimit(&g.AuthLimit)
}

func validateAuthLimit(a *AuthLimitConfig) error {
	if a.FailureThreshold < 0 {
		return fmt.Errorf("config: auth_limit.failure_threshold must not be negative")
	}
	if a.FailureWindow < 0 || a.BlockDuration < 0 || a.MaxBlockDuration < 0 {
		return fmt.Errorf("config: auth_limit durations must not be negative")
	}
	if a.HashWait < 0 {
		return fmt.Errorf("config: auth_limit.hash_wait must not be negative")
	}
	// A cap below the block would silently never be reached, which reads as a
	// configuration the operator did not get.
	if a.MaxBlockDuration > 0 && a.BlockDuration > a.MaxBlockDuration {
		return fmt.Errorf("config: auth_limit.block_duration (%s) exceeds max_block_duration (%s)",
			a.BlockDuration.Duration(), a.MaxBlockDuration.Duration())
	}
	return nil
}

func validate(cfg *Config) error {
	if err := validateGateway(&cfg.Gateway); err != nil {
		return err
	}

	// An empty instance list is a legitimate state: a freshly installed gateway
	// has none until an operator adds them through the admin API. Rejecting it
	// would make removing the last instance an unrecoverable reload failure.
	names := make(map[string]struct{})
	for _, inst := range cfg.Instances {
		// 3. invalid name pattern (checked before duplicate to give clearer errors)
		if !validName.MatchString(inst.Name) {
			return fmt.Errorf("config: instance name %q must match [a-zA-Z0-9_-]+", inst.Name)
		}

		// 4. duplicate instance names
		if _, exists := names[inst.Name]; exists {
			return fmt.Errorf("config: duplicate instance name %q", inst.Name)
		}
		names[inst.Name] = struct{}{}

		// 5. unknown backend
		switch inst.Backend {
		case "loki", "mimir", "tempo":
		default:
			return fmt.Errorf("config: instance %q has unknown backend %q (must be loki, mimir, tempo)", inst.Name, inst.Backend)
		}

		// 6. both url and push_urls set
		if inst.URL != "" && len(inst.PushURLs) > 0 {
			return fmt.Errorf("config: instance %q has both url and push_urls set", inst.Name)
		}

		// 7. neither url nor push_urls set
		if inst.URL == "" && len(inst.PushURLs) == 0 {
			return fmt.Errorf("config: instance %q has neither url nor push_urls set", inst.Name)
		}

		// 9. fan_out_mode declared without push_urls
		if inst.FanOutMode != "" && len(inst.PushURLs) == 0 {
			return fmt.Errorf("config: instance %q has fan_out_mode but no push_urls", inst.Name)
		}

		// 10. fan_out_mode not "any" or "all"
		if inst.FanOutMode != "" && inst.FanOutMode != "any" && inst.FanOutMode != "all" {
			return fmt.Errorf("config: instance %q has invalid fan_out_mode %q (must be any or all)", inst.Name, inst.FanOutMode)
		}

		// 11. labels on Tempo instance
		if inst.Backend == "tempo" && inst.Labels != nil {
			return fmt.Errorf("config: instance %q is tempo backend and cannot have labels config", inst.Name)
		}

		// 12. labels.filter.mode validation
		if inst.Labels != nil && inst.Labels.Filter != nil {
			switch inst.Labels.Filter.Mode {
			case "allowlist", "denylist":
			default:
				return fmt.Errorf("config: instance %q labels.filter.mode must be allowlist or denylist, got %q", inst.Name, inst.Labels.Filter.Mode)
			}
		}

		// 12b. negative per-target timeout
		for i, pt := range inst.PushURLs {
			if pt.TimeoutSeconds < 0 {
				return fmt.Errorf("config: instance %q push_urls[%d] has a negative timeout_seconds", inst.Name, i)
			}
		}

		// 12b. push_urls target group, which is backend-specific
		for i, pt := range inst.PushURLs {
			if !BackendAllowsGroup(inst.Backend, pt.Group) {
				return fmt.Errorf("config: instance %q push_urls[%d] has group %q, which %s does not serve (must be one of: %s)",
					inst.Name, i, pt.Group, inst.Backend, strings.Join(GroupsForBackend(inst.Backend), ", "))
			}
		}

		// 13. push_urls target missing url
		for i, pt := range inst.PushURLs {
			if pt.URL == "" {
				return fmt.Errorf("config: instance %q push_urls[%d] is missing url", inst.Name, i)
			}
		}

		// 14. URL format validation
		if inst.URL != "" {
			if err := validateUpstreamURL(inst.URL); err != nil {
				return fmt.Errorf("config: instance %q url is invalid: %w", inst.Name, err)
			}
		}
		for i, pt := range inst.PushURLs {
			if err := validateUpstreamURL(pt.URL); err != nil {
				return fmt.Errorf("config: instance %q push_urls[%d] is invalid: %w", inst.Name, i, err)
			}
		}
	}

	return nil
}

// validateUpstreamURL rejects anything that cannot be used as an upstream base
// URL. url.ParseRequestURI alone is too permissive: it accepts bare paths
// ("/foo"), protocol-relative URLs ("//host/path") and arbitrary schemes
// ("ftp://x"), all of which parse cleanly here and then fail at request time.
func validateUpstreamURL(raw string) error {
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "http", "https":
	case "":
		return fmt.Errorf("%q is missing an http:// or https:// scheme", raw)
	default:
		return fmt.Errorf("unsupported scheme %q (must be http or https)", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%q has no host", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%q must not contain a query string or fragment", raw)
	}
	return nil
}

// resolveTarget merges per-target auth with instance-level defaults and stamps
// the instance's tenant onto the target.
//
// The tenant is stamped rather than merged because there is no per-target
// tenant to merge with: every target of an instance carries the instance's,
// unconditionally. That is what makes a mismatch between an instance's ingest
// and query surfaces unrepresentable rather than merely discouraged.
func resolveTarget(pt PushTarget, instBasicAuth, instTenantID string, instSkipTLSVerify bool) PushTarget {
	basicAuth := pt.BasicAuth
	if basicAuth == "" {
		basicAuth = instBasicAuth
	}
	tenantID := instTenantID
	return PushTarget{
		URL:            pt.URL,
		BasicAuth:      basicAuth,
		TenantID:       tenantID,
		Group:          pt.Group,
		SkipTLSVerify:  pt.SkipTLSVerify || instSkipTLSVerify,
		TimeoutSeconds: pt.TimeoutSeconds,
	}
}

// Target groups name the HTTP surface a target serves. Ungrouped targets are
// the compatibility path: they are used for any group that has no explicit
// targets, so old fan-out lists still serve both ingest and query.
const (
	TargetGroupPush     = "push"
	TargetGroupQuery    = "query"
	TargetGroupOTLPHTTP = "otlp_http"
	TargetGroupJaeger   = "jaeger"
	TargetGroupZipkin   = "zipkin"
)

// groupsByBackend is which groups mean anything for each backend, and the one
// place that mapping lives.
//
// It is not the same list for all three, because the receivers are not. Tempo's
// distributor is a set of OpenTelemetry receivers on their own ports, so OTLP
// HTTP, Jaeger and Zipkin are separately addressable there. Mimir and Loki
// serve their OTLP paths on the same distributor listener as their native push
// paths, so there is no second surface to name -- forwardMimirPush and
// forwardLokiPush always ask for the push group, and an otlp_http target on
// either backend would be a target no request ever reaches.
//
// Rejecting those is the point. A group that validates but is never routed to
// is worse than an error: the operator sees a saved configuration and no data.
var groupsByBackend = map[string][]string{
	"loki":  {TargetGroupPush, TargetGroupQuery},
	"mimir": {TargetGroupPush, TargetGroupQuery},
	"tempo": {TargetGroupPush, TargetGroupQuery, TargetGroupOTLPHTTP, TargetGroupJaeger, TargetGroupZipkin},
}

// GroupsForBackend returns the groups an operator may assign to a target of
// this backend, in menu order. The empty group is always allowed and is not
// listed: it is the legacy fallback rather than a choice.
func GroupsForBackend(backend string) []string {
	return append([]string(nil), groupsByBackend[backend]...)
}

// BackendAllowsGroup reports whether group is meaningful for backend. The empty
// group is valid everywhere and means legacy fallback.
func BackendAllowsGroup(backend, group string) bool {
	if group == "" {
		return true
	}
	for _, g := range groupsByBackend[backend] {
		if g == group {
			return true
		}
	}
	return false
}

// targetsForExactGroup returns resolved targets carrying exactly group, in
// configuration order.
func (inst *InstanceConfig) targetsForExactGroup(group string) []PushTarget {
	out := make([]PushTarget, 0, len(inst.PushURLs))
	for _, pt := range inst.PushURLs {
		if pt.Group != group {
			continue
		}
		out = append(out, resolveTarget(pt, inst.BasicAuth, inst.TenantID, inst.SkipTLSVerify))
	}
	return out
}

// GetTargets returns resolved targets for a named surface. A specific group
// wins, then the generic push group for non-push surfaces, then ungrouped
// legacy targets.
//
// The single-url form names one origin serving every surface.
func (inst *InstanceConfig) GetTargets(group string) []PushTarget {
	if len(inst.PushURLs) > 0 {
		if targets := inst.targetsForExactGroup(group); len(targets) > 0 {
			return targets
		}
		if group != TargetGroupPush {
			if targets := inst.targetsForExactGroup(TargetGroupPush); len(targets) > 0 {
				return targets
			}
		}
		if targets := inst.targetsForExactGroup(""); len(targets) > 0 {
			return targets
		}
		return nil
	}
	return []PushTarget{{URL: inst.URL, BasicAuth: inst.BasicAuth, TenantID: inst.TenantID, SkipTLSVerify: inst.SkipTLSVerify}}
}

// GetPushTargets returns resolved generic push targets with effective auth.
func (inst *InstanceConfig) GetPushTargets() []PushTarget {
	return inst.GetTargets(TargetGroupPush)
}

// GetReadTargets returns the candidates for a read, in preference order.
//
// Query-group targets are preferred when present. Otherwise reads fall back to
// generic push targets and then legacy ungrouped targets, which keeps older
// configurations working without a proxy that merges every upstream surface.
//
// What it does not survive is a target that is up but stale, which answers
// successfully with less data than its peers. That divergence comes from a push
// that failed against one target while succeeding against another, and belongs
// to the fan-out delivery contract rather than here.
func (inst *InstanceConfig) GetReadTargets() []PushTarget {
	return inst.GetTargets(TargetGroupQuery)
}

// GetQueryTarget returns the single target for a request that is not fanned
// out: the first read target, which is the first query-group target when the
// instance declares any and a fallback target otherwise.
//
// Writes to a backend's HTTP API -- the ruler, Tempo's overrides, Alertmanager
// configuration -- come through here rather than through GetPushTargets,
// because they address the API surface and not the ingest receivers. That is
// why this follows the query group: those writes belong on the same listener the
// queries go to.
//
// The zero value is returned when the instance has no target for the surface at
// all, which callers must check; its URL is empty and forwarding to it would
// produce a request to a relative path.
func (inst *InstanceConfig) GetQueryTarget() PushTarget {
	if targets := inst.GetReadTargets(); len(targets) > 0 {
		return targets[0]
	}
	return PushTarget{}
}
