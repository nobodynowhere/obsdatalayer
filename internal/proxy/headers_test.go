package proxy_test

import (
	"net/http"
	"testing"

	"obsdatalayer/internal/proxy"
)

// The allowlist must carry everything the backends genuinely need. If one of
// these is ever dropped, ingest or querying breaks in a way that is hard to
// trace back to a header policy.
func TestRequiredHeadersAreForwardable(t *testing.T) {
	required := []string{
		"Content-Type",     // remote_write protobuf, OTLP, JSON pushes
		"Content-Encoding", // snappy / gzip
		"Accept",
		"Accept-Encoding",
		"User-Agent",
		"X-Prometheus-Remote-Write-Version", // Mimir rejects writes without it
		"X-Prometheus-Remote-Read-Version",  // remote_read
		"X-Prometheus-Remote-Write-Protobuf-Message", // remote-write 2.0
		"X-Query-Tags",  // Loki query attribution
		"Cache-Control", // Mimir query-frontend no-store
		"Traceparent",   // W3C trace context
		"Tracestate",
	}
	for _, h := range required {
		if !proxy.IsForwardable(h) {
			t.Errorf("%s must be forwardable; dropping it breaks a backend", h)
		}
	}
}

// Anything that asserts identity, or that an intermediary might trust as
// identity, must never be relayed from the client.
func TestIdentityHeadersAreNotForwardable(t *testing.T) {
	blocked := []string{
		"X-Scope-Orgid", // the tenant assertion; the gateway sets it
		"Authorization", // client credential
		"Proxy-Authorization",
		"Cookie",
		"X-Org-Id",
		"Org-Id",
		"X-Grafana-Org-Id",
		"X-Scope-Orgid-Extra",
		"X-Forwarded-For",
		"X-Real-Ip",
		"Connection",
		"Upgrade",
	}
	for _, h := range blocked {
		if proxy.IsForwardable(h) {
			t.Errorf("%s must not be relayed from the client", h)
		}
	}
}

// The policy is an allowlist: an unknown header defaults to dropped. This is
// the property that stops a future header becoming an identity channel.
func TestUnknownHeadersDefaultToDropped(t *testing.T) {
	for _, h := range []string{"X-Some-Future-Header", "X-Tenant", "X-Custom-Auth"} {
		if proxy.IsForwardable(h) {
			t.Errorf("unknown header %s should default to dropped", h)
		}
	}
}

func TestSanitizeInboundStripsAndReports(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-Scope-OrgID", "EVIL")
	h.Set("Authorization", "Basic zzz")
	h.Set("X-Org-Id", "EVIL")
	h.Set("Cookie", "session=abc")

	dropped := proxy.SanitizeInbound(h)

	if got := h.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type should survive, got %q", got)
	}
	for _, gone := range []string{"X-Scope-OrgID", "Authorization", "X-Org-Id", "Cookie"} {
		if h.Get(gone) != "" {
			t.Errorf("%s should have been stripped", gone)
		}
	}
	if len(dropped) != 4 {
		t.Errorf("expected 4 dropped headers reported, got %v", dropped)
	}
}

// Casing variants collapse onto the canonical key, so a lowercase spoof is
// stripped the same as a canonical one.
func TestSanitizeInboundIsCaseInsensitiveViaCanonicalKeys(t *testing.T) {
	h := http.Header{}
	h.Set("x-scope-orgid", "EVIL")
	h.Set("X-SCOPE-ORGID", "EVIL2")
	proxy.SanitizeInbound(h)
	if len(h) != 0 {
		t.Errorf("expected all casing variants stripped, got %v", h)
	}
}

// Multi-valued headers must be dropped entirely, not just their first value.
func TestSanitizeInboundDropsAllValues(t *testing.T) {
	h := http.Header{}
	h.Add("X-Scope-OrgID", "a")
	h.Add("X-Scope-OrgID", "b")
	proxy.SanitizeInbound(h)
	if vals := h.Values("X-Scope-OrgID"); len(vals) != 0 {
		t.Errorf("expected every value dropped, got %v", vals)
	}
}
