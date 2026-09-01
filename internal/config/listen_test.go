package config_test

import (
	"crypto/tls"
	"testing"

	"github.com/goccy/go-yaml"

	"obsdatalayer/internal/config"
)

func TestListenAddrParsing(t *testing.T) {
	cases := []struct {
		name        string
		yaml        string
		wantHost    string
		wantPort    int
		wantErr     bool
		adminAddr   string // Addr resolved with the admin default (loopback)
		dataAddr    string // Addr resolved with the data default (all interfaces)
		adminIsLoop bool
	}{
		{
			name: "bare port defaults per caller", yaml: "9091",
			wantHost: "", wantPort: 9091,
			adminAddr: "127.0.0.1:9091", dataAddr: ":9091", adminIsLoop: true,
		},
		{
			name: "wildcard binds all interfaces", yaml: `"*:9091"`,
			wantHost: "*", wantPort: 9091,
			adminAddr: ":9091", dataAddr: ":9091", adminIsLoop: false,
		},
		{
			name: "explicit loopback", yaml: `"127.0.0.1:9091"`,
			wantHost: "127.0.0.1", wantPort: 9091,
			adminAddr: "127.0.0.1:9091", dataAddr: "127.0.0.1:9091", adminIsLoop: true,
		},
		{
			name: "explicit interface", yaml: `"10.0.0.5:9091"`,
			wantHost: "10.0.0.5", wantPort: 9091,
			adminAddr: "10.0.0.5:9091", dataAddr: "10.0.0.5:9091", adminIsLoop: false,
		},
		{
			name: "ipv6 loopback", yaml: `"[::1]:9091"`,
			wantHost: "::1", wantPort: 9091,
			adminAddr: "[::1]:9091", dataAddr: "[::1]:9091", adminIsLoop: true,
		},
		{name: "hostname is rejected", yaml: `"localhost:9091"`, wantErr: true},
		{name: "port out of range", yaml: "70000", wantErr: true},
		{name: "zero port", yaml: "0", wantErr: true},
		{name: "garbage", yaml: `"not-an-address"`, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var a config.ListenAddr
			err := yaml.Unmarshal([]byte(tc.yaml), &a)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %s", tc.yaml)
				}
				return
			}
			if err != nil {
				t.Fatalf("unmarshal %s: %v", tc.yaml, err)
			}
			if a.Host != tc.wantHost || a.Port != tc.wantPort {
				t.Errorf("expected host=%q port=%d, got host=%q port=%d",
					tc.wantHost, tc.wantPort, a.Host, a.Port)
			}
			if got := a.Addr(config.DefaultAdminHost); got != tc.adminAddr {
				t.Errorf("admin Addr: expected %q, got %q", tc.adminAddr, got)
			}
			if got := a.Addr(config.DefaultDataHost); got != tc.dataAddr {
				t.Errorf("data Addr: expected %q, got %q", tc.dataAddr, got)
			}
			if got := a.IsLoopback(config.DefaultAdminHost); got != tc.adminIsLoop {
				t.Errorf("IsLoopback: expected %v, got %v", tc.adminIsLoop, got)
			}
		})
	}
}

// The admin plane must stay on loopback unless an operator opts out, since it
// carries config read, reload, and user management.
func TestBootstrapAdminDefaultsToLoopback(t *testing.T) {
	path := writeConfig(t, `
db:
  type: sqlite
  path: /tmp/test.db
gateway:
  listen: 8080
  admin_listen: 9091
`)
	b, err := config.LoadBootstrap(path)
	if err != nil {
		t.Fatalf("load bootstrap: %v", err)
	}
	if got := b.AdminAddr(); got != "127.0.0.1:9091" {
		t.Errorf("expected admin listener on loopback, got %q", got)
	}
	if !b.AdminIsLoopback() {
		t.Error("expected AdminIsLoopback to be true by default")
	}
	if got := b.DataAddr(); got != ":8080" {
		t.Errorf("expected data listener on all interfaces, got %q", got)
	}
}

