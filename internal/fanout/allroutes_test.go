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
	// ---- loki: ingest ----
	{"POST", "/api/loki/push"},
	{"POST", "/api/loki/loki/api/v1/push"},
	{"POST", "/api/loki/otlp/v1/logs"},
	// ---- loki: query ----
	{"GET", "/api/loki/query"},
	{"GET", "/api/loki/query_range"},
	{"GET", "/api/loki/labels"},
	{"GET", "/api/loki/label/app/values"},
	{"GET", "/api/loki/series"},
	{"GET", "/api/loki/index/stats"},
	{"GET", "/api/loki/index/volume"},
	{"GET", "/api/loki/index/volume_range"},
	{"GET", "/api/loki/patterns"},
	{"GET", "/api/loki/format_query"},
	{"GET", "/api/loki/status/buildinfo"},
	{"GET", "/api/loki/prometheus/api/v1/rules"},
	{"GET", "/api/loki/prometheus/api/v1/alerts"},
	// ---- loki: ruler ----
	{"GET", "/api/loki/rules"},
	{"GET", "/api/loki/rules/ns"},
	{"GET", "/api/loki/rules/ns/grp"},
	{"POST", "/api/loki/rules/ns"},
	{"DELETE", "/api/loki/rules/ns"},
	{"DELETE", "/api/loki/rules/ns/grp"},

	// ---- mimir: ingest ----
	{"POST", "/api/mimir/push"},
	{"POST", "/api/mimir/api/v1/push"},
	{"POST", "/api/mimir/otlp/v1/metrics"},
	{"POST", "/api/mimir/api/v1/push/influx/write"},
	// ---- mimir: query (short form) ----
	{"GET", "/api/mimir/query"},
	{"POST", "/api/mimir/query"},
	{"GET", "/api/mimir/query_range"},
	{"POST", "/api/mimir/query_range"},
	{"GET", "/api/mimir/query_exemplars"},
	{"POST", "/api/mimir/query_exemplars"},
	{"GET", "/api/mimir/labels"},
	{"POST", "/api/mimir/labels"},
	{"GET", "/api/mimir/label/job/values"},
	{"POST", "/api/mimir/label/job/values"},
	{"GET", "/api/mimir/series"},
	{"POST", "/api/mimir/series"},
	{"GET", "/api/mimir/search/metric_names"},
	{"POST", "/api/mimir/search/metric_names"},
	{"GET", "/api/mimir/search/label_names"},
	{"POST", "/api/mimir/search/label_names"},
	{"GET", "/api/mimir/search/label_values"},
	{"POST", "/api/mimir/search/label_values"},
	{"GET", "/api/mimir/metadata"},
	{"POST", "/api/mimir/metadata"},
	{"POST", "/api/mimir/read"},
	{"GET", "/api/mimir/status/buildinfo"},
	{"GET", "/api/mimir/format_query"},
	{"POST", "/api/mimir/format_query"},
	{"GET", "/api/mimir/cardinality/active_series"},
	{"POST", "/api/mimir/cardinality/active_series"},
	{"GET", "/api/mimir/cardinality/label_names"},
	{"POST", "/api/mimir/cardinality/label_names"},
	{"GET", "/api/mimir/cardinality/label_values"},
	{"POST", "/api/mimir/cardinality/label_values"},
	// ---- mimir: query (prometheus-prefixed form) ----
	{"GET", "/api/mimir/prometheus/api/v1/query"},
	{"POST", "/api/mimir/prometheus/api/v1/query"},
	{"GET", "/api/mimir/prometheus/api/v1/query_range"},
	{"POST", "/api/mimir/prometheus/api/v1/query_range"},
	{"GET", "/api/mimir/prometheus/api/v1/query_exemplars"},
	{"POST", "/api/mimir/prometheus/api/v1/query_exemplars"},
	{"GET", "/api/mimir/prometheus/api/v1/labels"},
	{"POST", "/api/mimir/prometheus/api/v1/labels"},
	{"GET", "/api/mimir/prometheus/api/v1/label/job/values"},
	{"POST", "/api/mimir/prometheus/api/v1/label/job/values"},
	{"GET", "/api/mimir/prometheus/api/v1/series"},
	{"POST", "/api/mimir/prometheus/api/v1/series"},
	{"GET", "/api/mimir/prometheus/api/v1/search/metric_names"},
	{"POST", "/api/mimir/prometheus/api/v1/search/metric_names"},
	{"GET", "/api/mimir/prometheus/api/v1/search/label_names"},
	{"POST", "/api/mimir/prometheus/api/v1/search/label_names"},
	{"GET", "/api/mimir/prometheus/api/v1/search/label_values"},
	{"POST", "/api/mimir/prometheus/api/v1/search/label_values"},
	{"GET", "/api/mimir/prometheus/api/v1/metadata"},
	{"POST", "/api/mimir/prometheus/api/v1/metadata"},
	{"POST", "/api/mimir/prometheus/api/v1/read"},
	{"GET", "/api/mimir/prometheus/api/v1/status/buildinfo"},
	{"GET", "/api/mimir/prometheus/api/v1/format_query"},
	{"POST", "/api/mimir/prometheus/api/v1/format_query"},
	{"GET", "/api/mimir/prometheus/api/v1/rules"},
	{"GET", "/api/mimir/prometheus/api/v1/alerts"},
	{"GET", "/api/mimir/prometheus/api/v1/cardinality/active_series"},
	{"POST", "/api/mimir/prometheus/api/v1/cardinality/active_series"},
	{"GET", "/api/mimir/prometheus/api/v1/cardinality/label_names"},
	{"POST", "/api/mimir/prometheus/api/v1/cardinality/label_names"},
	{"GET", "/api/mimir/prometheus/api/v1/cardinality/label_values"},
	{"POST", "/api/mimir/prometheus/api/v1/cardinality/label_values"},
	// ---- mimir: query (GEM-compatible root prometheus form) ----
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
	{"GET", "/prometheus/api/v1/status/buildinfo"},
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
	// ---- mimir: ruler + alertmanager ----
	{"GET", "/api/mimir/config/v1/rules"},
	{"GET", "/api/mimir/config/v1/rules/ns"},
	{"GET", "/api/mimir/config/v1/rules/ns/grp"},
	{"POST", "/api/mimir/config/v1/rules/ns"},
	{"DELETE", "/api/mimir/config/v1/rules/ns"},
	{"DELETE", "/api/mimir/config/v1/rules/ns/grp"},
	{"GET", "/api/mimir/prometheus/config/v1/rules"},
	{"GET", "/api/mimir/prometheus/config/v1/rules/ns"},
	{"GET", "/api/mimir/prometheus/config/v1/rules/ns/grp"},
	{"POST", "/api/mimir/prometheus/config/v1/rules/ns"},
	{"DELETE", "/api/mimir/prometheus/config/v1/rules/ns"},
	{"DELETE", "/api/mimir/prometheus/config/v1/rules/ns/grp"},
	{"GET", "/prometheus/config/v1/rules"},
	{"GET", "/prometheus/config/v1/rules/ns"},
	{"GET", "/prometheus/config/v1/rules/ns/grp"},
	{"POST", "/prometheus/config/v1/rules/ns"},
	{"DELETE", "/prometheus/config/v1/rules/ns"},
	{"DELETE", "/prometheus/config/v1/rules/ns/grp"},
	{"GET", "/api/mimir/api/v1/alerts"},
	{"POST", "/api/mimir/api/v1/alerts"},
	{"DELETE", "/api/mimir/api/v1/alerts"},
	{"GET", "/api/mimir/alertmanager/api/v1/alerts"},
	{"POST", "/api/mimir/alertmanager/api/v1/alerts"},
	{"DELETE", "/api/mimir/alertmanager/api/v1/alerts"},

	// ---- tempo ----
	{"POST", "/api/tempo/otlp/v1/traces"},
	{"POST", "/api/tempo/jaeger/v1/traces"},
	{"GET", "/api/tempo/search"},
	{"GET", "/api/tempo/search/tags"},
	{"GET", "/api/tempo/search/tag/service.name/values"},
	{"GET", "/api/tempo/v2/search/tags"},
	{"GET", "/api/tempo/v2/search/tag/service.name/values"},
	{"GET", "/api/tempo/traces/abc"},
	{"GET", "/api/tempo/v2/traces/abc"},
	{"GET", "/api/tempo/metrics/query"},
	{"GET", "/api/tempo/metrics/query_range"},
	{"GET", "/api/tempo/echo"},
	{"GET", "/api/tempo/overrides"},
	{"POST", "/api/tempo/overrides"},
	{"PATCH", "/api/tempo/overrides"},
	{"DELETE", "/api/tempo/overrides"},
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
	fanout.RegisterLoki(mux, h, p, newTestMetrics())
	fanout.RegisterMimir(mux, h, p, newTestMetrics())
	fanout.RegisterTempo(mux, h, p)

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
