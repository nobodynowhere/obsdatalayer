package fanout_test

import (
	"encoding/json"
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

func mimirInst(name string, url string) *config.InstanceConfig {
	return &config.InstanceConfig{
		Name:    name,
		Backend: "mimir",
		URL:     url,
	}
}

func newMimirTestMux(cfg *config.Config, p *proxy.Proxy) http.Handler {
	h := config.NewHolder(cfg, "")
	mux := http.NewServeMux()
	fanout.IngestRoutes(mux, h, p, newTestMetrics())
	fanout.LokiDSRoutes(mux, "/loki", h, p)
	fanout.MimirDSRoutes(mux, "/prometheus", h, p)
	fanout.TempoDSRoutes(mux, "/tempo", h, p)
	return middleware.BasicAuth(testAuth, nil, mux)
}

func withAuthTenants(t *testing.T, tenants ...string) {
	t.Helper()
	previous := append([]string(nil), testAuth.Tenants...)
	testAuth.Tenants = append([]string(nil), tenants...)
	t.Cleanup(func() {
		testAuth.Tenants = previous
	})
}

func TestMimirPushSuccess(t *testing.T) {
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{mimirInst("mimir-prod", upstream.URL)})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newMimirTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/push", strings.NewReader("body"))
	req.Header.Set("Authorization", authHeader())
	req.Header.Set("Content-Type", "application/x-protobuf")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", rec.Code)
	}
	if receivedPath != "/api/v1/push" {
		t.Errorf("expected upstream path /api/v1/push, got %q", receivedPath)
	}
}

func TestMimirPushInjectsConfiguredTenantWhenGranted(t *testing.T) {
	withAuthTenants(t, "tenant-a", "tenant-b")

	var receivedOrgID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedOrgID = r.Header.Get("X-Scope-OrgID")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	inst := &config.InstanceConfig{
		Name:     "mimir-prod",
		Backend:  "mimir",
		TenantID: "tenant-b",
		PushURLs: []config.PushTarget{
			{URL: upstream.URL},
		},
	}
	cfg := newTestConfig([]*config.InstanceConfig{inst})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newMimirTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/push", strings.NewReader("body"))
	req.Header.Set("Authorization", authHeader())
	req.Header.Set("Content-Type", "application/x-protobuf")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}
	if receivedOrgID != "tenant-b" {
		t.Errorf("expected configured tenant tenant-b to be injected, got %q", receivedOrgID)
	}
}

func TestMimirPushRejectsConfiguredTenantWithoutGrant(t *testing.T) {
	withAuthTenants(t, "tenant-a")

	var called bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	inst := &config.InstanceConfig{
		Name:     "mimir-prod",
		Backend:  "mimir",
		TenantID: "tenant-b",
		PushURLs: []config.PushTarget{
			{URL: upstream.URL},
		},
	}
	cfg := newTestConfig([]*config.InstanceConfig{inst})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newMimirTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/push", strings.NewReader("body"))
	req.Header.Set("Authorization", authHeader())
	req.Header.Set("Content-Type", "application/x-protobuf")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	if called {
		t.Error("upstream should not be called when the configured tenant is not granted")
	}
}

func TestMimirPushInjectsSingleGrantedTenantWhenInstanceIsUnscoped(t *testing.T) {
	withAuthTenants(t, "tenant-a")

	var receivedOrgID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedOrgID = r.Header.Get("X-Scope-OrgID")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{mimirInst("mimir-prod", upstream.URL)})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newMimirTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/push", strings.NewReader("body"))
	req.Header.Set("Authorization", authHeader())
	req.Header.Set("Content-Type", "application/x-protobuf")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}
	if receivedOrgID != "tenant-a" {
		t.Errorf("expected granted tenant tenant-a to be injected, got %q", receivedOrgID)
	}
}

func TestMimirPushPrefersTenantDedicatedInstance(t *testing.T) {
	withAuthTenants(t, "tenant-a")

	var dedicatedCalls, sharedCalls int
	dedicated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dedicatedCalls++
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(dedicated.Close)
	shared := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sharedCalls++
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(shared.Close)

	cfg := newTestConfig([]*config.InstanceConfig{
		{Name: "mimir-tenant-a", Backend: "mimir", URL: dedicated.URL, TenantID: "tenant-a"},
		mimirInst("mimir-shared", shared.URL),
	})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newMimirTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/push", strings.NewReader("body"))
	req.Header.Set("Authorization", authHeader())
	req.Header.Set("Content-Type", "application/x-protobuf")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}
	if dedicatedCalls != 1 || sharedCalls != 0 {
		t.Fatalf("expected only dedicated instance to be used, dedicated=%d shared=%d", dedicatedCalls, sharedCalls)
	}
}

