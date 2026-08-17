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
	m := newTestMetrics()
	fanout.RegisterLoki(mux, h, p, m)
	fanout.RegisterMimir(mux, h, p, m)
	fanout.RegisterTempo(mux, h, p)
	return middleware.BasicAuth(testUF, mux)
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

	req := httptest.NewRequest(http.MethodPost, "/api/mimir-prod/mimir/push", strings.NewReader("body"))
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

	req := httptest.NewRequest(http.MethodGet, "/api/mimir-prod/mimir/query_range?query=up&start=1&end=2&step=1", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/api/mimir-prod/mimir/query?query=up", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/api/mimir-prod/mimir/labels", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/api/mimir-prod/mimir/label/job/values", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/api/mimir-prod/mimir/series", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/api/mimir-prod/mimir/metadata", nil)
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

	req := httptest.NewRequest(http.MethodPost, "/api/mimir-prod/mimir/push", strings.NewReader("body"))
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

func TestMimirPushUnknownInstance(t *testing.T) {
	cfg := newTestConfig([]*config.InstanceConfig{})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newMimirTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodPost, "/api/unknown/mimir/push", strings.NewReader("body"))
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
	if body["error"] != "unknown instance" {
		t.Errorf("expected error='unknown instance', got %q", body["error"])
	}
}
