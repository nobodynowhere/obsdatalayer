package proxy_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/proxy"
)

func newProxy(queryClient *http.Client) *proxy.Proxy {
	return proxy.New(queryClient, queryClient)
}

func newInst(name, backend, url string) *config.InstanceConfig {
	return &config.InstanceConfig{
		Name:    name,
		Backend: backend,
		URL:     url,
	}
}

// echoHeadersHandler returns an HTTP handler that echoes all request headers as response headers
// prefixed with "X-Echo-" and writes the request path as the body.
func echoHeadersHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, vals := range r.Header {
			for _, v := range vals {
				w.Header().Add("X-Echo-"+k, v)
			}
		}
		w.Header().Set("X-Echo-Path", r.URL.Path)
		w.Header().Set("X-Echo-Query", r.URL.RawQuery)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, r.URL.Path)
	})
}

func TestForwardQueryBasicSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "upstream response")
	}))
	t.Cleanup(upstream.Close)

	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	inst := newInst("loki-prod", "loki", upstream.URL)

	req := httptest.NewRequest(http.MethodGet, "/api/loki/labels", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()

	p.ForwardQuery(rec, req, inst, "/loki/api/v1/labels")

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "upstream response" {
		t.Errorf("expected 'upstream response', got %q", rec.Body.String())
	}
}

