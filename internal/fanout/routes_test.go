package fanout_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/fanout"
	"obsdatalayer/internal/middleware"
	"obsdatalayer/internal/proxy"
)

type captureTransport struct {
	method string
	path   string
	body   string
}

func (t *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.method = req.Method
	t.path = req.URL.Path
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

func TestMimirTenantConfigReadRejectsAmbiguousTenant(t *testing.T) {
	withAuthTenants(t, "tenant-a", "tenant-b")
	capture := &captureTransport{}
	cfg := newTestConfig([]*config.InstanceConfig{mimirInst("mimir-prod", "http://mimir.local")})
	h := newRouteOnlyMux(cfg, capture)

	req := httptest.NewRequest(http.MethodGet, "/api/mimir/config/v1/rules", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if capture.path != "" {
		t.Fatalf("upstream should not be called, got %q", capture.path)
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
