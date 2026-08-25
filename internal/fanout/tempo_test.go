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

func tempoInst(name, url string) *config.InstanceConfig {
	return &config.InstanceConfig{
		Name:    name,
		Backend: "tempo",
		URL:     url,
	}
}

func newTempoTestMux(cfg *config.Config, p *proxy.Proxy) http.Handler {
	h := config.NewHolder(cfg, "")
	mux := http.NewServeMux()
	fanout.IngestRoutes(mux, h, p, newTestMetrics())
	fanout.LokiDSRoutes(mux, "/loki", h, p)
	fanout.MimirDSRoutes(mux, "/prometheus", h, p)
	fanout.TempoDSRoutes(mux, "/tempo", h, p)
	return middleware.BasicAuth(testAuth, nil, mux)
}

func TestTempoOTLPPush(t *testing.T) {
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{tempoInst("tempo-prod", upstream.URL)})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTempoTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader("trace data"))
	req.Header.Set("Authorization", authHeader())
	req.Header.Set("Content-Type", "application/x-protobuf")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	// Tempo's OTLP HTTP receiver is a bare OTel receiver at /v1/traces; it does
	// not namespace OTLP under /otlp the way Mimir and Loki do.
	if receivedPath != "/v1/traces" {
		t.Errorf("expected upstream path /v1/traces, got %q", receivedPath)
	}
}

func TestTempoJaegerPush(t *testing.T) {
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{tempoInst("tempo-prod", upstream.URL)})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTempoTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodPost, "/api/traces", strings.NewReader("trace data"))
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if receivedPath != "/api/traces" {
		t.Errorf("expected upstream path /api/traces, got %q", receivedPath)
	}
}

func TestTempoSearch(t *testing.T) {
	var receivedPath, receivedQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{tempoInst("tempo-prod", upstream.URL)})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTempoTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodGet, "/tempo/api/search?q=xxx", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if receivedPath != "/api/search" {
		t.Errorf("expected path /api/search, got %q", receivedPath)
	}
	if !strings.Contains(receivedQuery, "q=xxx") {
		t.Errorf("expected query param forwarded, got %q", receivedQuery)
	}
}

func TestTempoGetTrace(t *testing.T) {
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{tempoInst("tempo-prod", upstream.URL)})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTempoTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodGet, "/tempo/api/traces/abc123", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if receivedPath != "/api/traces/abc123" {
		t.Errorf("expected path /api/traces/abc123, got %q", receivedPath)
	}
}

func TestTempoSearchTagsV2(t *testing.T) {
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{tempoInst("tempo-prod", upstream.URL)})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTempoTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodGet, "/tempo/api/v2/search/tags", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if receivedPath != "/api/v2/search/tags" {
		t.Errorf("expected path /api/v2/search/tags, got %q", receivedPath)
	}
}

func TestTempoSearchTagValuesV2(t *testing.T) {
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{tempoInst("tempo-prod", upstream.URL)})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTempoTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodGet, "/tempo/api/v2/search/tag/service.name/values", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if receivedPath != "/api/v2/search/tag/service.name/values" {
		t.Errorf("expected path /api/v2/search/tag/service.name/values, got %q", receivedPath)
	}
}

func TestTempoNoMatchingInstance(t *testing.T) {
	cfg := newTestConfig([]*config.InstanceConfig{})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTempoTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader("data"))
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

func TestTempoPushBodyForwardedVerbatim(t *testing.T) {
	var receivedBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		receivedBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{tempoInst("tempo-prod", upstream.URL)})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTempoTestMux(cfg, p)

	sendBody := "binary trace data \x00\x01\x02"
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader(sendBody))
	req.Header.Set("Authorization", authHeader())
	req.Header.Set("Content-Type", "application/x-protobuf")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if receivedBody != sendBody {
		t.Errorf("expected body to be forwarded verbatim, got %q", receivedBody)
	}
}

