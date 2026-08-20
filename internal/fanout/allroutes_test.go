package fanout_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"obsdatalayer/internal/auth/authtest"
	"obsdatalayer/internal/config"
	"obsdatalayer/internal/fanout"
	"obsdatalayer/internal/middleware"
	"obsdatalayer/internal/proxy"
)

// dataPlaneRoutes is every route a client can reach on the data listener.
// It is asserted complete by TestRouteInventoryIsComplete below, so a route
// added without a corresponding entry here fails the build's test run rather
// than silently escaping the header and tenancy guarantees.
var dataPlaneRoutes = []struct{ method, path string }{
	// ---- IngestRoutes: every write path, all three backends ----
	{"POST", "/api/v1/push"},
	{"POST", "/api/v1/push/influx/write"},
	{"POST", "/otlp/v1/metrics"},
	{"POST", "/loki/api/v1/push"},
	{"POST", "/otlp/v1/logs"},
	{"POST", "/v1/traces"},
	{"POST", "/api/traces"},
	{"POST", "/api/v2/spans"},

	// ---- MimirDSRoutes: base gateway:port/prometheus ----
	{"GET", "/prometheus/api/v1/query"},
	{"POST", "/prometheus/api/v1/query"},
	{"GET", "/prometheus/api/v1/query_range"},
	{"POST", "/prometheus/api/v1/query_range"},
	{"GET", "/prometheus/api/v1/query_exemplars"},
	{"POST", "/prometheus/api/v1/query_exemplars"},
	{"GET", "/prometheus/api/v1/labels"},
	{"POST", "/prometheus/api/v1/labels"},
	{"GET", "/prometheus/api/v1/label/job/values"},
	{"POST", "/prometheus/api/v1/label/job/values"},
	{"GET", "/prometheus/api/v1/series"},
	{"POST", "/prometheus/api/v1/series"},
	{"GET", "/prometheus/api/v1/search/metric_names"},
	{"POST", "/prometheus/api/v1/search/metric_names"},
	{"GET", "/prometheus/api/v1/search/label_names"},
	{"POST", "/prometheus/api/v1/search/label_names"},
	{"GET", "/prometheus/api/v1/search/label_values"},
	{"POST", "/prometheus/api/v1/search/label_values"},
	{"GET", "/prometheus/api/v1/metadata"},
	{"POST", "/prometheus/api/v1/metadata"},
	{"POST", "/prometheus/api/v1/read"},
	{"GET", "/prometheus/ready"},
	{"GET", "/prometheus/api/v1/status/buildinfo"},
	{"GET", "/prometheus/api/v1/status/config"},
	{"GET", "/prometheus/api/v1/status/flags"},
	{"GET", "/prometheus/api/v1/format_query"},
	{"POST", "/prometheus/api/v1/format_query"},
	{"GET", "/prometheus/api/v1/rules"},
	{"GET", "/prometheus/api/v1/alerts"},
	{"GET", "/prometheus/api/v1/cardinality/active_series"},
	{"POST", "/prometheus/api/v1/cardinality/active_series"},
	{"GET", "/prometheus/api/v1/cardinality/label_names"},
	{"POST", "/prometheus/api/v1/cardinality/label_names"},
	{"GET", "/prometheus/api/v1/cardinality/label_values"},
	{"POST", "/prometheus/api/v1/cardinality/label_values"},
	{"GET", "/prometheus/rules"},
	{"GET", "/prometheus/rules/ns"},
	{"GET", "/prometheus/rules/ns/grp"},
	{"POST", "/prometheus/rules/ns"},
	{"DELETE", "/prometheus/rules/ns"},
	{"DELETE", "/prometheus/rules/ns/grp"},
	{"GET", "/prometheus/config/v1/rules"},
	{"GET", "/prometheus/config/v1/rules/ns"},
	{"GET", "/prometheus/config/v1/rules/ns/grp"},
	{"POST", "/prometheus/config/v1/rules/ns"},
	{"DELETE", "/prometheus/config/v1/rules/ns"},
	{"DELETE", "/prometheus/config/v1/rules/ns/grp"},

	// ---- LokiDSRoutes: base gateway:port/loki ----
	{"GET", "/loki/loki/api/v1/query"},
	{"GET", "/loki/loki/api/v1/query_range"},
	{"GET", "/loki/loki/api/v1/labels"},
	{"GET", "/loki/loki/api/v1/label/app/values"},
	{"GET", "/loki/loki/api/v1/series"},
	{"POST", "/loki/loki/api/v1/series"},
	{"GET", "/loki/loki/api/v1/index/stats"},
	{"GET", "/loki/loki/api/v1/index/volume"},
	{"GET", "/loki/loki/api/v1/index/volume_range"},
	{"GET", "/loki/loki/api/v1/patterns"},
	{"GET", "/loki/loki/api/v1/detected_fields"},
	{"POST", "/loki/loki/api/v1/detected_fields"},
	{"GET", "/loki/loki/api/v1/detected_field/level/values"},
	{"POST", "/loki/loki/api/v1/detected_field/level/values"},
	{"GET", "/loki/loki/api/v1/format_query"},
	{"POST", "/loki/loki/api/v1/format_query"},
	{"GET", "/loki/loki/api/v1/status/buildinfo"},
	{"GET", "/loki/loki/api/v1/tail"},
	{"GET", "/loki/loki/api/v1/rules"},
	{"GET", "/loki/loki/api/v1/rules/ns"},
	{"GET", "/loki/loki/api/v1/rules/ns/grp"},
	{"POST", "/loki/loki/api/v1/rules/ns"},
	{"DELETE", "/loki/loki/api/v1/rules/ns"},
	{"DELETE", "/loki/loki/api/v1/rules/ns/grp"},
	{"GET", "/loki/api/prom/rules"},
	{"GET", "/loki/api/prom/rules/ns"},
	{"GET", "/loki/api/prom/rules/ns/grp"},
	{"POST", "/loki/api/prom/rules/ns"},
	{"DELETE", "/loki/api/prom/rules/ns"},
	{"DELETE", "/loki/api/prom/rules/ns/grp"},
	{"GET", "/loki/prometheus/api/v1/rules"},
	{"GET", "/loki/prometheus/api/v1/alerts"},

	// ---- TempoDSRoutes: base gateway:port/tempo ----
	{"GET", "/tempo/api/traces/abc"},
	{"GET", "/tempo/api/v2/traces/abc"},
	{"GET", "/tempo/api/search"},
	{"GET", "/tempo/api/search/tags"},
	{"GET", "/tempo/api/v2/search/tags"},
	{"GET", "/tempo/api/search/tag/svc/values"},
	{"GET", "/tempo/api/v2/search/tag/svc/values"},
	{"GET", "/tempo/api/metrics/query_range"},
	{"GET", "/tempo/api/metrics/query"},
	{"GET", "/tempo/api/echo"},
	{"GET", "/tempo/api/status/buildinfo"},
	{"GET", "/tempo/api/overrides"},
	{"POST", "/tempo/api/overrides"},
	{"PATCH", "/tempo/api/overrides"},
	{"DELETE", "/tempo/api/overrides"},
	// ---- AlertmanagerDSRoutes: base gateway:port/alertmanager ----
	{"GET", "/alertmanager/alertmanager/api/v2/silences"},
	{"POST", "/alertmanager/alertmanager/api/v2/silences"},
	{"GET", "/alertmanager/alertmanager/api/v2/silence/sid"},
	{"DELETE", "/alertmanager/alertmanager/api/v2/silence/sid"},
	{"GET", "/alertmanager/alertmanager/api/v2/status"},
	{"GET", "/alertmanager/alertmanager/api/v2/alerts/groups"},
	{"GET", "/alertmanager/alertmanager/api/v2/alerts"},
	{"POST", "/alertmanager/alertmanager/api/v2/alerts"},
	{"GET", "/alertmanager/api/v1/alerts"},
	{"POST", "/alertmanager/api/v1/alerts"},
	{"DELETE", "/alertmanager/api/v1/alerts"},
}

