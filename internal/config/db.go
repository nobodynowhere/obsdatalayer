package config

import (
	"errors"
	"fmt"
	"time"

	"github.com/gofrs/uuid/v5"
	"gorm.io/gorm"

	dbstore "obsdatalayer/internal/db"
)

// Errors returned by the instance operations.
var (
	ErrNotFound = errors.New("instance not found")
	ErrExists   = errors.New("instance already exists")
)

// LoadFromDB loads the active Config from the database.
func LoadFromDB(db *gorm.DB) (*Config, error) {
	setting, err := loadSetting(db)
	if err != nil {
		return nil, err
	}

	var instances []dbstore.Instance
	if err := db.
		Preload("PushTargets").
		Preload("LabelsGroup.Filter.Names").
		Preload("LabelsGroup.Injects").
		Find(&instances).Error; err != nil {
		return nil, fmt.Errorf("load instances: %w", err)
	}

	cfg, err := mapConfig(setting, instances)
	if err != nil {
		return nil, err
	}
	return New(cfg)
}

// EnsureSettings creates the single settings row with defaults if the database
// has none. A fresh install starts fully configured-by-default and empty of
// instances; everything is then edited through the admin API.
func EnsureSettings(db *gorm.DB) error {
	var count int64
	if err := db.Model(&dbstore.GatewaySetting{}).Count(&count).Error; err != nil {
		return fmt.Errorf("count gateway settings: %w", err)
	}
	if count > 0 {
		return nil
	}

	defaults := &Config{}
	applyDefaults(defaults)

	id, err := uuid.NewV4()
	if err != nil {
		return fmt.Errorf("generate setting id: %w", err)
	}
	row := dbstore.GatewaySetting{
		ID:             id,
		MaxBodyBytes:   defaults.Gateway.MaxBodyBytes,
		QueryTimeout:   durationString(defaults.Gateway.Timeouts.Query),
		PushTimeout:    durationString(defaults.Gateway.Timeouts.Push),
		LogLevel:       defaults.Gateway.LogLevel,
		ReloadInterval: durationString(defaults.Gateway.ReloadInterval),
	}
	if err := db.Create(&row).Error; err != nil {
		return fmt.Errorf("create gateway settings: %w", err)
	}
	return nil
}

// SaveSettings replaces the gateway settings row.
func SaveSettings(db *gorm.DB, g GatewayConfig) error {
	cfg := &Config{Gateway: g}
	applyDefaults(cfg)
	if err := validateGateway(&cfg.Gateway); err != nil {
		return err
	}

	setting, err := loadSetting(db)
	if err != nil {
		return err
	}
	updates := map[string]any{
		"max_body_bytes":  cfg.Gateway.MaxBodyBytes,
		"query_timeout":   durationString(cfg.Gateway.Timeouts.Query),
		"push_timeout":    durationString(cfg.Gateway.Timeouts.Push),
		"log_level":       cfg.Gateway.LogLevel,
		"reload_interval": durationString(cfg.Gateway.ReloadInterval),
	}
	if err := db.Model(&dbstore.GatewaySetting{}).Where("id = ?", setting.ID).Updates(updates).Error; err != nil {
		return fmt.Errorf("update gateway settings: %w", err)
	}
	return nil
}

// loadSetting returns the single settings row.
func loadSetting(db *gorm.DB) (*dbstore.GatewaySetting, error) {
	var setting dbstore.GatewaySetting
	// Ordered so that a database which somehow holds more than one row still
	// resolves deterministically instead of picking an arbitrary UUID.
	if err := db.Order("id").First(&setting).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("gateway settings row is missing; run EnsureSettings")
		}
		return nil, fmt.Errorf("load gateway settings: %w", err)
	}
	return &setting, nil
}

// ---- instance management ----------------------------------------------------

// CreateInstance validates and inserts a new instance.
func CreateInstance(db *gorm.DB, inst *InstanceConfig, reg TenantRegistry) error {
	if err := prepareInstance(inst, reg); err != nil {
		return err
	}
	var count int64
	if err := db.Model(&dbstore.Instance{}).Where("name = ?", inst.Name).Count(&count).Error; err != nil {
		return fmt.Errorf("check instance %q: %w", inst.Name, err)
	}
	if count > 0 {
		return ErrExists
	}
	return db.Transaction(func(tx *gorm.DB) error {
		return saveInstance(tx, inst)
	})
}

// UpdateInstance replaces an existing instance in place. Child rows (push
// targets, labels) are rebuilt rather than diffed: they are small, and a full
// replace keeps the stored shape exactly matching the submitted document.
func UpdateInstance(db *gorm.DB, name string, inst *InstanceConfig, reg TenantRegistry) error {
	if err := prepareInstance(inst, reg); err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		var existing dbstore.Instance
		if err := tx.Where("name = ?", name).First(&existing).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("load instance %q: %w", name, err)
		}
		if inst.Name != name {
			var clash int64
			if err := tx.Model(&dbstore.Instance{}).Where("name = ?", inst.Name).Count(&clash).Error; err != nil {
				return fmt.Errorf("check instance %q: %w", inst.Name, err)
			}
			if clash > 0 {
				return ErrExists
			}
		}
		if err := deleteInstanceRows(tx, existing.ID); err != nil {
			return err
		}
		if err := tx.Where("id = ?", existing.ID).Delete(&dbstore.Instance{}).Error; err != nil {
			return fmt.Errorf("replace instance %q: %w", name, err)
		}
		return saveInstance(tx, inst)
	})
}