func TestTempoPushFansOutToAllTargets(t *testing.T) {
	received := make(chan string, 2)
	newUpstream := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			received <- name + ":" + string(b)
			w.WriteHeader(http.StatusOK)
		}))
	}
	a := newUpstream("a")
	b := newUpstream("b")
	t.Cleanup(a.Close)
	t.Cleanup(b.Close)

	cfg := newTestConfig([]*config.InstanceConfig{{
		Name:       "tempo-prod",
		Backend:    "tempo",
		FanOutMode: "all",
		PushURLs: []config.PushTarget{
			{URL: a.URL},
			{URL: b.URL},
		},
	}})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTempoTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader("trace data"))
	req.Header.Set("Authorization", authHeader())
	req.Header.Set("Content-Type", "application/x-protobuf")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case value := <-received:
			got[value] = true
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for Tempo fan-out target %d", i+1)
		}
	}
	if !got["a:trace data"] || !got["b:trace data"] {
		t.Errorf("expected both Tempo targets to receive the trace body, got %v", got)
	}
}

// TestHealthChecksIgnoreTenantBinding covers the failure that sent Grafana's
// data source test to "Tempo echo endpoint returned status 404".
//
// These endpoints answer for the process, not for a tenant, but they used to be
// routed by getInstance, which picks the instance holding a tenant's data. On
// any deployment where the Tempo instance is bound to a tenant the calling
// grant does not name, that resolved to nothing and the probe 404'd -- and with
// two bound instances, or a grant naming two tenants, it 409'd instead. Neither
// answer had anything to do with whether Tempo was up.
//
// Each case here returned 404 or 409 before getHealthInstance; all of them must
// now reach the backend and return its answer.
func TestHealthChecksIgnoreTenantBinding(t *testing.T) {
	var gotOrgID string
	var reached bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		gotOrgID = r.Header.Get("X-Scope-OrgID")
		w.Write([]byte("echo"))
	}))
	t.Cleanup(upstream.Close)

	bound := func(name, tenant string) *config.InstanceConfig {
		inst := tempoInst(name, upstream.URL)
		inst.TenantID = tenant
		return inst
	}

	cases := []struct {
		name      string
		instances []*config.InstanceConfig
		tenants   []string
	}{
		{"bound instance, grant names another tenant", []*config.InstanceConfig{bound("tempo-acme", "acme")}, []string{"beta"}},
		{"bound instance, grant names two tenants", []*config.InstanceConfig{bound("tempo-acme", "acme")}, []string{"acme", "beta"}},
		{"two bound instances, both granted", []*config.InstanceConfig{bound("tempo-acme", "acme"), bound("tempo-beta", "beta")}, []string{"acme", "beta"}},
		{"shared instance", []*config.InstanceConfig{tempoInst("tempo-prod", upstream.URL)}, []string{"acme"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached, gotOrgID = false, ""
			previous := append([]string(nil), testAuth.Tenants...)
			testAuth.Tenants = append([]string(nil), tc.tenants...)
			t.Cleanup(func() { testAuth.Tenants = previous })

			client := &http.Client{Timeout: 5 * time.Second}
			p := proxy.New(client, client)
			h := newTempoTestMux(newTestConfig(tc.instances), p)

			req := httptest.NewRequest(http.MethodGet, "/tempo/api/echo", nil)
			req.Header.Set("Authorization", authHeader())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
			}
			if !reached {
				t.Fatal("the probe never reached the backend")
			}
			if body := strings.TrimSpace(rec.Body.String()); body != "echo" {
				t.Errorf("expected the backend's answer %q, got %q", "echo", body)
			}
			// The instance carries a tenant ID, and on a query that would be
			// injected. A health check must assert no tenant whatever the
			// instance is bound to; see ForwardHealth.
			if gotOrgID != "" {
				t.Errorf("health check carried X-Scope-OrgID %q; it must carry none", gotOrgID)
			}
		})
	}
}

// TestHealthCheckWithoutBackendIs404 keeps the one 404 that is a real answer.
// Dropping the tenant from instance selection must not turn "no Tempo is
// configured at all" into a success, or the data source test would pass against
// a gateway with nothing behind it.
func TestHealthCheckWithoutBackendIs404(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTempoTestMux(newTestConfig([]*config.InstanceConfig{lokiInst("loki-prod", "http://upstream.local")}), p)

	req := httptest.NewRequest(http.MethodGet, "/tempo/api/echo", nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}
