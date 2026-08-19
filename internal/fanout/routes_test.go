package fanout_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/fanout"
	"obsdatalayer/internal/metrics"
	"obsdatalayer/internal/middleware"
	"obsdatalayer/internal/proxy"
)

type captureTransport struct {
	method   string
	host     string
	path     string
	rawQuery string
	body     string
}

func (t *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.method = req.Method
	t.host = req.URL.Host
	t.path = req.URL.Path
	t.rawQuery = req.URL.RawQuery
	if req.Body != nil {
		body, _ := io.ReadAll(req.Body)
		t.body = string(body)
	}
	return &http.Response{
		StatusCode: http.StatusAccepted,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("{}")),
		Request:    req,
	}, nil
}

func newRouteOnlyMux(cfg *config.Config, transport http.RoundTripper) http.Handler {
	h := config.NewHolder(cfg, "")
	client := &http.Client{Timeout: 5 * time.Second, Transport: transport}
	p := proxy.New(client, client)
	mux := http.NewServeMux()
	m := newTestMetrics()
	fanout.RegisterLoki(mux, h, p, m)
	fanout.RegisterMimir(mux, h, p, m)
	fanout.RegisterTempo(mux, h, p)
	return middleware.BasicAuth(testAuth, mux)
}

func withAuthSelectors(t *testing.T, selectors ...string) {
	t.Helper()
	previous := append([]string(nil), testAuth.LabelSelectors...)
	testAuth.LabelSelectors = append([]string(nil), selectors...)
	t.Cleanup(func() {
		testAuth.LabelSelectors = previous
	})
}

func TestMimirRuleWriteForwardsToPrometheusConfigAPI(t *testing.T) {
	withAuthTenants(t, "tenant-a")
	capture := &captureTransport{}
	cfg := newTestConfig([]*config.InstanceConfig{mimirInst("mimir-prod", "http://mimir.local")})
	h := newRouteOnlyMux(cfg, capture)

	req := httptest.NewRequest(http.MethodPost, "/api/mimir/prometheus/config/v1/rules/team-a", strings.NewReader("name: group-a"))
	req.Header.Set("Authorization", authHeader())
	req.Header.Set("Content-Type", "application/yaml")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}
	if capture.method != http.MethodPost || capture.path != "/prometheus/config/v1/rules/team-a" {
		t.Fatalf("expected POST /prometheus/config/v1/rules/team-a, got %s %s", capture.method, capture.path)
	}
	if capture.body != "name: group-a" {
		t.Fatalf("expected rule body to be forwarded, got %q", capture.body)
	}
}

func TestMimirAlertmanagerConfigWriteForwards(t *testing.T) {
	withAuthTenants(t, "tenant-a")
	capture := &captureTransport{}
	cfg := newTestConfig([]*config.InstanceConfig{mimirInst("mimir-prod", "http://mimir.local")})
	h := newRouteOnlyMux(cfg, capture)

	req := httptest.NewRequest(http.MethodPost, "/api/mimir/alertmanager/api/v1/alerts", strings.NewReader("route: {}"))
	req.Header.Set("Authorization", authHeader())
	req.Header.Set("Content-Type", "application/yaml")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}
	if capture.method != http.MethodPost || capture.path != "/api/v1/alerts" {
		t.Fatalf("expected POST /api/v1/alerts, got %s %s", capture.method, capture.path)
	}
}

func TestMimirRootPrometheusBuildInfoForwards(t *testing.T) {
	withAuthTenants(t, "tenant-a")
	capture := &captureTransport{}
	cfg := newTestConfig([]*config.InstanceConfig{{
		Name:    "mimir-prod",
		Backend: "mimir",
		URL:     "http://query.local",
		PushURLs: []config.PushTarget{
			{URL: "http://first.local"},
			{URL: "http://second.local"},
		},
	}})
	h := newRouteOnlyMux(cfg, capture)

	req := httptest.NewRequest(http.MethodGet, "/prometheus/api/v1/status/buildinfo", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}
	if capture.method != http.MethodGet || capture.host != "first.local" || capture.path != "/prometheus/api/v1/status/buildinfo" {
		t.Fatalf("expected GET first.local/prometheus/api/v1/status/buildinfo, got %s %s%s", capture.method, capture.host, capture.path)
	}
}

func TestMimirRootReadyForwards(t *testing.T) {
	withAuthTenants(t, "tenant-a")
	capture := &captureTransport{}
	cfg := newTestConfig([]*config.InstanceConfig{{
		Name:    "mimir-prod",
		Backend: "mimir",
		URL:     "http://query.local",
		PushURLs: []config.PushTarget{
			{URL: "http://first.local"},
			{URL: "http://second.local"},
		},
	}})
	h := newRouteOnlyMux(cfg, capture)

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}
	if capture.method != http.MethodGet || capture.host != "first.local" || capture.path != "/ready" {
		t.Fatalf("expected GET first.local/ready, got %s %s%s", capture.method, capture.host, capture.path)
	}
}

