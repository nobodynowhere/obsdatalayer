package db

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DSN holds the database connection settings.
// SQLite uses the pure-Go driver so the binary remains CGO-free.
type DSN struct {
	Type string `yaml:"type"`
	DSN  string `yaml:"dsn"`
}

// Open returns a GORM handle for the configured database type.
func Open(d DSN) (*gorm.DB, error) {
	var dialector gorm.Dialector
	switch d.Type {
	case "sqlite":
		dialector = sqlite.Open(d.DSN)
	case "postgres":
		dialector = postgres.Open(d.DSN)
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
		&User{},
		&Role{},
	)
}