func TestBootstrapAdminExplicitWildcard(t *testing.T) {
	path := writeConfig(t, `
db:
  type: sqlite
  path: /tmp/test.db
gateway:
  admin_listen: "*:9099"
`)
	b, err := config.LoadBootstrap(path)
	if err != nil {
		t.Fatalf("load bootstrap: %v", err)
	}
	if got := b.AdminAddr(); got != ":9099" {
		t.Errorf("expected wildcard bind, got %q", got)
	}
	if b.AdminIsLoopback() {
		t.Error("wildcard bind must not report as loopback")
	}
}

func TestBootstrapPortDefaults(t *testing.T) {
	path := writeConfig(t, `
db:
  type: sqlite
  path: /tmp/test.db
`)
	b, err := config.LoadBootstrap(path)
	if err != nil {
		t.Fatalf("load bootstrap: %v", err)
	}
	if got := b.DataAddr(); got != ":8080" {
		t.Errorf("expected default data port 8080, got %q", got)
	}
	if got := b.AdminAddr(); got != "127.0.0.1:9091" {
		t.Errorf("expected default admin 127.0.0.1:9091, got %q", got)
	}
	if addr, ok := b.OTLPGRPCAddr(); ok || addr != "" {
		t.Errorf("expected OTLP/gRPC listener to default disabled, got addr=%q enabled=%v", addr, ok)
	}
	if addr, ok := b.OTLPHTTPAddr(); ok || addr != "" {
		t.Errorf("expected OTLP/HTTP listener to default disabled, got addr=%q enabled=%v", addr, ok)
	}
}

func TestBootstrapOTLPHTTPListenIsOptIn(t *testing.T) {
	path := writeConfig(t, `
db:
  type: sqlite
  path: /tmp/test.db
gateway:
  otlp_http_listen: 4318
`)
	b, err := config.LoadBootstrap(path)
	if err != nil {
		t.Fatalf("load bootstrap: %v", err)
	}
	addr, ok := b.OTLPHTTPAddr()
	if !ok {
		t.Fatal("expected OTLP/HTTP listener to be enabled")
	}
	if addr != ":4318" {
		t.Errorf("expected OTLP/HTTP listener on all interfaces, got %q", addr)
	}
}

func TestBootstrapOTLPGRPCListenIsOptIn(t *testing.T) {
	path := writeConfig(t, `
db:
  type: sqlite
  path: /tmp/test.db
gateway:
  otlp_grpc_listen: 4317
`)
	b, err := config.LoadBootstrap(path)
	if err != nil {
		t.Fatalf("load bootstrap: %v", err)
	}
	addr, ok := b.OTLPGRPCAddr()
	if !ok {
		t.Fatal("expected OTLP/gRPC listener to be enabled")
	}
	if addr != ":4317" {
		t.Errorf("expected OTLP/gRPC listener on all interfaces, got %q", addr)
	}
}

func TestBootstrapAcceptsStructuredPostgres(t *testing.T) {
	path := writeConfig(t, `
db:
  type: postgres
  host: db.example.com
  port: 5432
  database: obsgateway
  user: obsgateway
  password: ${OBSGATEWAY_DB_PASSWORD}
  sslmode: require
  timezone: UTC
`)
	t.Setenv("OBSGATEWAY_DB_PASSWORD", "secret")

	b, err := config.LoadBootstrap(path)
	if err != nil {
		t.Fatalf("load bootstrap: %v", err)
	}
	dsn, err := b.DB.DSN()
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	if dsn != "host=db.example.com user=obsgateway dbname=obsgateway port=5432 password=secret sslmode=require TimeZone=UTC" {
		t.Errorf("unexpected postgres dsn: %q", dsn)
	}
}