func TestForwardQueryResponseHeadersCopied(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream-Header", "from-upstream")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	inst := newInst("loki-prod", "loki", upstream.URL)

	req := httptest.NewRequest(http.MethodGet, "/api/loki/labels", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()

	p.ForwardQuery(rec, req, inst, "/loki/api/v1/labels")

	if rec.Header().Get("X-Upstream-Header") != "from-upstream" {
		t.Errorf("expected upstream header to be copied, got %q", rec.Header().Get("X-Upstream-Header"))
	}
}

func TestForwardQueryQueryParamsForwarded(t *testing.T) {
	upstream := httptest.NewServer(echoHeadersHandler())
	t.Cleanup(upstream.Close)

	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	inst := newInst("loki-prod", "loki", upstream.URL)

	req := httptest.NewRequest(http.MethodGet, "/api/loki/query_range?query=xxx&start=1&end=2", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()

	p.ForwardQuery(rec, req, inst, "/loki/api/v1/query_range")

	echoQuery := rec.Header().Get("X-Echo-Query")
	if echoQuery != "query=xxx&start=1&end=2" {
		t.Errorf("expected query params forwarded, got %q", echoQuery)
	}
}

func TestForwardQueryAuthHeaderNotSentWithoutPassthrough(t *testing.T) {
	upstream := httptest.NewServer(echoHeadersHandler())
	t.Cleanup(upstream.Close)

	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	inst := newInst("loki-prod", "loki", upstream.URL)

	req := httptest.NewRequest(http.MethodGet, "/api/loki/labels", nil)
	req.Header.Set("Authorization", "Bearer client-token")
	rec := httptest.NewRecorder()

	p.ForwardQuery(rec, req, inst, "/loki/api/v1/labels")

	// Without auth passthrough, Authorization from client should NOT be forwarded
	echoAuth := rec.Header().Get("X-Echo-Authorization")
	if echoAuth != "" {
		t.Errorf("expected Authorization NOT forwarded upstream, got %q", echoAuth)
	}
}

func TestForwardQueryBasicAuthInjected(t *testing.T) {
	upstream := httptest.NewServer(echoHeadersHandler())
	t.Cleanup(upstream.Close)

	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	inst := &config.InstanceConfig{
		Name:      "loki-prod",
		Backend:   "loki",
		URL:       upstream.URL,
		BasicAuth: "user:pass",
	}

	req := httptest.NewRequest(http.MethodGet, "/api/loki/labels", nil)
	req.Header.Set("Authorization", "Bearer client-token")
	rec := httptest.NewRecorder()

	p.ForwardQuery(rec, req, inst, "/loki/api/v1/labels")

	echoAuth := rec.Header().Get("X-Echo-Authorization")
	expected := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:pass"))
	if echoAuth != expected {
		t.Errorf("expected basic auth injected, got %q, expected %q", echoAuth, expected)
	}
}

func TestForwardQueryTenantIDInjected(t *testing.T) {
	upstream := httptest.NewServer(echoHeadersHandler())
	t.Cleanup(upstream.Close)

	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	inst := &config.InstanceConfig{
		Name:     "loki-prod",
		Backend:  "loki",
		URL:      upstream.URL,
		TenantID: "my-tenant",
	}

	req := httptest.NewRequest(http.MethodGet, "/api/loki/labels", nil)
	req.Header.Set("Authorization", "Bearer client-token")
	rec := httptest.NewRecorder()

	p.ForwardQuery(rec, req, inst, "/loki/api/v1/labels")

	echoOrgID := rec.Header().Get("X-Echo-X-Scope-Orgid")
	if echoOrgID != "my-tenant" {
		t.Errorf("expected X-Scope-OrgID='my-tenant', got %q", echoOrgID)
	}
}

func TestForwardQueryNoAuthWithoutPassthroughOrConfig(t *testing.T) {
	upstream := httptest.NewServer(echoHeadersHandler())
	t.Cleanup(upstream.Close)

	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	inst := newInst("loki-prod", "loki", upstream.URL)

	req := httptest.NewRequest(http.MethodGet, "/api/loki/labels", nil)
	req.Header.Set("Authorization", "Bearer client-token")
	rec := httptest.NewRecorder()

	p.ForwardQuery(rec, req, inst, "/loki/api/v1/labels")

	echoAuth := rec.Header().Get("X-Echo-Authorization")
	echoOrgID := rec.Header().Get("X-Echo-X-Scope-Orgid")
	if echoAuth != "" {
		t.Errorf("expected no Authorization sent, got %q", echoAuth)
	}
	if echoOrgID != "" {
		t.Errorf("expected no X-Scope-OrgID sent, got %q", echoOrgID)
	}
}

func TestForwardQueryUpstream404(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(upstream.Close)

	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	inst := newInst("loki-prod", "loki", upstream.URL)

	req := httptest.NewRequest(http.MethodGet, "/api/loki/labels", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()

	p.ForwardQuery(rec, req, inst, "/loki/api/v1/labels")

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestForwardQueryUpstream204(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	inst := newInst("loki-prod", "loki", upstream.URL)

	req := httptest.NewRequest(http.MethodGet, "/api/loki/labels", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()

	p.ForwardQuery(rec, req, inst, "/loki/api/v1/labels")

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", rec.Code)
	}
}

func TestForwardQueryUpstreamUnreachable(t *testing.T) {
	// Use a port that nothing is listening on
	p := newProxy(&http.Client{Timeout: 2 * time.Second})
	inst := newInst("loki-prod", "loki", "http://127.0.0.1:1") // port 1 should be unreachable

	req := httptest.NewRequest(http.MethodGet, "/api/loki/labels", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()

	p.ForwardQuery(rec, req, inst, "/loki/api/v1/labels")

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", rec.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body["error"] == "" {
		t.Error("expected error in JSON body")
	}
}

func TestForwardQueryUpstreamTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate slow response - sleep longer than client timeout
		select {
		case <-r.Context().Done():
			return
		case <-make(chan struct{}): // blocks forever
		}
	}))
	t.Cleanup(upstream.Close)

	// Very short timeout client
	p := newProxy(&http.Client{Timeout: 50 * time.Millisecond})
	inst := newInst("loki-prod", "loki", upstream.URL)

	req := httptest.NewRequest(http.MethodGet, "/api/loki/labels", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()

	p.ForwardQuery(rec, req, inst, "/loki/api/v1/labels")

	if rec.Code != http.StatusGatewayTimeout && rec.Code != http.StatusBadGateway {
		t.Errorf("expected 504 or 502, got %d", rec.Code)
	}
}

func TestForwardQueryOrgIDStrippedWithoutPassthrough(t *testing.T) {
	upstream := httptest.NewServer(echoHeadersHandler())
	t.Cleanup(upstream.Close)

	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	inst := newInst("loki-prod", "loki", upstream.URL)

	req := httptest.NewRequest(http.MethodGet, "/api/loki/labels", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Scope-OrgID", "client-tenant")
	rec := httptest.NewRecorder()

	p.ForwardQuery(rec, req, inst, "/loki/api/v1/labels")

	// Without passthrough, X-Scope-OrgID from client should be stripped
	echoOrgID := rec.Header().Get("X-Echo-X-Scope-Orgid")
	if echoOrgID == "client-tenant" {
		t.Error("expected X-Scope-OrgID to be stripped without passthrough")
	}
}

// errorRoundTripper returns a fixed error for every request.
type errorRoundTripper struct{ err error }

func (e *errorRoundTripper) RoundTrip(*http.Request) (*http.Response, error) { return nil, e.err }

// TestForwardQueryWrappedTimeoutError verifies that a *url.Error wrapping
// context.DeadlineExceeded is detected as a timeout and produces a 504.
// This exercises the errors.As path in isTimeoutError.
func TestForwardQueryWrappedTimeoutError(t *testing.T) {
	wrapped := &url.Error{Op: "Get", URL: "http://x", Err: context.DeadlineExceeded}
	client := &http.Client{Transport: &errorRoundTripper{err: wrapped}}
	p := proxy.New(client, client)
	inst := newInst("loki-prod", "loki", "http://x")

	req := httptest.NewRequest(http.MethodGet, "/api/loki/labels", nil)
	rec := httptest.NewRecorder()

	p.ForwardQuery(rec, req, inst, "/loki/api/v1/labels")

	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("expected 504 for wrapped timeout error, got %d", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "upstream timeout" {
		t.Errorf("expected error='upstream timeout', got %q", body["error"])
	}
}
