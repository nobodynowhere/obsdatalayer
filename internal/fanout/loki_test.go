package fanout_test

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"obsdatalayer/internal/auth"
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
	fanout.IngestRoutes(mux, h, p, newTestMetrics())
	fanout.LokiDSRoutes(mux, "/loki", h, p)
	fanout.MimirDSRoutes(mux, "/prometheus", h, p)
	fanout.TempoDSRoutes(mux, "/tempo", h, p)
	return middleware.BasicAuth(testAuth, nil, mux)
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
	req := httptest.NewRequest(http.MethodPost, "/loki/api/v1/push", strings.NewReader(body))
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
	req := httptest.NewRequest(http.MethodPost, "/loki/api/v1/push", strings.NewReader(body))
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

func TestLokiReadLabelSelectorRewritesQuery(t *testing.T) {
	withAuthSelectors(t, `{cluster="prod"}`)

	var receivedQuery string
	var receivedOrgID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedQuery = r.URL.Query().Get("query")
		receivedOrgID = r.Header.Get("X-Scope-OrgID")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"success"}`)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{lokiInst("loki-prod", upstream.URL)})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTestMux(cfg, p, newTestMetrics())

	req := httptest.NewRequest(http.MethodGet, `/loki/loki/api/v1/query?query=%7Bjob%3D%22api%22%7D%7C%3D%22error%22`, nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(receivedQuery, `{cluster="prod",job="api"}`) {
		t.Fatalf("expected query to be constrained, got %q", receivedQuery)
	}
	if receivedOrgID != "test-tenant" {
		t.Fatalf("expected tenant header to still be injected, got %q", receivedOrgID)
	}
}

func TestLokiRestrictedQueryWithoutSelectorIsRejected(t *testing.T) {
	withAuthSelectors(t, `{cluster="prod"}`)

	var called bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{lokiInst("loki-prod", upstream.URL)})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTestMux(cfg, p, newTestMetrics())

	req := httptest.NewRequest(http.MethodGet, `/loki/loki/api/v1/query?query=1%2B1`, nil)
	req.Header.Set("Authorization", authHeader())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if called {
		t.Fatal("restricted query without an enforceable selector reached upstream")
	}
}

func TestLokiPushNoMatchingInstance(t *testing.T) {
	cfg := newTestConfig([]*config.InstanceConfig{})
	m := newTestMetrics()
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTestMux(cfg, p, m)

	req := httptest.NewRequest(http.MethodPost, "/loki/api/v1/push", strings.NewReader("{}"))
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
	if body["error"] != "no matching instance" {
		t.Errorf("expected error='no matching instance', got %q", body["error"])
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

	req := httptest.NewRequest(http.MethodGet, "/loki/loki/api/v1/query_range?query=xxx&start=1&end=2&step=1", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/loki/loki/api/v1/query?query=xxx", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/loki/loki/api/v1/labels", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/loki/loki/api/v1/label/app/values", nil)
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

	req := httptest.NewRequest(http.MethodGet, `/loki/loki/api/v1/series?match[]=xxx`, nil)
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

	req := httptest.NewRequest(http.MethodGet, "/loki/loki/api/v1/labels", nil)
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
	req := httptest.NewRequest(http.MethodPost, "/loki/api/v1/push", strings.NewReader(body))
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

// TestLokiRulerListingsRefuseMultiTenant pins Loki's own rule: only query
// endpoints honour a pipe-joined X-Scope-OrgID. The Prometheus-compatible
// listings are ruler endpoints, so a grant spanning several tenants must be
// refused here rather than forwarded as a header Loki will not honour.
func TestLokiRulerListingsRefuseMultiTenant(t *testing.T) {
	for _, path := range []string{
		"/loki/prometheus/api/v1/rules",
		"/loki/prometheus/api/v1/alerts",
	} {
		t.Run(path, func(t *testing.T) {
			capture := &captureTransport{}
			cfg := newTestConfig([]*config.InstanceConfig{{
				Name: "loki-prod", Backend: "loki", URL: "http://loki.local",
			}})
			h := config.NewHolder(cfg, "")
			client := &http.Client{Timeout: 5 * time.Second, Transport: capture}
			p := proxy.New(client, client)
			mux := http.NewServeMux()
			fanout.IngestRoutes(mux, h, p, newTestMetrics())
			fanout.LokiDSRoutes(mux, "/loki", h, p)

			// Two tenants resolved: ambiguous for a ruler endpoint.
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req = req.WithContext(auth.WithRequestAuth(req.Context(), &auth.RequestAuth{
				Username:  "alice",
				TenantIDs: []string{"tenant-a", "tenant-b"},
				IsRead:    true,
			}))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403 for a multi-tenant ruler read, got %d", rec.Code)
			}
			if capture.path != "" {
				t.Fatalf("nothing should have been forwarded upstream, got %q", capture.path)
			}

			// One tenant resolved: forwarded normally.
			req2 := httptest.NewRequest(http.MethodGet, path, nil)
			req2 = req2.WithContext(auth.WithRequestAuth(req2.Context(), &auth.RequestAuth{
				Username:  "alice",
				TenantIDs: []string{"tenant-a"},
				IsRead:    true,
			}))
			rec2 := httptest.NewRecorder()
			mux.ServeHTTP(rec2, req2)

			if rec2.Code == http.StatusForbidden {
				t.Fatal("a single-tenant ruler read must be allowed")
			}
			if got := capture.orgID(); got != "tenant-a" {
				t.Fatalf("expected X-Scope-OrgID tenant-a, got %q", got)
			}
		})
	}
}

// TestLokiTailUpgradesAndStreams drives a real WebSocket handshake through the
// gateway: a 101 from the upstream, then bytes in both directions over the
// hijacked connection. A buffering proxy cannot do this, which is why tail has
// its own forwarding path.
func TestLokiTailUpgradesAndStreams(t *testing.T) {
	withAuthTenants(t, "tenant-a")
	var gotPath, gotOrg, gotKey, gotUpgrade, gotXFF string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotOrg = r.Header.Get("X-Scope-OrgID")
		gotKey = r.Header.Get("Sec-WebSocket-Key")
		gotUpgrade = r.Header.Get("Upgrade")
		gotXFF = r.Header.Get("X-Forwarded-For")

		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("upstream ResponseWriter is not a Hijacker")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_, _ = buf.WriteString("tailed-line\n")
		_ = buf.Flush()
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{{
		Name: "loki-prod", Backend: "loki", URL: upstream.URL,
	}})
	h := config.NewHolder(cfg, "")
	client := &http.Client{Timeout: 5 * time.Second, Transport: proxy.NewTransport()}
	mux := http.NewServeMux()
	fanout.LokiDSRoutes(mux, "/loki", h, proxy.New(client, client))
	// Logging wraps the ResponseWriter, so it must be in the chain here: the
	// production handler includes it, and a wrapper that is not an
	// http.Hijacker silently breaks the upgrade.
	gw := httptest.NewServer(middleware.Logging(middleware.BasicAuth(testAuth, nil, middleware.SanitizeHeaders(mux))))
	t.Cleanup(gw.Close)

	// Speak the handshake over a raw connection; http.Client cannot express one.
	conn, err := net.Dial("tcp", strings.TrimPrefix(gw.URL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	req := "GET /loki/loki/api/v1/tail?query=%7Ba%3D%22b%22%7D HTTP/1.1\r\n" +
		"Host: " + strings.TrimPrefix(gw.URL, "http://") + "\r\n" +
		"Authorization: " + authHeader() + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"X-Scope-OrgID: spoofed\r\n" +
		"\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	res := string(got)

	if !strings.Contains(res, "101 Switching Protocols") {
		t.Fatalf("expected a 101 upgrade, got:\n%s", res)
	}
	if !strings.Contains(res, "tailed-line") {
		t.Errorf("expected streamed data after the upgrade, got:\n%s", res)
	}
	if gotPath != "/loki/api/v1/tail" {
		t.Errorf("expected upstream path /loki/api/v1/tail, got %q", gotPath)
	}
	// The handshake headers survive; the spoofed tenant and the client address
	// do not.
	if gotUpgrade != "websocket" || gotKey != "dGhlIHNhbXBsZSBub25jZQ==" {
		t.Errorf("handshake headers did not survive: upgrade=%q key=%q", gotUpgrade, gotKey)
	}
	if gotOrg != "tenant-a" {
		t.Errorf("expected injected tenant tenant-a, got %q", gotOrg)
	}
	if gotXFF != "" {
		t.Errorf("client address leaked upstream as X-Forwarded-For %q", gotXFF)
	}
}
