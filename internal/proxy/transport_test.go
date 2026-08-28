package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"obsdatalayer/internal/config"
)

type markerTransport struct {
	name string
	seen *string
}

func (m markerTransport) RoundTrip(*http.Request) (*http.Response, error) {
	*m.seen = m.name
	return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
}

func TestTLSSwitchTransportUsesBaseByDefault(t *testing.T) {
	var seen string
	tr := &tlsSwitchTransport{
		base:     markerTransport{name: "base", seen: &seen},
		insecure: markerTransport{name: "insecure", seen: &seen},
	}
	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()
	if seen != "base" {
		t.Errorf("expected base transport, got %q", seen)
	}
}

func TestTLSSwitchTransportUsesInsecureWhenMarked(t *testing.T) {
	var seen string
	tr := &tlsSwitchTransport{
		base:     markerTransport{name: "base", seen: &seen},
		insecure: markerTransport{name: "insecure", seen: &seen},
	}
	req, err := http.NewRequestWithContext(
		WithSkipTLSVerify(context.Background()),
		http.MethodGet,
		"https://example.com",
		nil,
	)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()
	if seen != "insecure" {
		t.Errorf("expected insecure transport, got %q", seen)
	}
}

func TestNewTransportUsesScaleDefaults(t *testing.T) {
	tr := NewTransport().(*tlsSwitchTransport)
	base := tr.base.(*http.Transport)

	if base.MaxIdleConns != 10000 {
		t.Errorf("MaxIdleConns = %d, want 10000", base.MaxIdleConns)
	}
	if base.MaxIdleConnsPerHost != 10000 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 10000", base.MaxIdleConnsPerHost)
	}
	if base.MaxConnsPerHost != 0 {
		t.Errorf("MaxConnsPerHost = %d, want unlimited", base.MaxConnsPerHost)
	}
	if base.IdleConnTimeout != 90*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 90s", base.IdleConnTimeout)
	}
}

func TestNewTransportUsesConfiguredPool(t *testing.T) {
	tr := NewTransportWithConfig(config.TransportConfig{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 100,
		MaxConnsPerHost:     80,
		IdleConnTimeout:     config.Duration(45 * time.Second),
	}).(*tlsSwitchTransport)
	base := tr.base.(*http.Transport)
	insecure := tr.insecure.(*http.Transport)

	for name, got := range map[string]*http.Transport{"base": base, "insecure": insecure} {
		if got.MaxIdleConns != 200 {
			t.Errorf("%s MaxIdleConns = %d, want 200", name, got.MaxIdleConns)
		}
		if got.MaxIdleConnsPerHost != 100 {
			t.Errorf("%s MaxIdleConnsPerHost = %d, want 100", name, got.MaxIdleConnsPerHost)
		}
		if got.MaxConnsPerHost != 80 {
			t.Errorf("%s MaxConnsPerHost = %d, want 80", name, got.MaxConnsPerHost)
		}
		if got.IdleConnTimeout != 45*time.Second {
			t.Errorf("%s IdleConnTimeout = %v, want 45s", name, got.IdleConnTimeout)
		}
	}
}