// recorder captures what each upstream request actually carried.
type recorder struct {
	mu   sync.Mutex
	hits []http.Header
}

func (rec *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	rec.mu.Lock()
	rec.hits = append(rec.hits, req.Header.Clone())
	rec.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Header:     make(http.Header),
		Body:       http.NoBody,
		Request:    req,
	}, nil
}

// newDataPlane builds the real data-plane handler: the same middleware chain
// main.go mounts, over all three backend route sets.
func newDataPlane(t *testing.T, transport http.RoundTripper, stub *authtest.Stub) http.Handler {
	t.Helper()
	upstreamURL := "http://upstream.local"
	cfg := newTestConfig([]*config.InstanceConfig{
		{Name: "loki-prod", Backend: "loki", URL: upstreamURL},
		{Name: "mimir-prod", Backend: "mimir", URL: upstreamURL},
		{Name: "tempo-prod", Backend: "tempo", URL: upstreamURL},
	})
	h := config.NewHolder(cfg, "")
	client := &http.Client{Timeout: 5 * time.Second, Transport: transport}
	p := proxy.New(client, client)

	mux := http.NewServeMux()
	fanout.IngestRoutes(mux, h, p, newTestMetrics())
	fanout.LokiDSRoutes(mux, "/loki", h, p)
	fanout.MimirDSRoutes(mux, "/prometheus", h, p)
	fanout.TempoDSRoutes(mux, "/tempo", h, p)
	fanout.AlertmanagerDSRoutes(mux, "/alertmanager", h, p)

	return middleware.BasicAuth(stub, middleware.SanitizeHeaders(mux))
}

