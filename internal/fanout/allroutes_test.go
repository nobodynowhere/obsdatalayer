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

// tenantedRoutes is every route a client can reach on the data listener that
// carries a tenant's data, and must therefore leave the gateway with the
// gateway's own X-Scope-OrgID on it.
//
// The operational routes are the other kind and are listed separately in
// untenantedRoutes, because for those the guarantee is inverted: they must
// leave with no tenant assertion at all. Together the two lists are asserted
// complete by TestRouteInventoryIsComplete below -- in both directions, so a
// route added without an entry fails the build's test run rather than silently
// escaping whichever guarantee applies to it.
var tenantedRoutes = []struct{ method, path string }{
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
	{"GET", "/loki/loki/api/v1/tail"},
	{"GET", "/loki/loki/api/v1/delete"},
	{"POST", "/loki/loki/api/v1/delete"},
	{"PUT", "/loki/loki/api/v1/delete"},
	{"DELETE", "/loki/loki/api/v1/delete"},
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

// untenantedRoutes is the operational family: one route per allowlisted
// endpoint per backend. It is derived from the registration table rather than
// typed out, so adding an endpoint cannot leave a route uninventoried -- and
// the reverse check below still fails if the table grows a backend whose mount
// is not registered.
func untenantedRoutes() []struct{ method, path string } {
	// The two Mimir configuration dumps still answer at their
	// Prometheus-compatible paths, but they are forwarded operationally, so
	// they belong to the untenanted guarantee rather than the tenanted one.
	out := []struct{ method, path string }{
		{"GET", "/prometheus/api/v1/status/config"},
		{"GET", "/prometheus/api/v1/status/flags"},

		// The health checks a Grafana data source calls unprompted. They stay
		// on a read grant and keep answering at the paths Grafana expects, but
		// they are forwarded by ForwardHealth, which sends no tenant
		// assertion -- so the untenanted guarantee is the one that applies to
		// them, not the tenanted one. See getHealthInstance.
		{"GET", "/prometheus/ready"},
		{"GET", "/prometheus/api/v1/status/buildinfo"},
		{"GET", "/alertmanager/api/v1/status/buildinfo"},
		{"GET", "/loki/loki/api/v1/status/buildinfo"},
		{"GET", "/tempo/api/echo"},
		{"GET", "/tempo/api/status/buildinfo"},
	}
	for backend, mount := range fanout.BackendMounts {
		for _, e := range fanout.OperationalEndpoints(backend) {
			out = append(out, struct{ method, path string }{
				"GET", mount + "/targets/" + backend + "-prod/" + e.Alias,
			})
		}
	}
	return out
}

func allInventoryRoutes() []struct{ method, path string } {
	return append(append([]struct{ method, path string }{}, tenantedRoutes...), untenantedRoutes()...)
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

	return middleware.BasicAuth(stub, nil, middleware.SanitizeHeaders(mux))
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

	for _, route := range tenantedRoutes {
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

// TestEveryOperationalRouteSendsNoTenancy is the inverted guarantee. Everywhere
// in the test above the gateway must assert a tenant; on these routes it must
// assert none, because the endpoints behind them are not registered inside
// their backend's tenant middleware and a tenant header would be a claim the
// answer does not honour. Spoofed identity headers are dropped here just the
// same.
func TestEveryOperationalRouteSendsNoTenancy(t *testing.T) {
	rec := &recorder{}

	stub := authtest.New()
	stub.Tenants = []string{"tenant-a"}
	handler := newDataPlane(t, rec, stub)

	spoof := map[string]string{
		"X-Scope-OrgID":    "EVIL",
		"x-scope-orgid":    "EVIL",
		"X-Org-Id":         "EVIL",
		"X-Grafana-Org-Id": "EVIL",
		"X-Forwarded-For":  "1.2.3.4",
		"Cookie":           "session=abc",
	}

	for _, route := range untenantedRoutes() {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			rec.mu.Lock()
			rec.hits = nil
			rec.mu.Unlock()

			req := httptest.NewRequest(route.method, route.path, nil)
			req.Header.Set("Authorization", stub.Header())
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
				if org := got.Get("X-Scope-OrgID"); org != "" {
					t.Errorf("operational route carried X-Scope-OrgID %q; it must carry none", org)
				}
				for name := range spoof {
					if v := got.Get(name); v != "" {
						t.Errorf("client header %s leaked upstream with %q", name, v)
					}
				}
				if got.Get("Authorization") != "" {
					t.Error("client Authorization leaked upstream")
				}
			}
		})
	}
}

// recordingRegistrar collects the patterns a route bundle registers. See the
// Registrar doc comment for why the bundles take an interface.
type recordingRegistrar struct{ patterns []string }

func (r *recordingRegistrar) Handle(pattern string, _ http.Handler) {
	r.patterns = append(r.patterns, pattern)
}

func (r *recordingRegistrar) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	r.patterns = append(r.patterns, pattern)
}

// TestNoRouteEscapesTheInventory is the direction that was missing, and the one
// that matters: it fails when a route exists that neither inventory names.
//
// Checking only that each listed route is registered -- which is all the test
// below does -- cannot catch a new route, because a new route makes no listed
// route disappear. So every bundle is registered against a recorder, each
// recorded pattern is mounted on a mux of its own, and every inventory entry is
// dispatched through it. A pattern nothing dispatches to is a route no
// guarantee test covers.
func TestNoRouteEscapesTheInventory(t *testing.T) {
	h := config.NewHolder(newTestConfig(nil), "")
	p := proxy.New(http.DefaultClient, http.DefaultClient)

	reg := &recordingRegistrar{}
	fanout.IngestRoutes(reg, h, p, newTestMetrics())
	fanout.LokiDSRoutes(reg, "/loki", h, p)
	fanout.MimirDSRoutes(reg, "/prometheus", h, p)
	fanout.TempoDSRoutes(reg, "/tempo", h, p)
	fanout.AlertmanagerDSRoutes(reg, "/alertmanager", h, p)

	hit := make(map[string]bool, len(reg.patterns))
	probe := http.NewServeMux()
	for _, pattern := range reg.patterns {
		pattern := pattern
		probe.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) { hit[pattern] = true })
	}

	for _, route := range allInventoryRoutes() {
		probe.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(route.method, route.path, nil))
	}

	for _, pattern := range reg.patterns {
		if !hit[pattern] {
			t.Errorf("route %q is registered but no inventory entry reaches it; "+
				"add it to tenantedRoutes, or to the operational table if it is untenanted", pattern)
		}
	}
	t.Logf("inventory reaches all %d registered data-plane routes", len(reg.patterns))
}

// TestRouteInventoryIsComplete fails when a route exists that the guarantee test
// above does not cover, so the inventory cannot silently fall behind the code.
func TestRouteInventoryIsComplete(t *testing.T) {
	rec := &recorder{}

	stub := authtest.New()
	handler := newDataPlane(t, rec, stub)

	covered := make(map[string]bool, len(allInventoryRoutes()))
	for _, r := range allInventoryRoutes() {
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
	t.Logf("route inventory covers %d data-plane routes", len(allInventoryRoutes()))
}