func TestMimirPrometheusStatusConfigForwardsToStatusConfig(t *testing.T) {
	withAuthTenants(t, "tenant-a")
	capture := &captureTransport{}
	cfg := newTestConfig([]*config.InstanceConfig{mimirInst("mimir-prod", "http://mimir.local")})
	h := newRouteOnlyMux(cfg, capture)

	req := httptest.NewRequest(http.MethodGet, "/prometheus/api/v1/status/config", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}
	if capture.method != http.MethodGet || capture.path != "/api/v1/status/config" {
		t.Fatalf("expected GET /api/v1/status/config, got %s %s", capture.method, capture.path)
	}
}

func TestMimirSearchMetricNamesAppliesReadPolicy(t *testing.T) {
	withAuthTenants(t, "tenant-a")
	withAuthSelectors(t, `{cluster="prod"}`)
	capture := &captureTransport{}
	cfg := newTestConfig([]*config.InstanceConfig{mimirInst("mimir-prod", "http://mimir.local")})
	h := newRouteOnlyMux(cfg, capture)

	req := httptest.NewRequest(http.MethodGet, "/prometheus/api/v1/search/metric_names", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}
	if capture.method != http.MethodGet || capture.path != "/prometheus/api/v1/search/metric_names" {
		t.Fatalf("expected GET /prometheus/api/v1/search/metric_names, got %s %s", capture.method, capture.path)
	}
	if capture.rawQuery != "match%5B%5D=%7Bcluster%3D%22prod%22%7D" {
		t.Fatalf("expected read policy match[] query, got %q", capture.rawQuery)
	}
}

func TestMimirTenantConfigReadAllowsMultiTenant(t *testing.T) {
	withAuthTenants(t, "tenant-a", "tenant-b")
	capture := &captureTransport{}
	cfg := newTestConfig([]*config.InstanceConfig{mimirInst("mimir-prod", "http://mimir.local")})
	h := newRouteOnlyMux(cfg, capture)

	req := httptest.NewRequest(http.MethodGet, "/api/mimir/config/v1/rules", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}
	if capture.method != http.MethodGet || capture.path != "/prometheus/config/v1/rules" {
		t.Fatalf("expected GET /prometheus/config/v1/rules, got %s %s", capture.method, capture.path)
	}
}

func TestLokiRuleWriteForwardsToRulerAPI(t *testing.T) {
	withAuthTenants(t, "tenant-a")
	capture := &captureTransport{}
	cfg := newTestConfig([]*config.InstanceConfig{lokiInst("loki-prod", "http://loki.local")})
	h := newRouteOnlyMux(cfg, capture)

	req := httptest.NewRequest(http.MethodDelete, "/api/loki/rules/team-a/group-a", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}
	if capture.method != http.MethodDelete || capture.path != "/loki/api/v1/rules/team-a/group-a" {
		t.Fatalf("expected DELETE /loki/api/v1/rules/team-a/group-a, got %s %s", capture.method, capture.path)
	}
}

func TestTempoMetricsQueryForwards(t *testing.T) {
	withAuthTenants(t, "tenant-a")
	capture := &captureTransport{}
	cfg := newTestConfig([]*config.InstanceConfig{tempoInst("tempo-prod", "http://tempo.local")})
	h := newRouteOnlyMux(cfg, capture)

	req := httptest.NewRequest(http.MethodGet, "/api/tempo/metrics/query_range?q={}", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}
	if capture.method != http.MethodGet || capture.path != "/api/metrics/query_range" {
		t.Fatalf("expected GET /api/metrics/query_range, got %s %s", capture.method, capture.path)
	}
}

func TestLokiPushRecordsPayloadCounters(t *testing.T) {
	withAuthTenants(t, "tenant-a")
	capture := &captureTransport{}
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	cfg := newTestConfig([]*config.InstanceConfig{{
		Name:    "loki-prod",
		Backend: "loki",
		URL:     "http://loki.local",
		Labels: &config.LabelsConfig{
			Filter: &config.FilterConfig{Mode: "denylist", Names: []string{"env"}},
			Inject: map[string]string{"cluster": "prod"},
		},
	}})
	h := config.NewHolder(cfg, "")
	client := &http.Client{Timeout: 5 * time.Second, Transport: capture}
	p := proxy.New(client, client)
	mux := http.NewServeMux()
	fanout.RegisterLoki(mux, h, p, m)
	handler := middleware.BasicAuth(testAuth, mux)

	body := `{"streams":[{"stream":{"app":"api","env":"dev"},"values":[["1","a"]]},{"stream":{"app":"worker"},"values":[["2","b"]]}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/loki/push", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader())
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}
	assertCounter(t, m.WriteItemsValue("loki", "loki-prod", "streams", "received"), 2)
	assertCounter(t, m.WriteItemsValue("loki", "loki-prod", "streams", "modified"), 2)
	assertCounter(t, m.WriteItemsValue("loki", "loki-prod", "streams", "unchanged"), 0)
	assertCounter(t, m.WriteItemsValue("loki", "loki-prod", "streams", "forwarded"), 2)
	assertCounter(t, m.RewriteLabelsValue("loki", "loki-prod", "dropped"), 1)
	assertCounter(t, m.RewriteLabelsValue("loki", "loki-prod", "injected"), 2)
}

func assertCounter(t *testing.T, got uint64, want uint64) {
	t.Helper()
	if got != want {
		t.Fatalf("expected counter %v, got %v", want, got)
	}
}