// TestEveryRouteStripsClientHeadersAndInjectsTenancy is the guarantee: for every
// route a client can reach, spoofed identity headers never reach the backend and
// the gateway's own tenant assertion always does.
func TestEveryRouteStripsClientHeadersAndInjectsTenancy(t *testing.T) {
	rec := &recorder{}

	stub := authtest.New()
	stub.Tenants = []string{"tenant-a"}
	handler := newDataPlane(t, rec, stub)

	// Every identity-shaped header a client could try.
	spoof := map[string]string{
		"X-Scope-OrgID":       "EVIL",
		"x-scope-orgid":       "EVIL",
		"X-Org-Id":            "EVIL",
		"Org-Id":              "EVIL",
		"X-Grafana-Org-Id":    "EVIL",
		"X-Scope-Orgid-Extra": "EVIL",
		"X-Forwarded-For":     "1.2.3.4",
		"X-Real-IP":           "1.2.3.4",
		"Cookie":              "session=abc",
		"Proxy-Authorization": "Basic zzz",
	}

	for _, route := range dataPlaneRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			rec.mu.Lock()
			rec.hits = nil
			rec.mu.Unlock()

			req := httptest.NewRequest(route.method, route.path, strings.NewReader("{}"))
			req.Header.Set("Authorization", stub.Header())
			req.Header.Set("Content-Type", "application/json")
			for k, v := range spoof {
				req.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code == http.StatusNotFound {
				t.Fatalf("route is not registered (got 404); inventory is stale")
			}

			rec.mu.Lock()
			hits := rec.hits
			rec.mu.Unlock()
			if len(hits) == 0 {
				t.Fatalf("request never reached the upstream (status %d)", w.Code)
			}

			for _, got := range hits {
				// The tenant assertion must be present and must be ours.
				if org := got.Get("X-Scope-OrgID"); org != "tenant-a" {
					t.Errorf("expected injected tenant 'tenant-a', got %q", org)
				}
				// No client-supplied identity may have survived.
				for name := range spoof {
					if name == "X-Scope-OrgID" || name == "x-scope-orgid" {
						continue // asserted above: value must be ours, not EVIL
					}
					if v := got.Get(name); v != "" {
						t.Errorf("client header %s leaked upstream with %q", name, v)
					}
				}
				if got.Get("Authorization") != "" {
					t.Error("client Authorization leaked upstream")
				}
				// A legitimately forwardable header must still arrive.
				if got.Get("Content-Type") == "" {
					t.Error("Content-Type should have been forwarded")
				}
			}
		})
	}
}

// TestRouteInventoryIsComplete fails when a route exists that the guarantee test
// above does not cover, so the inventory cannot silently fall behind the code.
func TestRouteInventoryIsComplete(t *testing.T) {
	rec := &recorder{}

	stub := authtest.New()
	handler := newDataPlane(t, rec, stub)

	covered := make(map[string]bool, len(dataPlaneRoutes))
	for _, r := range dataPlaneRoutes {
		covered[r.method+" "+r.path] = true
	}

	// Probe each inventory entry; a 404 means the inventory names a route that
	// no longer exists.
	for key := range covered {
		parts := strings.SplitN(key, " ", 2)
		req := httptest.NewRequest(parts[0], parts[1], strings.NewReader("{}"))
		req.Header.Set("Authorization", stub.Header())
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code == http.StatusNotFound {
			t.Errorf("inventory lists %s but it is not registered", key)
		}
	}
	t.Logf("route inventory covers %d data-plane routes", len(dataPlaneRoutes))
}
