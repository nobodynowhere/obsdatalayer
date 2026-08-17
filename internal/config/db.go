package config

import (
	"fmt"
	"time"

	"github.com/gofrs/uuid/v5"
	"gorm.io/gorm"

	dbstore "obsdatalayer/internal/db"
)

// LoadFromDB loads the active Config from the database and applies bootstrap overrides.
func LoadFromDB(db *gorm.DB, bootstrap *Bootstrap) (*Config, error) {
	var setting dbstore.GatewaySetting
	if err := db.First(&setting).Error; err != nil {
		return nil, fmt.Errorf("load gateway settings: %w", err)
	}

	var instances []dbstore.Instance
	if err := db.
		Preload("PushTargets").
		Preload("LabelsGroup.Filter.Names").
		Preload("LabelsGroup.Injects").
		Find(&instances).Error; err != nil {
		return nil, fmt.Errorf("load instances: %w", err)
	}

	cfg, err := mapConfig(&setting, instances)
	if err != nil {
		return nil, err
	}

	cfg.Gateway.Port = bootstrap.Gateway.Port
	cfg.Gateway.AdminPort = bootstrap.Gateway.AdminPort

	return New(cfg)
}

// SeedFromYAML parses YAML and writes the resulting config into the database.
func SeedFromYAML(db *gorm.DB, data []byte) error {
	cfg, err := LoadYAML(data)
	if err != nil {
		return err
	}
	return SeedFromConfig(db, cfg)
}

// SeedFromConfig writes cfg into the database as the seed configuration.
// It should normally only be called when the database is empty.
func SeedFromConfig(db *gorm.DB, cfg *Config) error {
	cfg, err := New(cfg)
	if err != nil {
		return err
	}

	return db.Transaction(func(tx *gorm.DB) error {
		settingID, err := uuid.NewV4()
		if err != nil {
			return fmt.Errorf("generate setting id: %w", err)
		}

		setting := dbstore.GatewaySetting{
			ID:           settingID,
			Version:      cfg.Version,
			MaxBodyBytes: cfg.Gateway.MaxBodyBytes,
			QueryTimeout: durationString(cfg.Gateway.Timeouts.Query),
			PushTimeout:  durationString(cfg.Gateway.Timeouts.Push),
		}
		if err := tx.Create(&setting).Error; err != nil {
			return fmt.Errorf("create gateway setting: %w", err)
		}

		for _, inst := range cfg.Instances {
			if err := saveInstance(tx, inst); err != nil {
				return err
			}
		}

		return nil
	})
}

func durationString(d Duration) string {
	if d == 0 {
		return ""
	}
	return time.Duration(d).String()
}

func parseDurationString(s string) (Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	return Duration(d), nil
}

func mapConfig(setting *dbstore.GatewaySetting, instances []dbstore.Instance) (*Config, error) {
	queryDur, err := parseDurationString(setting.QueryTimeout)
	if err != nil {
		return nil, fmt.Errorf("invalid query_timeout %q: %w", setting.QueryTimeout, err)
	}
	pushDur, err := parseDurationString(setting.PushTimeout)
	if err != nil {
		return nil, fmt.Errorf("invalid push_timeout %q: %w", setting.PushTimeout, err)
	}

	cfg := &Config{
		Version: setting.Version,
		Gateway: GatewayConfig{
			MaxBodyBytes: setting.MaxBodyBytes,
			Timeouts: TimeoutConfig{
				Query: queryDur,
				Push:  pushDur,
			},
		},
		Instances: make([]*InstanceConfig, 0, len(instances)),
	}

	for _, inst := range instances {
		ic, err := mapInstance(&inst)
		if err != nil {
			return nil, err
		}
		cfg.Instances = append(cfg.Instances, ic)
	}

	return cfg, nil
}

func mapInstance(inst *dbstore.Instance) (*InstanceConfig, error) {
	ic := &InstanceConfig{
		Name:       inst.Name,
		Backend:    inst.Backend,
		URL:        inst.URL,
		FanOutMode: inst.FanOutMode,
		BasicAuth:  inst.BasicAuth,
		TenantID:   inst.TenantID,
	}

	if len(inst.PushTargets) > 0 {
		ic.PushURLs = make([]PushTarget, 0, len(inst.PushTargets))
		for _, pt := range inst.PushTargets {
			ic.PushURLs = append(ic.PushURLs, PushTarget{
				URL:       pt.URL,
				BasicAuth: pt.BasicAuth,
				TenantID:  pt.TenantID,
			})
		}
	}

	if inst.LabelsGroup != nil {
		ic.Labels = &LabelsConfig{}
		if inst.LabelsGroup.Filter != nil {
			filter := &FilterConfig{
				Mode:  inst.LabelsGroup.Filter.Mode,
				Names: make([]string, 0, len(inst.LabelsGroup.Filter.Names)),
			}
			for _, n := range inst.LabelsGroup.Filter.Names {
				filter.Names = append(filter.Names, n.Name)
			}
			ic.Labels.Filter = filter
		}
		if len(inst.LabelsGroup.Injects) > 0 {
			ic.Labels.Inject = make(map[string]string, len(inst.LabelsGroup.Injects))
			for _, inj := range inst.LabelsGroup.Injects {
				ic.Labels.Inject[inj.Key] = inj.Value
			}
		}
	}

	return ic, nil
}

