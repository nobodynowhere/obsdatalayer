package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

var validName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Duration is a time.Duration that parses from YAML strings like "30s".
type Duration time.Duration

func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(dur)
	return nil
}

type Config struct {
	Version   int               `yaml:"version"`
	Gateway   GatewayConfig     `yaml:"gateway"`
	Instances []*InstanceConfig `yaml:"instances"`
	ByName    map[string]*InstanceConfig `yaml:"-"`
}

type GatewayConfig struct {
	Port     int           `yaml:"port"`
	Token    string        `yaml:"token"`
	Timeouts TimeoutConfig `yaml:"timeouts"`
}

type TimeoutConfig struct {
	Query Duration `yaml:"query"`
	Push  Duration `yaml:"push"`
}

type InstanceConfig struct {
	Name            string        `yaml:"name"`
	Backend         string        `yaml:"backend"`
	URL             string        `yaml:"url"`
	PushURLs        []PushTarget  `yaml:"push_urls"`
	FanOutMode      string        `yaml:"fan_out_mode"`
	BasicAuth       string        `yaml:"basic_auth"`
	TenantID        string        `yaml:"tenant_id"`
	AuthPassthrough bool          `yaml:"auth_passthrough"`
	Labels          *LabelsConfig `yaml:"labels"`
}

type PushTarget struct {
	URL       string `yaml:"url"`
	BasicAuth string `yaml:"basic_auth"`
	TenantID  string `yaml:"tenant_id"`
}

type LabelsConfig struct {
	Filter *FilterConfig     `yaml:"filter"`
	Inject map[string]string `yaml:"inject"`
}

type FilterConfig struct {
	Mode  string   `yaml:"mode"`
	Names []string `yaml:"names"`
}

// ResolvedTarget is a push target with effective auth (instance defaults applied).
type ResolvedTarget struct {
	URL       string
	BasicAuth string
	TenantID  string
}

// Load reads YAML config from path, validates it, builds ByName map, and applies defaults.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Apply defaults
	if cfg.Gateway.Port == 0 {
		cfg.Gateway.Port = 8080
	}
	if cfg.Gateway.Timeouts.Query == 0 {
		cfg.Gateway.Timeouts.Query = Duration(30 * time.Second)
	}
	if cfg.Gateway.Timeouts.Push == 0 {
		cfg.Gateway.Timeouts.Push = Duration(60 * time.Second)
	}
	for _, inst := range cfg.Instances {
		if inst.FanOutMode == "" && len(inst.PushURLs) > 0 {
			inst.FanOutMode = "any"
		}
	}

	if err := validate(&cfg); err != nil {
		return nil, err
	}

	cfg.ByName = make(map[string]*InstanceConfig, len(cfg.Instances))
	for _, inst := range cfg.Instances {
		cfg.ByName[inst.Name] = inst
	}

	return &cfg, nil
}

func validate(cfg *Config) error {
	// 1. version must be 1
	if cfg.Version != 1 {
		return fmt.Errorf("config: version must be 1, got %d", cfg.Version)
	}

	// 2. no instances
	if len(cfg.Instances) == 0 {
		return fmt.Errorf("config: no instances defined")
	}

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

		// 6. auth_passthrough with basic_auth or tenant_id
		if inst.AuthPassthrough && (inst.BasicAuth != "" || inst.TenantID != "") {
			return fmt.Errorf("config: instance %q has auth_passthrough:true but also sets basic_auth or tenant_id", inst.Name)
		}

		// 7. both url and push_urls set
		if inst.URL != "" && len(inst.PushURLs) > 0 {
			return fmt.Errorf("config: instance %q has both url and push_urls set", inst.Name)
		}

		// 8. neither url nor push_urls set
		if inst.URL == "" && len(inst.PushURLs) == 0 {
			return fmt.Errorf("config: instance %q has neither url nor push_urls set", inst.Name)
		}

		// 9. push_urls on Tempo instance
		if inst.Backend == "tempo" && len(inst.PushURLs) > 0 {
			return fmt.Errorf("config: instance %q is tempo backend and cannot have push_urls", inst.Name)
		}

		// 10. fan_out_mode declared without push_urls
		if inst.FanOutMode != "" && len(inst.PushURLs) == 0 {
			return fmt.Errorf("config: instance %q has fan_out_mode but no push_urls", inst.Name)
		}

		// 11. fan_out_mode not "any" or "all"
		if inst.FanOutMode != "" && inst.FanOutMode != "any" && inst.FanOutMode != "all" {
			return fmt.Errorf("config: instance %q has invalid fan_out_mode %q (must be any or all)", inst.Name, inst.FanOutMode)
		}

		// 12. labels on Tempo instance
		if inst.Backend == "tempo" && inst.Labels != nil {
			return fmt.Errorf("config: instance %q is tempo backend and cannot have labels config", inst.Name)
		}

		// 13. labels.filter.mode validation
		if inst.Labels != nil && inst.Labels.Filter != nil {
			switch inst.Labels.Filter.Mode {
			case "allowlist", "denylist":
			default:
				return fmt.Errorf("config: instance %q labels.filter.mode must be allowlist or denylist, got %q", inst.Name, inst.Labels.Filter.Mode)
			}
		}

		// 14. push_urls target missing url
		for i, pt := range inst.PushURLs {
			if pt.URL == "" {
				return fmt.Errorf("config: instance %q push_urls[%d] is missing url", inst.Name, i)
			}
		}

		// 15. per-target basic_auth/tenant_id when instance has auth_passthrough:true
		if inst.AuthPassthrough {
			for i, pt := range inst.PushURLs {
				if pt.BasicAuth != "" || pt.TenantID != "" {
					return fmt.Errorf("config: instance %q push_urls[%d] sets basic_auth/tenant_id but instance has auth_passthrough:true", inst.Name, i)
				}
			}
		}
	}

	return nil
}

// resolveTarget merges per-target auth with instance-level defaults.
func resolveTarget(pt PushTarget, instBasicAuth, instTenantID string) ResolvedTarget {
	basicAuth := pt.BasicAuth
	if basicAuth == "" {
		basicAuth = instBasicAuth
	}
	tenantID := pt.TenantID
	if tenantID == "" {
		tenantID = instTenantID
	}
	return ResolvedTarget{URL: pt.URL, BasicAuth: basicAuth, TenantID: tenantID}
}

// GetPushTargets returns resolved push targets with effective auth.
func (inst *InstanceConfig) GetPushTargets() []ResolvedTarget {
	if len(inst.PushURLs) > 0 {
		targets := make([]ResolvedTarget, len(inst.PushURLs))
		for i, pt := range inst.PushURLs {
			targets[i] = resolveTarget(pt, inst.BasicAuth, inst.TenantID)
		}
		return targets
	}
	return []ResolvedTarget{{URL: inst.URL, BasicAuth: inst.BasicAuth, TenantID: inst.TenantID}}
}

// GetQueryTarget returns the query target (first push target or single URL).
func (inst *InstanceConfig) GetQueryTarget() ResolvedTarget {
	if len(inst.PushURLs) > 0 {
		return resolveTarget(inst.PushURLs[0], inst.BasicAuth, inst.TenantID)
	}
	return ResolvedTarget{URL: inst.URL, BasicAuth: inst.BasicAuth, TenantID: inst.TenantID}
}