// DeleteInstance removes an instance and its child rows.
func DeleteInstance(db *gorm.DB, name string) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var existing dbstore.Instance
		if err := tx.Where("name = ?", name).First(&existing).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("load instance %q: %w", name, err)
		}
		if err := deleteInstanceRows(tx, existing.ID); err != nil {
			return err
		}
		if err := tx.Where("id = ?", existing.ID).Delete(&dbstore.Instance{}).Error; err != nil {
			return fmt.Errorf("delete instance %q: %w", name, err)
		}
		return nil
	})
}

// prepareInstance applies defaults and runs the same validation the runtime
// config uses, so a rejected instance never reaches the database.
func prepareInstance(inst *InstanceConfig, reg TenantRegistry) error {
	cfg := &Config{Instances: []*InstanceConfig{inst}}
	applyDefaults(cfg)
	if err := validate(cfg); err != nil {
		return err
	}
	return cfg.ValidateTenants(reg)
}

// deleteInstanceRows removes the child rows belonging to an instance. SQLite
// foreign keys are enabled, but the labels chain is deleted explicitly so the
// behaviour is identical on Postgres.
func deleteInstanceRows(tx *gorm.DB, instanceID uuid.UUID) error {
	if err := tx.Where("instance_id = ?", instanceID).Delete(&dbstore.PushTarget{}).Error; err != nil {
		return fmt.Errorf("delete push targets: %w", err)
	}

	var groups []dbstore.LabelsGroup
	if err := tx.Where("instance_id = ?", instanceID).Find(&groups).Error; err != nil {
		return fmt.Errorf("load labels groups: %w", err)
	}
	for _, g := range groups {
		var filters []dbstore.Filter
		if err := tx.Where("labels_group_id = ?", g.ID).Find(&filters).Error; err != nil {
			return fmt.Errorf("load filters: %w", err)
		}
		for _, f := range filters {
			if err := tx.Where("filter_id = ?", f.ID).Delete(&dbstore.FilterName{}).Error; err != nil {
				return fmt.Errorf("delete filter names: %w", err)
			}
		}
		if err := tx.Where("labels_group_id = ?", g.ID).Delete(&dbstore.Filter{}).Error; err != nil {
			return fmt.Errorf("delete filters: %w", err)
		}
		if err := tx.Where("labels_group_id = ?", g.ID).Delete(&dbstore.LabelInject{}).Error; err != nil {
			return fmt.Errorf("delete label injects: %w", err)
		}
	}
	if err := tx.Where("instance_id = ?", instanceID).Delete(&dbstore.LabelsGroup{}).Error; err != nil {
		return fmt.Errorf("delete labels groups: %w", err)
	}
	return nil
}

// ---- mapping ----------------------------------------------------------------

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
	reloadDur, err := parseDurationString(setting.ReloadInterval)
	if err != nil {
		return nil, fmt.Errorf("invalid reload_interval %q: %w", setting.ReloadInterval, err)
	}

	cfg := &Config{
		Gateway: GatewayConfig{
			MaxBodyBytes:   setting.MaxBodyBytes,
			LogLevel:       setting.LogLevel,
			ReloadInterval: reloadDur,
			Timeouts: TimeoutConfig{
				Query: queryDur,
				Push:  pushDur,
			},
		},
		Instances: make([]*InstanceConfig, 0, len(instances)),
	}

	for _, inst := range instances {
		cfg.Instances = append(cfg.Instances, mapInstance(&inst))
	}
	return cfg, nil
}

func mapInstance(inst *dbstore.Instance) *InstanceConfig {
	ic := &InstanceConfig{
		Name:          inst.Name,
		Backend:       inst.Backend,
		URL:           inst.URL,
		FanOutMode:    inst.FanOutMode,
		BasicAuth:     inst.BasicAuth,
		TenantID:      inst.TenantID,
		SkipTLSVerify: inst.SkipTLSVerify,
	}

	if len(inst.PushTargets) > 0 {
		ic.PushURLs = make([]PushTarget, 0, len(inst.PushTargets))
		for _, pt := range inst.PushTargets {
			ic.PushURLs = append(ic.PushURLs, PushTarget{
				URL:           pt.URL,
				BasicAuth:     pt.BasicAuth,
				TenantID:      pt.TenantID,
				SkipTLSVerify: pt.SkipTLSVerify,
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

	return ic
}

// ---- writers ----------------------------------------------------------------

func saveInstance(tx *gorm.DB, inst *InstanceConfig) error {
	instID, err := uuid.NewV4()
	if err != nil {
		return fmt.Errorf("generate instance id: %w", err)
	}

	dbInst := dbstore.Instance{
		ID:            instID,
		Name:          inst.Name,
		Backend:       inst.Backend,
		URL:           inst.URL,
		FanOutMode:    inst.FanOutMode,
		BasicAuth:     inst.BasicAuth,
		TenantID:      inst.TenantID,
		SkipTLSVerify: inst.SkipTLSVerify,
	}
	if err := tx.Create(&dbInst).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return ErrExists
		}
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
		ID:            ptID,
		InstanceID:    instID,
		URL:           pt.URL,
		BasicAuth:     pt.BasicAuth,
		TenantID:      pt.TenantID,
		SkipTLSVerify: pt.SkipTLSVerify,
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
	lg := dbstore.LabelsGroup{ID: lgID, InstanceID: instID}
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
	dbFilter := dbstore.Filter{ID: filterID, LabelsGroupID: lgID, Mode: filter.Mode}
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
	fn := dbstore.FilterName{ID: fnID, FilterID: filterID, Name: name}
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
	inj := dbstore.LabelInject{ID: injID, LabelsGroupID: lgID, Key: key, Value: value}
	if err := tx.Create(&inj).Error; err != nil {
		return fmt.Errorf("create label inject: %w", err)
	}
	return nil
}