func saveInstance(tx *gorm.DB, inst *InstanceConfig) error {
	instID, err := uuid.NewV4()
	if err != nil {
		return fmt.Errorf("generate instance id: %w", err)
	}

	dbInst := dbstore.Instance{
		ID:         instID,
		Name:       inst.Name,
		Backend:    inst.Backend,
		URL:        inst.URL,
		FanOutMode: inst.FanOutMode,
		BasicAuth:  inst.BasicAuth,
		TenantID:   inst.TenantID,
	}
	if err := tx.Create(&dbInst).Error; err != nil {
		return fmt.Errorf("create instance %q: %w", inst.Name, err)
	}

	for _, pt := range inst.PushURLs {
		if err := savePushTarget(tx, instID, pt); err != nil {
			return err
		}
	}

	if inst.Labels != nil {
		if err := saveLabelsGroup(tx, instID, inst.Labels); err != nil {
			return err
		}
	}

	return nil
}

func savePushTarget(tx *gorm.DB, instID uuid.UUID, pt PushTarget) error {
	ptID, err := uuid.NewV4()
	if err != nil {
		return fmt.Errorf("generate push target id: %w", err)
	}
	dbPt := dbstore.PushTarget{
		ID:         ptID,
		InstanceID: instID,
		URL:        pt.URL,
		BasicAuth:  pt.BasicAuth,
		TenantID:   pt.TenantID,
	}
	if err := tx.Create(&dbPt).Error; err != nil {
		return fmt.Errorf("create push target: %w", err)
	}
	return nil
}

func saveLabelsGroup(tx *gorm.DB, instID uuid.UUID, labels *LabelsConfig) error {
	lgID, err := uuid.NewV4()
	if err != nil {
		return fmt.Errorf("generate labels group id: %w", err)
	}
	lg := dbstore.LabelsGroup{
		ID:         lgID,
		InstanceID: instID,
	}
	if err := tx.Create(&lg).Error; err != nil {
		return fmt.Errorf("create labels group: %w", err)
	}

	if labels.Filter != nil {
		if err := saveFilter(tx, lgID, labels.Filter); err != nil {
			return err
		}
	}

	for k, v := range labels.Inject {
		if err := saveLabelInject(tx, lgID, k, v); err != nil {
			return err
		}
	}

	return nil
}

func saveFilter(tx *gorm.DB, lgID uuid.UUID, filter *FilterConfig) error {
	filterID, err := uuid.NewV4()
	if err != nil {
		return fmt.Errorf("generate filter id: %w", err)
	}
	dbFilter := dbstore.Filter{
		ID:            filterID,
		LabelsGroupID: lgID,
		Mode:          filter.Mode,
	}
	if err := tx.Create(&dbFilter).Error; err != nil {
		return fmt.Errorf("create filter: %w", err)
	}

	for _, name := range filter.Names {
		if err := saveFilterName(tx, filterID, name); err != nil {
			return err
		}
	}

	return nil
}

func saveFilterName(tx *gorm.DB, filterID uuid.UUID, name string) error {
	fnID, err := uuid.NewV4()
	if err != nil {
		return fmt.Errorf("generate filter name id: %w", err)
	}
	fn := dbstore.FilterName{
		ID:       fnID,
		FilterID: filterID,
		Name:     name,
	}
	if err := tx.Create(&fn).Error; err != nil {
		return fmt.Errorf("create filter name: %w", err)
	}
	return nil
}

func saveLabelInject(tx *gorm.DB, lgID uuid.UUID, key, value string) error {
	injID, err := uuid.NewV4()
	if err != nil {
		return fmt.Errorf("generate label inject id: %w", err)
	}
	inj := dbstore.LabelInject{
		ID:            injID,
		LabelsGroupID: lgID,
		Key:           key,
		Value:         value,
	}
	if err := tx.Create(&inj).Error; err != nil {
		return fmt.Errorf("create label inject: %w", err)
	}
	return nil
}
