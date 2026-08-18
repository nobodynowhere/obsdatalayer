package db_test

import (
	"strings"
	"testing"

	"obsdatalayer/internal/db"
)

func TestSQLiteDSNFromStructuredFields(t *testing.T) {
	cfg := db.Config{
		Type:  "sqlite",
		Path:  "/var/lib/obsgateway/gateway.db",
		Mode:  "rwc",
		Cache: "shared",
		Pragmas: map[string]any{
			"journal_mode": "WAL",
			"busy_timeout": 5000,
		},
	}

	dsn, err := cfg.DSN()
	if err != nil {
		t.Fatalf("DSN: %v", err)
	}
	if dsn != "file:/var/lib/obsgateway/gateway.db?_pragma=busy_timeout%285000%29&_pragma=journal_mode%28WAL%29&cache=shared&mode=rwc" {
		t.Errorf("unexpected sqlite DSN: %q", dsn)
	}
	if got := cfg.SQLitePath(); got != "/var/lib/obsgateway/gateway.db" {
		t.Errorf("expected sqlite path, got %q", got)
	}
}

func TestSQLitePlainPathStaysPlain(t *testing.T) {
	cfg := db.Config{Type: "sqlite", Path: "./gateway.db"}
	dsn, err := cfg.DSN()
	if err != nil {
		t.Fatalf("DSN: %v", err)
	}
	if dsn != "./gateway.db" {
		t.Errorf("expected plain sqlite path, got %q", dsn)
	}
}

func TestSQLiteRequiresPath(t *testing.T) {
	_, err := (db.Config{Type: "sqlite"}).DSN()
	if err == nil || !strings.Contains(err.Error(), "db.path") {
		t.Fatalf("expected missing path error, got %v", err)
	}
}

func TestPostgresDSNFromStructuredFields(t *testing.T) {
	t.Setenv("PG_PASS", "secret value")

	cfg := db.Config{
		Type:           "postgres",
		Host:           "db.example.com",
		Database:       "obsgateway",
		User:           "obs",
		Password:       "${PG_PASS}",
		SSLMode:        "require",
		TimeZone:       "UTC",
		ConnectTimeout: 10,
	}

	dsn, err := cfg.DSN()
	if err != nil {
		t.Fatalf("DSN: %v", err)
	}
	want := "host=db.example.com user=obs dbname=obsgateway port=5432 password='secret value' sslmode=require TimeZone=UTC connect_timeout=10"
	if dsn != want {
		t.Errorf("unexpected postgres DSN:\nwant %q\n got %q", want, dsn)
	}

	redacted := cfg.RedactedDSN()
	if strings.Contains(redacted, "secret") {
		t.Fatalf("redacted DSN leaked password: %q", redacted)
	}
	if !strings.Contains(redacted, "XXXXXXXX") {
		t.Fatalf("redacted DSN did not include mask: %q", redacted)
	}
}

func TestPostgresValidatesRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		cfg  db.Config
		want string
	}{
		{name: "host", cfg: db.Config{Type: "postgres", Database: "obs", User: "obs"}, want: "db.host"},
		{name: "database", cfg: db.Config{Type: "postgres", Host: "localhost", User: "obs"}, want: "db.database"},
		{name: "user", cfg: db.Config{Type: "postgres", Host: "localhost", Database: "obs"}, want: "db.user"},
		{
			name: "sslmode",
			cfg:  db.Config{Type: "postgres", Host: "localhost", Database: "obs", User: "obs", SSLMode: "sometimes"},
			want: "db.sslmode",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.cfg.DSN()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q error, got %v", tc.want, err)
			}
		})
	}
}