func TestMimirPushFallsBackToSharedInstance(t *testing.T) {
	withAuthTenants(t, "tenant-b")

	var dedicatedCalls, sharedCalls int
	dedicated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dedicatedCalls++
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(dedicated.Close)
	shared := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sharedCalls++
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(shared.Close)

	cfg := newTestConfig([]*config.InstanceConfig{
		{Name: "mimir-tenant-a", Backend: "mimir", URL: dedicated.URL, TenantID: "tenant-a"},
		mimirInst("mimir-shared", shared.URL),
	})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newMimirTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/push", strings.NewReader("body"))
	req.Header.Set("Authorization", authHeader())
	req.Header.Set("Content-Type", "application/x-protobuf")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}
	if dedicatedCalls != 0 || sharedCalls != 1 {
		t.Fatalf("expected only shared instance to be used, dedicated=%d shared=%d", dedicatedCalls, sharedCalls)
	}
}

func TestMimirPushRejectsAmbiguousUnscopedWrite(t *testing.T) {
	withAuthTenants(t, "tenant-a", "tenant-b")

	var called bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{mimirInst("mimir-prod", upstream.URL)})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newMimirTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/push", strings.NewReader("body"))
	req.Header.Set("Authorization", authHeader())
	req.Header.Set("Content-Type", "application/x-protobuf")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if called {
		t.Error("upstream should not be called for an ambiguous write tenant")
	}
	if !strings.Contains(rec.Body.String(), "ambiguous") {
		t.Errorf("expected ambiguity message, got %q", rec.Body.String())
	}
}

func TestMimirQueryRange(t *testing.T) {
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"success"}`)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{mimirInst("mimir-prod", upstream.URL)})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newMimirTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodGet, "/prometheus/api/v1/query_range?query=up&start=1&end=2&step=1", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if receivedPath != "/prometheus/api/v1/query_range" {
		t.Errorf("expected path /prometheus/api/v1/query_range, got %q", receivedPath)
	}
}

func TestMimirInstantQuery(t *testing.T) {
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{mimirInst("mimir-prod", upstream.URL)})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newMimirTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodGet, "/prometheus/api/v1/query?query=up", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if receivedPath != "/prometheus/api/v1/query" {
		t.Errorf("expected path /prometheus/api/v1/query, got %q", receivedPath)
	}
}

func TestMimirPrometheusInstantQuery(t *testing.T) {
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{mimirInst("mimir-prod", upstream.URL)})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newMimirTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodGet, "/prometheus/api/v1/query?query=up", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if receivedPath != "/prometheus/api/v1/query" {
		t.Errorf("expected path /prometheus/api/v1/query, got %q", receivedPath)
	}
}

func TestMimirLabels(t *testing.T) {
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{mimirInst("mimir-prod", upstream.URL)})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newMimirTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodGet, "/prometheus/api/v1/labels", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if receivedPath != "/prometheus/api/v1/labels" {
		t.Errorf("expected path /prometheus/api/v1/labels, got %q", receivedPath)
	}
}

func TestMimirLabelValues(t *testing.T) {
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{mimirInst("mimir-prod", upstream.URL)})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newMimirTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodGet, "/prometheus/api/v1/label/job/values", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if receivedPath != "/prometheus/api/v1/label/job/values" {
		t.Errorf("expected path /prometheus/api/v1/label/job/values, got %q", receivedPath)
	}
}

func TestMimirSeries(t *testing.T) {
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{mimirInst("mimir-prod", upstream.URL)})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newMimirTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodGet, "/prometheus/api/v1/series", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if receivedPath != "/prometheus/api/v1/series" {
		t.Errorf("expected path /prometheus/api/v1/series, got %q", receivedPath)
	}
}

func TestMimirMetadata(t *testing.T) {
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{mimirInst("mimir-prod", upstream.URL)})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newMimirTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodGet, "/prometheus/api/v1/metadata", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if receivedPath != "/prometheus/api/v1/metadata" {
		t.Errorf("expected path /prometheus/api/v1/metadata, got %q", receivedPath)
	}
}

func TestMimirPushPartialFailureHeader(t *testing.T) {
	upstreamOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstreamOK.Close)

	upstreamFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstreamFail.Close)

	inst := &config.InstanceConfig{
		Name:       "mimir-prod",
		Backend:    "mimir",
		FanOutMode: "any",
		PushURLs: []config.PushTarget{
			{URL: upstreamOK.URL},
			{URL: upstreamFail.URL},
		},
	}
	cfg := newTestConfig([]*config.InstanceConfig{inst})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newMimirTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/push", strings.NewReader("body"))
	req.Header.Set("Authorization", authHeader())
	req.Header.Set("Content-Type", "application/x-protobuf")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204 (partial success), got %d", rec.Code)
	}
	pfHeader := rec.Header().Get("X-Gateway-Partial-Failure")
	if pfHeader == "" {
		t.Error("expected X-Gateway-Partial-Failure header to be set")
	}
	if !strings.Contains(pfHeader, "status=500") {
		t.Errorf("expected partial failure header to mention status=500, got %q", pfHeader)
	}
}

func TestMimirPushNoMatchingInstance(t *testing.T) {
	cfg := newTestConfig([]*config.InstanceConfig{})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newMimirTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/push", strings.NewReader("body"))
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "no matching instance" {
		t.Errorf("expected error='no matching instance', got %q", body["error"])
	}
}
