package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ListenAddr is a listener specification parsed from YAML. It accepts a bare
// port ("9091"), a wildcard host ("*:9091"), or an explicit interface
// ("127.0.0.1:9091", "[::1]:9091").
//
// Host has three meanings:
//   - ""  the host was not specified; the caller's default applies
//   - "*" bind every interface
//   - any other value binds that specific address
type ListenAddr struct {
	Host string
	Port int
}

// UnmarshalYAML parses a bare port or a host:port pair.
func (a *ListenAddr) UnmarshalYAML(b []byte) error {
	s := strings.TrimSpace(string(b))
	s = strings.Trim(s, `"'`)
	if s == "" {
		return nil
	}
	return a.parse(s)
}

// MarshalYAML renders the address back as a quoted host:port string.
func (a ListenAddr) MarshalYAML() ([]byte, error) {
	return []byte(strconv.Quote(a.spec())), nil
}

// IsZero lets YAML omitempty skip optional listeners. Without it, an unset
// listener marshals as "0", which cannot be parsed on the next load.
func (a ListenAddr) IsZero() bool {
	return a.Host == "" && a.Port == 0
}

func (a *ListenAddr) parse(s string) error {
	// A bare port leaves Host empty so the caller's default wins.
	if port, err := strconv.Atoi(s); err == nil {
		if port < 1 || port > 65535 {
			return fmt.Errorf("listen port %d out of range 1-65535", port)
		}
		a.Host, a.Port = "", port
		return nil
	}

	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", s, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("invalid listen port %q: %w", portStr, err)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("listen port %d out of range 1-65535", port)
	}
	if host != "*" && host != "" {
		if net.ParseIP(host) == nil {
			return fmt.Errorf("listen host %q is not a valid IP address (use * for all interfaces)", host)
		}
	}
	a.Host, a.Port = host, port
	return nil
}

// spec renders the address as written, without applying a default host.
func (a ListenAddr) spec() string {
	if a.Host == "" {
		return strconv.Itoa(a.Port)
	}
	return net.JoinHostPort(a.Host, strconv.Itoa(a.Port))
}

// Addr returns the address to hand to net.Listen. defaultHost is used when no
// host was configured; "*" (from either source) binds every interface.
func (a ListenAddr) Addr(defaultHost string) string {
	host := a.Host
	if host == "" {
		host = defaultHost
	}
	if host == "*" {
		host = ""
	}
	return net.JoinHostPort(host, strconv.Itoa(a.Port))
}

// IsLoopback reports whether the resolved host is a loopback address.
// Used to warn when the admin plane is exposed beyond the local host.
func (a ListenAddr) IsLoopback(defaultHost string) bool {
	host := a.Host
	if host == "" {
		host = defaultHost
	}
	if host == "*" || host == "" {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
