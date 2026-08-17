package config

import (
	"fmt"
	"os"

	"github.com/goccy/go-yaml"

	"obsdatalayer/internal/db"
)

// Default hosts applied when a listener specifies only a port.
// The data plane serves clients so it binds every interface; the admin plane
// carries config read/write and user management so it stays on loopback.
const (
	DefaultDataHost  = "*"
	DefaultAdminHost = "127.0.0.1"
)

// GatewayBootstrap holds the process listener addresses. They are named for
// what they are -- full "host:port" listen addresses, not bare ports.
type GatewayBootstrap struct {
	Listen      ListenAddr `yaml:"listen"`
	AdminListen ListenAddr `yaml:"admin_listen"`
}

// Bootstrap is the startup file. It carries only what cannot be read from the
// database: how to reach the database, and where to listen. Everything else --
// instances, tenants, timeouts, body limits, log level, reload interval, users
// and roles -- lives in the database and is managed through the admin API.
type Bootstrap struct {
	DB      db.DSN           `yaml:"db"`
	Gateway GatewayBootstrap `yaml:"gateway"`
}

// DataAddr returns the data listener address, defaulting to all interfaces.
func (b *Bootstrap) DataAddr() string {
	return b.Gateway.Listen.Addr(DefaultDataHost)
}

// AdminAddr returns the admin listener address, defaulting to loopback.
func (b *Bootstrap) AdminAddr() string {
	return b.Gateway.AdminListen.Addr(DefaultAdminHost)
}

// AdminIsLoopback reports whether the admin listener is confined to loopback.
func (b *Bootstrap) AdminIsLoopback() bool {
	return b.Gateway.AdminListen.IsLoopback(DefaultAdminHost)
}

// LoadBootstrap reads the bootstrap YAML file at path and returns the parsed
// Bootstrap.
//
// Parse failures are routed through redactYAMLError: this file contains the
// database DSN, which for Postgres embeds a password, and a raw goccy error
// quotes the offending source line verbatim.
func LoadBootstrap(path string) (*Bootstrap, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read bootstrap %s: %w", path, err)
	}

	var b Bootstrap
	if err := yaml.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("parse bootstrap %s: %w", path, redactYAMLError(err, data))
	}

	if b.Gateway.Listen.Port == 0 {
		b.Gateway.Listen.Port = 8080
	}
	if b.Gateway.AdminListen.Port == 0 {
		b.Gateway.AdminListen.Port = 9091
	}
	if b.DB.Type == "" {
		return nil, fmt.Errorf("bootstrap %s: db.type is required (sqlite or postgres)", path)
	}
	if b.DB.DSN == "" {
		return nil, fmt.Errorf("bootstrap %s: db.dsn is required", path)
	}

	return &b, nil
}
