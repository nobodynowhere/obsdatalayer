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
	m := newTestMetrics()
	fanout.RegisterLoki(mux, h, p, m)
	fanout.RegisterMimir(mux, h, p, m)
	fanout.RegisterTempo(mux, h, p)
	return middleware.BasicAuth(testUF, mux)
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

	req := httptest.NewRequest(http.MethodPost, "/api/tempo-prod/tempo/otlp/v1/traces", strings.NewReader("trace data"))
	req.Header.Set("Authorization", authHeader())
	req.Header.Set("Content-Type", "application/x-protobuf")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if receivedPath != "/otlp/v1/traces" {
		t.Errorf("expected upstream path /otlp/v1/traces, got %q", receivedPath)
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

	req := httptest.NewRequest(http.MethodPost, "/api/tempo-prod/tempo/jaeger/v1/traces", strings.NewReader("trace data"))
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

	req := httptest.NewRequest(http.MethodGet, "/api/tempo-prod/tempo/search?q=xxx", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/api/tempo-prod/tempo/traces/abc123", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/api/tempo-prod/tempo/v2/search/tags", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/api/tempo-prod/tempo/v2/search/tag/service.name/values", nil)
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

func TestTempoUnknownInstance(t *testing.T) {
	cfg := newTestConfig([]*config.InstanceConfig{})
	client := &http.Client{Timeout: 5 * time.Second}
	p := proxy.New(client, client)
	h := newTempoTestMux(cfg, p)

	req := httptest.NewRequest(http.MethodPost, "/api/unknown/tempo/otlp/v1/traces", strings.NewReader("data"))
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

func TestTempoPushBodyStreamedVerbatim(t *testing.T) {
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
	req := httptest.NewRequest(http.MethodPost, "/api/tempo-prod/tempo/otlp/v1/traces", strings.NewReader(sendBody))
	req.Header.Set("Authorization", authHeader())
	req.Header.Set("Content-Type", "application/x-protobuf")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if receivedBody != sendBody {
		t.Errorf("expected body to be streamed verbatim, got %q", receivedBody)
	}
}