func TestBootstrapRejectsSQLiteWithoutPath(t *testing.T) {
	path := writeConfig(t, `
db:
  type: sqlite
`)
	_, err := config.LoadBootstrap(path)
	if err == nil {
		t.Fatal("expected missing sqlite path to fail")
	}
}

func TestBootstrapAcceptsSQLitePragmas(t *testing.T) {
	path := writeConfig(t, `
db:
  type: sqlite
  path: /tmp/test.db
  mode: rwc
  cache: shared
  pragmas:
    busy_timeout: 5000
    journal_mode: WAL
`)
	b, err := config.LoadBootstrap(path)
	if err != nil {
		t.Fatalf("load bootstrap: %v", err)
	}
	dsn, err := b.DB.DSN()
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	if dsn != "file:/tmp/test.db?_pragma=busy_timeout%285000%29&_pragma=journal_mode%28WAL%29&cache=shared&mode=rwc" {
		t.Errorf("unexpected sqlite dsn: %q", dsn)
	}
}

func TestBootstrapTLSDefaults(t *testing.T) {
	path := writeConfig(t, `
db:
  type: sqlite
  path: /tmp/test.db
gateway:
  tls:
    enabled: true
    cert_file: /tmp/obsgateway.crt
    key_file: /tmp/obsgateway.key
`)
	b, err := config.LoadBootstrap(path)
	if err != nil {
		t.Fatalf("load bootstrap: %v", err)
	}
	if b.Gateway.TLS.MinVersion != "TLS1.2" {
		t.Errorf("expected TLS1.2 default, got %q", b.Gateway.TLS.MinVersion)
	}
	if len(b.Gateway.TLS.CipherSuites) == 0 {
		t.Fatal("expected default TLS cipher suites")
	}
	tlsConfig, err := b.Gateway.TLS.ServerTLSConfig()
	if err != nil {
		t.Fatalf("server tls config: %v", err)
	}
	if tlsConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("expected TLS 1.2 minimum, got %x", tlsConfig.MinVersion)
	}
}

func TestBootstrapTLSRejectsMissingCertificate(t *testing.T) {
	path := writeConfig(t, `
db:
  type: sqlite
  path: /tmp/test.db
gateway:
  tls:
    enabled: true
`)
	_, err := config.LoadBootstrap(path)
	if err == nil {
		t.Fatal("expected missing certificate paths to fail")
	}
}

func TestBootstrapForTLSGenerationAllowsMissingCertificate(t *testing.T) {
	path := writeConfig(t, `
db:
  type: sqlite
  path: /tmp/test.db
gateway:
  tls:
    enabled: true
`)
	b, err := config.LoadBootstrapForTLSGeneration(path)
	if err != nil {
		t.Fatalf("load bootstrap for TLS generation: %v", err)
	}
	if !b.Gateway.TLS.Enabled {
		t.Fatal("expected TLS enabled flag to survive helper load")
	}
}

func TestBootstrapTLSRejectsWeakMinimumVersion(t *testing.T) {
	path := writeConfig(t, `
db:
  type: sqlite
  path: /tmp/test.db
gateway:
  tls:
    enabled: true
    cert_file: /tmp/obsgateway.crt
    key_file: /tmp/obsgateway.key
    min_version: TLS1.1
`)
	_, err := config.LoadBootstrap(path)
	if err == nil {
		t.Fatal("expected weak TLS version to fail")
	}
}

func TestBootstrapTLSRejectsUnknownCipher(t *testing.T) {
	path := writeConfig(t, `
db:
  type: sqlite
  path: /tmp/test.db
gateway:
  tls:
    enabled: true
    cert_file: /tmp/obsgateway.crt
    key_file: /tmp/obsgateway.key
    cipher_suites:
      - TLS_RSA_WITH_3DES_EDE_CBC_SHA
`)
	_, err := config.LoadBootstrap(path)
	if err == nil {
		t.Fatal("expected unknown cipher suite to fail")
	}
}
