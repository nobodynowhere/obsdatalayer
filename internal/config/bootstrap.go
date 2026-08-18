package config

import (
	"crypto/tls"
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
	TLS         TLSConfig  `yaml:"tls"`
}

// TLSConfig holds the process-level TLS settings for both listeners.
type TLSConfig struct {
	Enabled      bool     `yaml:"enabled"`
	CertFile     string   `yaml:"cert_file"`
	KeyFile      string   `yaml:"key_file"`
	MinVersion   string   `yaml:"min_version"`
	CipherSuites []string `yaml:"cipher_suites"`
}

// Bootstrap is the startup file. It carries only what cannot be read from the
// database: how to reach the database, where to listen, and how listener TLS is
// configured. Everything else -- instances, tenants, timeouts, body limits, log
// level, reload interval, users and roles -- lives in the database and is
// managed through the admin API.
type Bootstrap struct {
	DB      db.Config        `yaml:"db"`
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
// database password, and a raw goccy error quotes the offending source line
// verbatim.
func LoadBootstrap(path string) (*Bootstrap, error) {
	return loadBootstrap(path, false)
}

// LoadBootstrapForTLSGeneration loads the bootstrap file for the certificate
// helper. It permits missing certificate paths because the helper can choose
// them and optionally write them back to the file.
func LoadBootstrapForTLSGeneration(path string) (*Bootstrap, error) {
	return loadBootstrap(path, true)
}

func loadBootstrap(path string, allowMissingTLSFiles bool) (*Bootstrap, error) {
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
	b.Gateway.TLS.applyDefaults()
	if b.DB.Type == "" {
		return nil, fmt.Errorf("bootstrap %s: db.type is required (sqlite or postgres)", path)
	}
	if _, err := b.DB.DSN(); err != nil {
		return nil, fmt.Errorf("bootstrap %s: %w", path, err)
	}
	if err := b.Gateway.TLS.validate(!allowMissingTLSFiles); err != nil {
		return nil, fmt.Errorf("bootstrap %s: %w", path, err)
	}

	return &b, nil
}

func (t *TLSConfig) applyDefaults() {
	if t.MinVersion == "" {
		t.MinVersion = "TLS1.2"
	}
	if len(t.CipherSuites) == 0 {
		t.CipherSuites = DefaultTLSCipherSuiteNames()
	}
}

// Validate checks that TLS settings are complete and supported.
func (t TLSConfig) Validate() error {
	return t.validate(true)
}

func (t TLSConfig) validate(requireFiles bool) error {
	if !t.Enabled && !requireFiles {
		return nil
	}
	if !t.Enabled {
		return nil
	}
	if requireFiles && t.CertFile == "" {
		return fmt.Errorf("gateway.tls.cert_file is required when TLS is enabled")
	}
	if requireFiles && t.KeyFile == "" {
		return fmt.Errorf("gateway.tls.key_file is required when TLS is enabled")
	}
	if _, err := TLSVersion(t.MinVersion); err != nil {
		return err
	}
	for _, name := range t.CipherSuites {
		if _, err := TLSCipherSuite(name); err != nil {
			return err
		}
	}
	return nil
}

// ServerTLSConfig converts bootstrap TLS settings into a net/http TLS config.
func (t TLSConfig) ServerTLSConfig() (*tls.Config, error) {
	if !t.Enabled {
		return nil, nil
	}
	min, err := TLSVersion(t.MinVersion)
	if err != nil {
		return nil, err
	}
	ciphers := make([]uint16, 0, len(t.CipherSuites))
	for _, name := range t.CipherSuites {
		id, err := TLSCipherSuite(name)
		if err != nil {
			return nil, err
		}
		ciphers = append(ciphers, id)
	}
	return &tls.Config{
		MinVersion:               min,
		CipherSuites:             ciphers,
		PreferServerCipherSuites: true,
	}, nil
}

func TLSVersion(name string) (uint16, error) {
	switch name {
	case "TLS1.2", "1.2", "tls1.2":
		return tls.VersionTLS12, nil
	case "TLS1.3", "1.3", "tls1.3":
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("gateway.tls.min_version must be TLS1.2 or TLS1.3")
	}
}

func TLSCipherSuite(name string) (uint16, error) {
	ciphers := map[string]uint16{
		"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256":       tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256":         tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384":       tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384":         tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256": tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
		"TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256":   tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
	}
	if id, ok := ciphers[name]; ok {
		return id, nil
	}
	return 0, fmt.Errorf("unsupported gateway.tls.cipher_suites entry %q", name)
}

func DefaultTLSCipherSuiteNames() []string {
	return []string{
		"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
		"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
		"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384",
		"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
		"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256",
		"TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256",
	}
}
