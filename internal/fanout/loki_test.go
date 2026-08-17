package fanout_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"obsdatalayer/internal/auth/authtest"
	"obsdatalayer/internal/config"
	"obsdatalayer/internal/fanout"
	"obsdatalayer/internal/metrics"
	"obsdatalayer/internal/middleware"
	"obsdatalayer/internal/proxy"
)

// testAuth is the shared stub authorizer for the fan-out HTTP tests.
var testAuth = authtest.New()

func newTestMetrics() *metrics.Metrics {
	return metrics.New(prometheus.NewRegistry())
}

func newTestConfig(instances []*config.InstanceConfig) *config.Config {
	byName := make(map[string]*config.InstanceConfig)
	for _, inst := range instances {
		byName[inst.Name] = inst
	}
	return &config.Config{
		Gateway: config.GatewayConfig{
			MaxBodyBytes: 32 * 1024 * 1024,
		},
		Instances: instances,
		ByName:    byName,
	}
}

func newTestMux(cfg *config.Config, p *proxy.Proxy, m *metrics.Metrics) http.Handler {
	h := config.NewHolder(cfg, "")
	mux := http.NewServeMux()
	fanout.RegisterLoki(mux, h, p, m)
	fanout.RegisterMimir(mux, h, p, m)
	fanout.RegisterTempo(mux, h, p)
	return middleware.BasicAuth(testAuth, mux)
}

func lokiInst(name string, url string) *config.InstanceConfig {
	return &config.InstanceConfig{
		Name:    name,
		Backend: "loki",
		URL:     url,
	}
}

// authHeader returns the Authorization header value for the stub credentials.
func authHeader() string {
	return testAuth.Header()
}

func TestLokiPushSuccess(t *testing.T) {
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{lokiInst("loki-prod", upstream.URL)})
	m := newTestMetrics()
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTestMux(cfg, p, m)

	body := `{"streams":[{"stream":{"app":"foo"},"values":[["1","log"]]}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/loki-prod/loki/push", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader())
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", rec.Code)
	}
	if receivedPath != "/loki/api/v1/push" {
		t.Errorf("expected upstream path /loki/api/v1/push, got %q", receivedPath)
	}
}

func TestLokiPushWithLabelsRewrite(t *testing.T) {
	var receivedBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		receivedBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	inst := &config.InstanceConfig{
		Name:    "loki-prod",
		Backend: "loki",
		URL:     upstream.URL,
		Labels: &config.LabelsConfig{
			Filter: &config.FilterConfig{Mode: "denylist", Names: []string{"env"}},
		},
	}
	cfg := newTestConfig([]*config.InstanceConfig{inst})
	m := newTestMetrics()
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTestMux(cfg, p, m)

	body := `{"streams":[{"stream":{"app":"foo","env":"staging"},"values":[["1","log"]]}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/loki-prod/loki/push", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader())
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", rec.Code)
	}
	// env label should be removed
	if strings.Contains(receivedBody, `"env"`) {
		t.Errorf("expected 'env' to be removed from rewritten body, got %s", receivedBody)
	}
	if !strings.Contains(receivedBody, `"app"`) {
		t.Errorf("expected 'app' to remain in rewritten body, got %s", receivedBody)
	}
}

func TestLokiPushUnknownInstance(t *testing.T) {
	cfg := newTestConfig([]*config.InstanceConfig{})
	m := newTestMetrics()
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTestMux(cfg, p, m)

	req := httptest.NewRequest(http.MethodPost, "/api/unknown/loki/push", strings.NewReader("{}"))
	req.Header.Set("Authorization", authHeader())
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "unknown instance" {
		t.Errorf("expected error='unknown instance', got %q", body["error"])
	}
}

func TestLokiQueryRange(t *testing.T) {
	var receivedPath, receivedQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"success"}`)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{lokiInst("loki-prod", upstream.URL)})
	m := newTestMetrics()
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTestMux(cfg, p, m)

	req := httptest.NewRequest(http.MethodGet, "/api/loki-prod/loki/query_range?query=xxx&start=1&end=2&step=1", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if receivedPath != "/loki/api/v1/query_range" {
		t.Errorf("expected path /loki/api/v1/query_range, got %q", receivedPath)
	}
	if !strings.Contains(receivedQuery, "query=xxx") {
		t.Errorf("expected query params forwarded, got %q", receivedQuery)
	}
}

func TestLokiInstantQuery(t *testing.T) {
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{lokiInst("loki-prod", upstream.URL)})
	m := newTestMetrics()
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTestMux(cfg, p, m)

	req := httptest.NewRequest(http.MethodGet, "/api/loki-prod/loki/query?query=xxx", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if receivedPath != "/loki/api/v1/query" {
		t.Errorf("expected path /loki/api/v1/query, got %q", receivedPath)
	}
}

func TestLokiLabels(t *testing.T) {
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{lokiInst("loki-prod", upstream.URL)})
	m := newTestMetrics()
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTestMux(cfg, p, m)

	req := httptest.NewRequest(http.MethodGet, "/api/loki-prod/loki/labels", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if receivedPath != "/loki/api/v1/labels" {
		t.Errorf("expected path /loki/api/v1/labels, got %q", receivedPath)
	}
}

func TestLokiLabelValues(t *testing.T) {
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{lokiInst("loki-prod", upstream.URL)})
	m := newTestMetrics()
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTestMux(cfg, p, m)

	req := httptest.NewRequest(http.MethodGet, "/api/loki-prod/loki/label/app/values", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if receivedPath != "/loki/api/v1/label/app/values" {
		t.Errorf("expected path /loki/api/v1/label/app/values, got %q", receivedPath)
	}
}

func TestLokiSeries(t *testing.T) {
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{lokiInst("loki-prod", upstream.URL)})
	m := newTestMetrics()
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTestMux(cfg, p, m)

	req := httptest.NewRequest(http.MethodGet, `/api/loki-prod/loki/series?match[]=xxx`, nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if receivedPath != "/loki/api/v1/series" {
		t.Errorf("expected path /loki/api/v1/series, got %q", receivedPath)
	}
}

func TestLokiMissingAuth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{lokiInst("loki-prod", upstream.URL)})
	m := newTestMetrics()
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTestMux(cfg, p, m)

	req := httptest.NewRequest(http.MethodGet, "/api/loki-prod/loki/labels", nil)
	// No Authorization header
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestLokiPushFanoutMultipleTargets(t *testing.T) {
	var count1, count2 atomic.Int32

	upstream1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count1.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream1.Close)

	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count2.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream2.Close)

	inst := &config.InstanceConfig{
		Name:       "loki-prod",
		Backend:    "loki",
		FanOutMode: "any",
		PushURLs: []config.PushTarget{
			{URL: upstream1.URL},
			{URL: upstream2.URL},
		},
	}
	cfg := newTestConfig([]*config.InstanceConfig{inst})
	m := newTestMetrics()
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTestMux(cfg, p, m)

	body := `{"streams":[{"stream":{"app":"foo"},"values":[["1","log"]]}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/loki-prod/loki/push", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader())
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", rec.Code)
	}
	if count1.Load() != 1 {
		t.Errorf("expected upstream1 to receive 1 request, got %d", count1.Load())
	}
	if count2.Load() != 1 {
		t.Errorf("expected upstream2 to receive 1 request, got %d", count2.Load())
	}
}
