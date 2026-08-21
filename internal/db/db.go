package db

import (
	"fmt"
	"log"
	"log/slog"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Config holds the database connection settings.
// SQLite uses the pure-Go driver so the binary remains CGO-free.
type Config struct {
	Type string `yaml:"type"`

	// SQLite fields.
	Path    string         `yaml:"path,omitempty"`
	Mode    string         `yaml:"mode,omitempty"`
	Cache   string         `yaml:"cache,omitempty"`
	Pragmas map[string]any `yaml:"pragmas,omitempty"`

	// Postgres fields.
	Host           string `yaml:"host,omitempty"`
	Port           int    `yaml:"port,omitempty"`
	Database       string `yaml:"database,omitempty"`
	User           string `yaml:"user,omitempty"`
	Password       string `yaml:"password,omitempty"`
	SSLMode        string `yaml:"sslmode,omitempty"`
	TimeZone       string `yaml:"timezone,omitempty"`
	ConnectTimeout int    `yaml:"connect_timeout,omitempty"`
}

// Open returns a GORM handle for the configured database type.
func Open(d Config) (*gorm.DB, error) {
	dsn, err := d.DSN()
	if err != nil {
		return nil, err
	}
	slog.Debug("opening database", "type", d.Type, "dsn", d.RedactedDSN())

	var dialector gorm.Dialector
	switch d.Type {
	case "sqlite":
		dialector = sqlite.Open(dsn)
	case "postgres":
		dialector = postgres.Open(dsn)
	default:
		return nil, fmt.Errorf("unsupported db type %q", d.Type)
	}

	// TranslateError maps driver-specific constraint violations onto GORM's
	// sentinel errors (gorm.ErrDuplicatedKey and friends). Without it, callers
	// are forced into matching driver error strings, which differ between
	// SQLite ("UNIQUE constraint failed") and Postgres ("duplicate key value").
	//
	// IgnoreRecordNotFoundError keeps "record not found" out of the log: the
	// gateway uses First() as an existence check during bootstrap and seeding,
	// so a miss is an expected outcome, not an error worth alarming an operator.
	db, err := gorm.Open(dialector, &gorm.Config{
		TranslateError: true,
		Logger: logger.New(log.New(os.Stderr, "", log.LstdFlags), logger.Config{
			SlowThreshold:             200 * time.Millisecond,
			LogLevel:                  logger.Warn,
			IgnoreRecordNotFoundError: true,
		}),
	})
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if d.Type == "sqlite" {
		if err := db.Exec("PRAGMA foreign_keys = ON").Error; err != nil {
			return nil, fmt.Errorf("enable sqlite foreign keys: %w", err)
		}
	}

	return db, nil
}

// DSN builds the driver connection string from the structured config fields.
func (d Config) DSN() (string, error) {
	switch d.Type {
	case "sqlite":
		return d.sqliteDSN()
	case "postgres":
		return d.postgresDSN(false)
	default:
		return "", fmt.Errorf("unsupported db type %q", d.Type)
	}
}

// RedactedDSN returns the generated connection string with secrets masked. It is
// intended for debug logging only.
func (d Config) RedactedDSN() string {
	var (
		dsn string
		err error
	)
	if d.Type == "postgres" {
		dsn, err = d.postgresDSN(true)
	} else {
		dsn, err = d.DSN()
	}
	if err != nil {
		return "<invalid>"
	}
	return dsn
}

// SQLitePath returns the expanded SQLite file path, if this config uses a file.
func (d Config) SQLitePath() string {
	if d.Type != "sqlite" {
		return ""
	}
	path := os.ExpandEnv(d.Path)
	if path == ":memory:" || strings.HasPrefix(path, "file::memory:") {
		return ""
	}
	if strings.HasPrefix(path, "file:") {
		u, err := url.Parse(path)
		if err == nil && u.Path != "" {
			return u.Path
		}
		return strings.TrimPrefix(path, "file:")
	}
	return path
}

func (d Config) sqliteDSN() (string, error) {
	path := os.ExpandEnv(d.Path)
	if path == "" {
		return "", fmt.Errorf("sqlite db.path is required")
	}
	if d.Mode != "" && !validSQLiteMode(d.Mode) {
		return "", fmt.Errorf("sqlite db.mode must be one of ro, rw, rwc or memory")
	}
	if d.Cache != "" && d.Cache != "shared" && d.Cache != "private" {
		return "", fmt.Errorf("sqlite db.cache must be shared or private")
	}

	q := url.Values{}
	if d.Mode != "" {
		q.Set("mode", d.Mode)
	}
	if d.Cache != "" {
		q.Set("cache", d.Cache)
	}
	for _, key := range sortedKeys(d.Pragmas) {
		val := os.ExpandEnv(fmt.Sprint(d.Pragmas[key]))
		q.Add("_pragma", key+"("+val+")")
	}
	if len(q) == 0 {
		return path, nil
	}
	return sqliteURI(path) + "?" + q.Encode(), nil
}

func validSQLiteMode(mode string) bool {
	switch mode {
	case "ro", "rw", "rwc", "memory":
		return true
	default:
		return false
	}
}

func sqliteURI(path string) string {
	if path == ":memory:" || strings.HasPrefix(path, "file:") {
		return path
	}
	return "file:" + path
}

func (d Config) postgresDSN(redact bool) (string, error) {
	host := os.ExpandEnv(d.Host)
	database := os.ExpandEnv(d.Database)
	user := os.ExpandEnv(d.User)
	password := os.ExpandEnv(d.Password)
	if host == "" {
		return "", fmt.Errorf("postgres db.host is required")
	}
	if database == "" {
		return "", fmt.Errorf("postgres db.database is required")
	}
	if user == "" {
		return "", fmt.Errorf("postgres db.user is required")
	}
	if d.Port < 0 || d.Port > 65535 {
		return "", fmt.Errorf("postgres db.port must be between 1 and 65535 when set")
	}
	if d.SSLMode != "" && !validPostgresSSLMode(d.SSLMode) {
		return "", fmt.Errorf("postgres db.sslmode must be disable, allow, prefer, require, verify-ca or verify-full")
	}
	if d.ConnectTimeout < 0 {
		return "", fmt.Errorf("postgres db.connect_timeout must be non-negative")
	}

	port := d.Port
	if port == 0 {
		port = 5432
	}

	parts := []string{
		postgresKeyValue("host", host),
		postgresKeyValue("user", user),
		postgresKeyValue("dbname", database),
		postgresKeyValue("port", strconv.Itoa(port)),
	}
	if password != "" {
		if redact {
			password = "XXXXXXXX"
		}
		parts = append(parts, postgresKeyValue("password", password))
	}
	if d.SSLMode != "" {
		parts = append(parts, postgresKeyValue("sslmode", d.SSLMode))
	}
	if d.TimeZone != "" {
		parts = append(parts, postgresKeyValue("TimeZone", os.ExpandEnv(d.TimeZone)))
	}
	if d.ConnectTimeout > 0 {
		parts = append(parts, postgresKeyValue("connect_timeout", strconv.Itoa(d.ConnectTimeout)))
	}
	return strings.Join(parts, " "), nil
}

func postgresKeyValue(key, value string) string {
	return key + "=" + quotePostgresValue(value)
}

func quotePostgresValue(value string) string {
	if value == "" || strings.ContainsAny(value, " \t\r\n'\\") {
		escaped := strings.ReplaceAll(value, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `'`, `\'`)
		return "'" + escaped + "'"
	}
	return value
}

func validPostgresSSLMode(mode string) bool {
	switch mode {
	case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
		return true
	default:
		return false
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Migrate creates or updates the tables used by the gateway.
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(
		&GatewaySetting{},
		&Instance{},
		&PushTarget{},
		&LabelsGroup{},
		&Filter{},
		&FilterName{},
		&LabelInject{},
		&Tenant{},
		&GrantReadPolicy{},
		&User{},
		&APIKey{},
		&Role{},
	)
}
