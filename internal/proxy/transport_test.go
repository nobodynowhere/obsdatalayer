package proxy

import (
	"context"
	"net/http"
	"testing"
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
