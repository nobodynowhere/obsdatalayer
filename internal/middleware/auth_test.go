package middleware_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/auth/authtest"
	"obsdatalayer/internal/authlimit"
	"obsdatalayer/internal/metrics"
	"obsdatalayer/internal/middleware"
)

func newHandlerCalledFlag() (http.Handler, *bool) {
	called := false
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	return h, &called
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body["error"]
}

type decisionStub struct {
	*authtest.Stub
	Decision auth.AccessDecision
}

func (s *decisionStub) AccessDecision(_, _, _ string) auth.AccessDecision {
	return s.Decision
}

func captureLogs(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// ---- BasicAuth (data plane) -------------------------------------------------

func TestBasicAuthValidCreds(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	stub := authtest.New()
	h := middleware.BasicAuth(stub, nil, inner)

	req := httptest.NewRequest(http.MethodGet, "/loki/loki/api/v1/labels", nil)
	req.Header.Set("Authorization", stub.Header())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if !*called {
		t.Error("expected inner handler to be called")
	}
}

func TestBasicAuthWrongPassword(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	h := middleware.BasicAuth(authtest.New(), nil, inner)

	req := httptest.NewRequest(http.MethodGet, "/loki/loki/api/v1/labels", nil)
	req.Header.Set("Authorization", authtest.BasicHeader("testuser", "nope"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if *called {
		t.Error("expected inner handler NOT to be called")
	}
}

func TestBasicAuthMissingHeader(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	h := middleware.BasicAuth(authtest.New(), nil, inner)

	req := httptest.NewRequest(http.MethodGet, "/loki/loki/api/v1/labels", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if *called {
		t.Error("expected inner handler NOT to be called")
	}
	if got := decodeError(t, rec); got != "unauthorized" {
		t.Errorf("expected error='unauthorized', got %q", got)
	}
}

func TestBasicAuthForbiddenWhenNoGrant(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	stub := authtest.New()
	// Read-only: no loki:write grant.
	stub.Allow = map[string]bool{"loki:read": true}
	h := middleware.BasicAuth(stub, nil, inner)

	req := httptest.NewRequest(http.MethodPost, "/loki/api/v1/push", nil)
	req.Header.Set("Authorization", stub.Header())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
	if *called {
		t.Error("expected inner handler NOT to be called on 403")
	}
	if got := decodeError(t, rec); got != "forbidden" {
		t.Errorf("expected error='forbidden', got %q", got)
	}
}

func TestBasicAuthSkipsHealthz(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	h := middleware.BasicAuth(authtest.New(), nil, inner)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for /healthz without auth, got %d", rec.Code)
	}
	if !*called {
		t.Error("expected inner handler to be called for /healthz")
	}
}

func TestBasicAuthAttachesRequestAuth(t *testing.T) {
	var captured *auth.RequestAuth
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = auth.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	stub := authtest.New()
	h := middleware.BasicAuth(stub, nil, inner)

	req := httptest.NewRequest(http.MethodGet, "/loki/loki/api/v1/labels", nil)
	req.Header.Set("Authorization", stub.Header())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if captured == nil {
		t.Fatal("expected RequestAuth in context, got nil")
	}
	if captured.Username != "testuser" {
		t.Errorf("expected username 'testuser', got %q", captured.Username)
	}
	if !captured.IsRead {
		t.Error("expected IsRead=true for GET")
	}
	if len(captured.TenantIDs) != 1 || captured.TenantIDs[0] != "test-tenant" {
		t.Errorf("expected [test-tenant], got %v", captured.TenantIDs)
	}
}

func TestBasicAuthPostIsWrite(t *testing.T) {
	var captured *auth.RequestAuth
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = auth.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	stub := authtest.New()
	h := middleware.BasicAuth(stub, nil, inner)

	req := httptest.NewRequest(http.MethodPost, "/loki/api/v1/push", nil)
	req.Header.Set("Authorization", stub.Header())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if captured == nil {
		t.Fatal("expected RequestAuth in context")
	}
	if captured.IsRead {
		t.Error("expected IsRead=false for POST")
	}
}

func TestBasicAuthPrometheusPostQueryIsRead(t *testing.T) {
	for _, path := range []string{
		"/prometheus/api/v1/query",
		"/prometheus/api/v1/query",
		"/prometheus/api/v1/search/metric_names",
	} {
		t.Run(path, func(t *testing.T) {
			var captured *auth.RequestAuth
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured = auth.FromContext(r.Context())
				w.WriteHeader(http.StatusOK)
			})
			stub := authtest.New()
			stub.Allow = map[string]bool{"mimir:read": true}
			h := middleware.BasicAuth(stub, nil, inner)

			req := httptest.NewRequest(http.MethodPost, path, nil)
			req.Header.Set("Authorization", stub.Header())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", rec.Code)
			}
			if captured == nil {
				t.Fatal("expected RequestAuth in context")
			}
			if !captured.IsRead {
				t.Error("expected Prometheus POST query to be read")
			}
		})
	}
}

func TestBasicAuthRootPrometheusPathUsesMimirBackend(t *testing.T) {
	for _, path := range []string{
		"/prometheus/api/v1/status/buildinfo",
		"/prometheus/ready",
		"/ready",
	} {
		t.Run(path, func(t *testing.T) {
			inner, called := newHandlerCalledFlag()
			stub := authtest.New()
			stub.Allow = map[string]bool{"mimir:read": true}
			h := middleware.BasicAuth(stub, nil, inner)

			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", stub.Header())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", rec.Code)
			}
			if !*called {
				t.Fatal("expected inner handler to be called")
			}
		})
	}
}

func TestBasicAuthMimirRulesAndAlertsUseDiscreteActions(t *testing.T) {
	cases := []struct {
		method string
		path   string
		action string
		isRead bool
	}{
		{http.MethodGet, "/prometheus/api/v1/rules", auth.ActionRulesRead, true},
		{http.MethodGet, "/prometheus/config/v1/rules", auth.ActionRulesRead, true},
		{http.MethodPost, "/prometheus/config/v1/rules/team-a", auth.ActionRulesWrite, false},
		{http.MethodDelete, "/prometheus/config/v1/rules/team-a/group-a", auth.ActionRulesWrite, false},
		{http.MethodGet, "/prometheus/api/v1/alerts", auth.ActionAlertsRead, true},
		{http.MethodGet, "/alertmanager/api/v1/alerts", auth.ActionAlertsRead, true},
		{http.MethodPost, "/alertmanager/alertmanager/api/v2/silences", auth.ActionAlertsWrite, false},
		{http.MethodDelete, "/alertmanager/api/v1/alerts", auth.ActionAlertsWrite, false},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var captured *auth.RequestAuth
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured = auth.FromContext(r.Context())
				w.WriteHeader(http.StatusOK)
			})
			stub := authtest.New()
			stub.Allow = map[string]bool{"mimir:" + tc.action: true}
			h := middleware.BasicAuth(stub, nil, inner)

			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Authorization", stub.Header())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", rec.Code)
			}
			if captured == nil {
				t.Fatal("expected RequestAuth in context")
			}
			if captured.IsRead != tc.isRead {
				t.Fatalf("expected IsRead=%v, got %v", tc.isRead, captured.IsRead)
			}
		})
	}
}

func TestBasicAuthMimirMetricReadDoesNotAllowRulesOrAlerts(t *testing.T) {
	for _, path := range []string{
		"/prometheus/api/v1/rules",
		"/prometheus/api/v1/alerts",
	} {
		t.Run(path, func(t *testing.T) {
			inner, called := newHandlerCalledFlag()
			stub := authtest.New()
			stub.Allow = map[string]bool{"mimir:read": true}
			h := middleware.BasicAuth(stub, nil, inner)

			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", stub.Header())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d", rec.Code)
			}
			if *called {
				t.Fatal("expected inner handler not to be called")
			}
		})
	}
}

func TestBasicAuthLokiTailUsesTailAction(t *testing.T) {
	const path = "/loki/loki/api/v1/tail"

	inner, called := newHandlerCalledFlag()
	stub := authtest.New()
	stub.Allow = map[string]bool{"loki:" + auth.ActionTail: true}
	h := middleware.BasicAuth(stub, nil, inner)

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", stub.Header())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with loki:tail, got %d", rec.Code)
	}
	if !*called {
		t.Fatal("expected inner handler to be called")
	}

	inner2, called2 := newHandlerCalledFlag()
	denied := authtest.New()
	denied.Allow = map[string]bool{"loki:" + auth.ActionRead: true}
	rec2 := httptest.NewRecorder()
	middleware.BasicAuth(denied, nil, inner2).ServeHTTP(rec2, authedRequest(http.MethodGet, path, denied))
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("loki:read must not authorize tail; got %d", rec2.Code)
	}
	if *called2 {
		t.Fatal("handler must not run for a denied tail request")
	}
}

func TestBasicAuthLogsAmbiguousTenantDenial(t *testing.T) {
	logs := captureLogs(t, slog.LevelInfo)
	inner, called := newHandlerCalledFlag()
	stub := &decisionStub{
		Stub: authtest.New(),
		Decision: auth.AccessDecision{
			Allowed:     false,
			DenyReason:  auth.AccessDenyAmbiguousTenant,
			TenantCount: 2,
		},
	}
	h := middleware.BasicAuth(stub, nil, inner)

	req := httptest.NewRequest(http.MethodGet, "/loki/loki/api/v1/tail", nil)
	req.Header.Set("Authorization", stub.Header())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if *called {
		t.Fatal("handler must not run for a denied request")
	}
	got := logs.String()
	for _, want := range []string{
		`msg="data plane request denied"`,
		`reason=ambiguous_tenant`,
		`backend=loki`,
		`action=tail`,
		`path=/loki/loki/api/v1/tail`,
		`tenant_count=2`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected log to contain %s, got:\n%s", want, got)
		}
	}
}

func TestBasicAuthUnauthorizedHeaders(t *testing.T) {
	inner, _ := newHandlerCalledFlag()
	h := middleware.BasicAuth(authtest.New(), nil, inner)

	req := httptest.NewRequest(http.MethodGet, "/loki/loki/api/v1/labels", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("expected WWW-Authenticate header on 401")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
}

// ---- AdminAuth (admin plane) ------------------------------------------------

func TestAdminAuthAllowsAdmin(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	stub := authtest.NewAdmin()
	h := middleware.AdminAuth(stub, nil, inner)

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.Header.Set("Authorization", stub.Header())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if !*called {
		t.Error("expected inner handler to be called")
	}
}

func TestAdminAuthRejectsNonAdmin(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	stub := authtest.New() // valid credentials, no admin grant
	h := middleware.AdminAuth(stub, nil, inner)

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.Header.Set("Authorization", stub.Header())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for authenticated non-admin, got %d", rec.Code)
	}
	if *called {
		t.Error("expected inner handler NOT to be called")
	}
}

// Metrics and health on the admin listener must not be reachable anonymously:
// the fan-out metrics carry upstream backend URLs.
func TestAdminAuthProtectsMetricsAndHealthz(t *testing.T) {
	for _, path := range []string{"/metrics", "/healthz"} {
		t.Run(path, func(t *testing.T) {
			inner, called := newHandlerCalledFlag()
			h := middleware.AdminAuth(authtest.NewAdmin(), nil, inner)

			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("expected 401 for anonymous %s, got %d", path, rec.Code)
			}
			if *called {
				t.Errorf("expected handler NOT to be called for anonymous %s", path)
			}
		})
	}
}

func TestAdminAuthRejectsBadPassword(t *testing.T) {
	inner, _ := newHandlerCalledFlag()
	h := middleware.AdminAuth(authtest.NewAdmin(), nil, inner)

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.Header.Set("Authorization", authtest.BasicHeader("testuser", "wrong"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

// ---- UI bundle exemption ----------------------------------------------------

// The SPA shell and its assets are served anonymously: a browser cannot supply
// Basic auth for the initial document load, and the bundle carries no tenant
// data. Everything it displays still comes from authenticated endpoints.
func TestAdminAuthServesUIWithoutCredentials(t *testing.T) {
	for _, path := range []string{"/", "/ui/", "/ui/assets/index-abc123.js", "/ui/tenants"} {
		t.Run(path, func(t *testing.T) {
			inner, called := newHandlerCalledFlag()
			h := middleware.AdminAuth(authtest.NewAdmin(), nil, inner)

			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Errorf("expected 200 for anonymous %s, got %d", path, rec.Code)
			}
			if !*called {
				t.Errorf("expected the UI handler to be reached for %s", path)
			}
		})
	}
}

// The exemption must be scoped to the bundle and nothing else.
func TestAdminAuthExemptionDoesNotLeakToAPI(t *testing.T) {
	for _, path := range []string{"/api/tenants", "/api/users", "/api/roles", "/api/config", "/api/whoami", "/uiconfig"} {
		t.Run(path, func(t *testing.T) {
			inner, called := newHandlerCalledFlag()
			h := middleware.AdminAuth(authtest.NewAdmin(), nil, inner)

			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("expected 401 for anonymous %s, got %d", path, rec.Code)
			}
			if *called {
				t.Errorf("handler must not be reached anonymously for %s", path)
			}
		})
	}
}

func TestBasicAuthLokiRulesAndAlertsUseDiscreteActions(t *testing.T) {
	cases := []struct {
		method string
		path   string
		action string
		isRead bool
	}{
		// Ruler configuration API: /loki/api/v1/rules upstream.
		{http.MethodGet, "/loki/loki/api/v1/rules", auth.ActionRulesRead, true},
		{http.MethodGet, "/loki/loki/api/v1/rules/team-a", auth.ActionRulesRead, true},
		{http.MethodGet, "/loki/loki/api/v1/rules/team-a/group-a", auth.ActionRulesRead, true},
		{http.MethodPost, "/loki/loki/api/v1/rules/team-a", auth.ActionRulesWrite, false},
		{http.MethodDelete, "/loki/loki/api/v1/rules/team-a/group-a", auth.ActionRulesWrite, false},
		{http.MethodDelete, "/loki/loki/api/v1/rules/team-a", auth.ActionRulesWrite, false},
		// Prometheus-compatible listings.
		{http.MethodGet, "/loki/prometheus/api/v1/rules", auth.ActionRulesRead, true},
		{http.MethodGet, "/loki/prometheus/api/v1/alerts", auth.ActionAlertsRead, true},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var captured *auth.RequestAuth
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured = auth.FromContext(r.Context())
				w.WriteHeader(http.StatusOK)
			})
			stub := authtest.New()
			stub.Allow = map[string]bool{"loki:" + tc.action: true}
			h := middleware.BasicAuth(stub, nil, inner)

			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Authorization", stub.Header())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", rec.Code)
			}
			if captured == nil {
				t.Fatal("expected RequestAuth in context")
			}
			if captured.IsRead != tc.isRead {
				t.Fatalf("expected IsRead=%v, got %v", tc.isRead, captured.IsRead)
			}
		})
	}
}

// TestBasicAuthLokiDataGrantsDoNotReachRulesOrAlerts is the point of the whole
// change: before it, a plain loki write grant could create and delete alerting
// rule groups, and a plain read grant could enumerate them.
func TestBasicAuthLokiDataGrantsDoNotReachRulesOrAlerts(t *testing.T) {
	cases := []struct {
		method string
		path   string
		grant  string
	}{
		{http.MethodGet, "/loki/loki/api/v1/rules", "loki:read"},
		{http.MethodGet, "/loki/loki/api/v1/rules/team-a", "loki:read"},
		{http.MethodGet, "/loki/prometheus/api/v1/rules", "loki:read"},
		{http.MethodGet, "/loki/prometheus/api/v1/alerts", "loki:read"},
		{http.MethodPost, "/loki/loki/api/v1/rules/team-a", "loki:write"},
		{http.MethodDelete, "/loki/loki/api/v1/rules/team-a", "loki:write"},
		{http.MethodDelete, "/loki/loki/api/v1/rules/team-a/group-a", "loki:write"},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path+" with "+tc.grant, func(t *testing.T) {
			inner, called := newHandlerCalledFlag()
			stub := authtest.New()
			stub.Allow = map[string]bool{tc.grant: true}
			h := middleware.BasicAuth(stub, nil, inner)

			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Authorization", stub.Header())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d", rec.Code)
			}
			if *called {
				t.Fatal("handler must not run for a denied request")
			}
		})
	}
}

// TestBasicAuthLokiRulesReadDoesNotImplyRulesWrite keeps the read/write split
// meaningful in both directions.
func TestBasicAuthLokiRulesReadDoesNotImplyRulesWrite(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	stub := authtest.New()
	stub.Allow = map[string]bool{"loki:" + auth.ActionRulesRead: true}
	h := middleware.BasicAuth(stub, nil, inner)

	req := httptest.NewRequest(http.MethodPost, "/loki/loki/api/v1/rules/team-a", nil)
	req.Header.Set("Authorization", stub.Header())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if *called {
		t.Fatal("handler must not run for a denied request")
	}
}

// TestLokiRulesPathMatchingIsNotOverEager guards the prefix match: a route that
// merely starts with the same letters must not inherit rule permissions.
func TestLokiRulesPathMatchingIsNotOverEager(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	stub := authtest.New()
	stub.Allow = map[string]bool{"loki:read": true}
	h := middleware.BasicAuth(stub, nil, inner)

	// Not a rules path, so a plain read grant must still work.
	req := httptest.NewRequest(http.MethodGet, "/loki/loki/api/v1/rulesomething", nil)
	req.Header.Set("Authorization", stub.Header())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !*called {
		t.Fatal("expected inner handler to be called")
	}
}

// TestLokiNativePathsResolveToLokiBackend covers the paths a Grafana Loki data
// source actually requests when it is pointed at the gateway root.
func TestLokiNativePathsResolveToLokiBackend(t *testing.T) {
	for _, path := range []string{
		"/loki/loki/api/v1/query",
		"/loki/loki/api/v1/query_range",
		"/loki/loki/api/v1/labels",
		"/loki/loki/api/v1/label/app/values",
		"/loki/loki/api/v1/series",
		"/loki/loki/api/v1/index/stats",
		"/loki/loki/api/v1/index/volume",
		"/loki/loki/api/v1/index/volume_range",
		"/loki/loki/api/v1/patterns",
		"/loki/loki/api/v1/detected_fields",
		"/loki/loki/api/v1/detected_field/level/values",
		"/loki/loki/api/v1/format_query",
		"/loki/loki/api/v1/status/buildinfo",
	} {
		t.Run(path, func(t *testing.T) {
			inner, called := newHandlerCalledFlag()
			stub := authtest.New()
			stub.Allow = map[string]bool{"loki:read": true}
			h := middleware.BasicAuth(stub, nil, inner)

			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", stub.Header())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", rec.Code)
			}
			if !*called {
				t.Fatal("expected inner handler to be called")
			}
		})
	}
}

// TestLokiNativePathsAreNotMimir guards the split between the two root
// prefixes: /loki/ is Loki, /prometheus/ is Mimir, and neither may borrow the
// other's grants.
func TestLokiNativePathsAreNotMimir(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	stub := authtest.New()
	stub.Allow = map[string]bool{"mimir:read": true, "mimir:*": true}
	h := middleware.BasicAuth(stub, nil, inner)

	req := httptest.NewRequest(http.MethodGet, "/loki/loki/api/v1/query", nil)
	req.Header.Set("Authorization", stub.Header())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if *called {
		t.Fatal("handler must not run for a denied request")
	}
}

// TestLokiNativePushIsAWrite is the sharp edge of the read-POST classification:
// /loki/api/v1/push must never be mistaken for a form-encoded query.
func TestLokiNativePushIsAWrite(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	stub := authtest.New()
	stub.Allow = map[string]bool{"loki:read": true}
	h := middleware.BasicAuth(stub, nil, inner)

	req := httptest.NewRequest(http.MethodPost, "/loki/api/v1/push", nil)
	req.Header.Set("Authorization", stub.Header())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("a read grant must not authorize push; expected 403, got %d", rec.Code)
	}
	if *called {
		t.Fatal("handler must not run for a denied request")
	}
}

// TestLokiFormPostsAreReads covers the endpoints Loki accepts as form posts
// because a LogQL selector can outgrow a practical URL.
func TestLokiFormPostsAreReads(t *testing.T) {
	for _, path := range []string{
		"/loki/loki/api/v1/series",
		"/loki/loki/api/v1/detected_fields",
		"/loki/loki/api/v1/detected_field/level/values",
		"/loki/loki/api/v1/format_query",
		"/loki/loki/api/v1/series",
		"/loki/loki/api/v1/detected_fields",
		"/loki/loki/api/v1/detected_field/level/values",
		"/loki/loki/api/v1/format_query",
	} {
		t.Run(path, func(t *testing.T) {
			var captured *auth.RequestAuth
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured = auth.FromContext(r.Context())
				w.WriteHeader(http.StatusOK)
			})
			stub := authtest.New()
			stub.Allow = map[string]bool{"loki:read": true}
			h := middleware.BasicAuth(stub, nil, inner)

			req := httptest.NewRequest(http.MethodPost, path, nil)
			req.Header.Set("Authorization", stub.Header())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", rec.Code)
			}
			if captured == nil || !captured.IsRead {
				t.Fatal("expected the request to be classified as a read")
			}
		})
	}
}

// TestLokiNativeAndLegacyRulerUseControlActions covers both spellings Loki
// serves for its ruler configuration API, including the legacy /api/prom form
// that Grafana uses for data source-managed alert rules. /api/prom would
// otherwise parse as a backend literally named "prom".
func TestLokiNativeAndLegacyRulerUseControlActions(t *testing.T) {
	cases := []struct {
		method string
		path   string
		action string
	}{
		{http.MethodGet, "/loki/loki/api/v1/rules", auth.ActionRulesRead},
		{http.MethodGet, "/loki/loki/api/v1/rules/team-a", auth.ActionRulesRead},
		{http.MethodGet, "/loki/loki/api/v1/rules/team-a/group-a", auth.ActionRulesRead},
		{http.MethodPost, "/loki/loki/api/v1/rules/team-a", auth.ActionRulesWrite},
		{http.MethodDelete, "/loki/loki/api/v1/rules/team-a", auth.ActionRulesWrite},
		{http.MethodGet, "/loki/api/prom/rules", auth.ActionRulesRead},
		{http.MethodGet, "/loki/api/prom/rules/team-a", auth.ActionRulesRead},
		{http.MethodGet, "/loki/api/prom/rules/team-a/group-a", auth.ActionRulesRead},
		{http.MethodPost, "/loki/api/prom/rules/team-a", auth.ActionRulesWrite},
		{http.MethodDelete, "/loki/api/prom/rules/team-a", auth.ActionRulesWrite},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			// The exact action is required; a plain data grant must not do.
			inner, called := newHandlerCalledFlag()
			denied := authtest.New()
			denied.Allow = map[string]bool{"loki:read": true, "loki:write": true}
			middleware.BasicAuth(denied, nil, inner).ServeHTTP(
				httptest.NewRecorder(), authedRequest(tc.method, tc.path, denied))
			if *called {
				t.Fatal("a plain loki data grant must not reach the ruler API")
			}

			inner2, called2 := newHandlerCalledFlag()
			stub := authtest.New()
			stub.Allow = map[string]bool{"loki:" + tc.action: true}
			req := authedRequest(tc.method, tc.path, stub)
			rec := httptest.NewRecorder()
			middleware.BasicAuth(stub, nil, inner2).ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200 with %s, got %d", tc.action, rec.Code)
			}
			if !*called2 {
				t.Fatal("expected inner handler to be called")
			}
		})
	}
}

func authedRequest(method, path string, stub *authtest.Stub) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", stub.Header())
	return req
}

// TestIngestionPathsResolveToTheirBackend covers the ingestion namespace, whose
// paths are each upstream project's own and therefore carry no /api/{backend}/
// segment for the generic parse to read.
func TestIngestionPathsResolveToTheirBackend(t *testing.T) {
	cases := []struct{ path, backend string }{
		{"/api/v1/push", "mimir"},
		{"/api/v1/push/influx/write", "mimir"},
		{"/otlp/v1/metrics", "mimir"},
		{"/loki/api/v1/push", "loki"},
		{"/otlp/v1/logs", "loki"},
		{"/v1/traces", "tempo"},
		{"/api/traces", "tempo"},
		{"/api/v2/spans", "tempo"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			// The owning backend's write grant admits it.
			inner, called := newHandlerCalledFlag()
			stub := authtest.New()
			stub.Allow = map[string]bool{tc.backend + ":write": true}
			rec := httptest.NewRecorder()
			middleware.BasicAuth(stub, nil, inner).ServeHTTP(rec, authedRequest(http.MethodPost, tc.path, stub))
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200 with %s:write, got %d", tc.backend, rec.Code)
			}
			if !*called {
				t.Fatal("expected inner handler to be called")
			}

			// Another backend's write grant does not.
			other := "loki"
			if tc.backend == "loki" {
				other = "mimir"
			}
			inner2, called2 := newHandlerCalledFlag()
			stub2 := authtest.New()
			stub2.Allow = map[string]bool{other + ":write": true, other + ":*": true}
			rec2 := httptest.NewRecorder()
			middleware.BasicAuth(stub2, nil, inner2).ServeHTTP(rec2, authedRequest(http.MethodPost, tc.path, stub2))
			if rec2.Code != http.StatusForbidden {
				t.Fatalf("expected 403 with %s grant, got %d", other, rec2.Code)
			}
			if *called2 {
				t.Fatal("handler must not run for a denied request")
			}
		})
	}
}

// TestIngestionPathsAreWrites keeps ingestion out of the read classification --
// none of these may be satisfied by a read grant.
func TestIngestionPathsAreWrites(t *testing.T) {
	for _, tc := range []struct{ path, backend string }{
		{"/api/v1/push", "mimir"},
		{"/otlp/v1/metrics", "mimir"},
		{"/loki/api/v1/push", "loki"},
		{"/otlp/v1/logs", "loki"},
		{"/v1/traces", "tempo"},
		{"/api/traces", "tempo"},
		{"/api/v2/spans", "tempo"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			inner, called := newHandlerCalledFlag()
			stub := authtest.New()
			stub.Allow = map[string]bool{tc.backend + ":read": true}
			rec := httptest.NewRecorder()
			middleware.BasicAuth(stub, nil, inner).ServeHTTP(rec, authedRequest(http.MethodPost, tc.path, stub))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("a read grant must not authorize ingestion; got %d", rec.Code)
			}
			if *called {
				t.Fatal("handler must not run for a denied request")
			}
		})
	}
}

// TestAlertmanagerMountUsesAlertActions covers the Alertmanager data source
// mount: it resolves to Mimir, and GET is alerts:read while everything else is
// alerts:write. A plain mimir data grant must not reach any of it.
func TestAlertmanagerMountUsesAlertActions(t *testing.T) {
	cases := []struct {
		method string
		path   string
		action string
	}{
		{http.MethodGet, "/alertmanager/alertmanager/api/v2/silences", auth.ActionAlertsRead},
		{http.MethodPost, "/alertmanager/alertmanager/api/v2/silences", auth.ActionAlertsWrite},
		{http.MethodGet, "/alertmanager/alertmanager/api/v2/silence/sid", auth.ActionAlertsRead},
		{http.MethodDelete, "/alertmanager/alertmanager/api/v2/silence/sid", auth.ActionAlertsWrite},
		{http.MethodGet, "/alertmanager/alertmanager/api/v2/status", auth.ActionAlertsRead},
		{http.MethodGet, "/alertmanager/alertmanager/api/v2/alerts/groups", auth.ActionAlertsRead},
		{http.MethodGet, "/alertmanager/alertmanager/api/v2/alerts", auth.ActionAlertsRead},
		{http.MethodPost, "/alertmanager/alertmanager/api/v2/alerts", auth.ActionAlertsWrite},
		{http.MethodGet, "/alertmanager/api/v1/alerts", auth.ActionAlertsRead},
		{http.MethodPost, "/alertmanager/api/v1/alerts", auth.ActionAlertsWrite},
		{http.MethodDelete, "/alertmanager/api/v1/alerts", auth.ActionAlertsWrite},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			// The backend is Mimir, and the exact alert action is required.
			inner, called := newHandlerCalledFlag()
			stub := authtest.New()
			stub.Allow = map[string]bool{"mimir:" + tc.action: true}
			rec := httptest.NewRecorder()
			middleware.BasicAuth(stub, nil, inner).ServeHTTP(rec, authedRequest(tc.method, tc.path, stub))
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200 with mimir:%s, got %d", tc.action, rec.Code)
			}
			if !*called {
				t.Fatal("expected inner handler to be called")
			}

			// A plain data grant must not reach the Alertmanager.
			inner2, called2 := newHandlerCalledFlag()
			denied := authtest.New()
			denied.Allow = map[string]bool{"mimir:read": true, "mimir:write": true}
			rec2 := httptest.NewRecorder()
			middleware.BasicAuth(denied, nil, inner2).ServeHTTP(rec2, authedRequest(tc.method, tc.path, denied))
			if rec2.Code != http.StatusForbidden {
				t.Fatalf("expected 403 with a plain data grant, got %d", rec2.Code)
			}
			if *called2 {
				t.Fatal("handler must not run for a denied request")
			}

			// Loki has no Alertmanager; a Loki grant must never match.
			inner3, called3 := newHandlerCalledFlag()
			loki := authtest.New()
			loki.Allow = map[string]bool{"loki:" + tc.action: true, "loki:*": true}
			rec3 := httptest.NewRecorder()
			middleware.BasicAuth(loki, nil, inner3).ServeHTTP(rec3, authedRequest(tc.method, tc.path, loki))
			if rec3.Code != http.StatusForbidden {
				t.Fatalf("expected 403 with a loki grant, got %d", rec3.Code)
			}
			if *called3 {
				t.Fatal("handler must not run for a denied request")
			}
		})
	}
}

// TestLokiDeleteUsesDeleteActions covers the log deletion API: it carries its
// own actions so that an ordinary write grant -- which every log shipper needs
// -- cannot destroy what it shipped.
func TestLokiDeleteUsesDeleteActions(t *testing.T) {
	const path = "/loki/loki/api/v1/delete"

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		tc := struct{ method, action string }{method, auth.ActionDelete}
		t.Run(tc.method, func(t *testing.T) {
			inner, called := newHandlerCalledFlag()
			stub := authtest.New()
			stub.Allow = map[string]bool{"loki:" + tc.action: true}
			rec := httptest.NewRecorder()
			middleware.BasicAuth(stub, nil, inner).ServeHTTP(rec, authedRequest(tc.method, path, stub))
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200 with loki:%s, got %d", tc.action, rec.Code)
			}
			if !*called {
				t.Fatal("expected inner handler to be called")
			}

			// A full data grant must not reach deletion.
			inner2, called2 := newHandlerCalledFlag()
			denied := authtest.New()
			denied.Allow = map[string]bool{"loki:read": true, "loki:write": true}
			rec2 := httptest.NewRecorder()
			middleware.BasicAuth(denied, nil, inner2).ServeHTTP(rec2, authedRequest(tc.method, path, denied))
			if rec2.Code != http.StatusForbidden {
				t.Fatalf("a read+write grant must not authorize deletion; got %d", rec2.Code)
			}
			if *called2 {
				t.Fatal("handler must not run for a denied request")
			}
		})
	}
}

// ---- authentication throttling ---------------------------------------------

func newGuard(threshold int) (*middleware.AuthGuard, *metrics.Metrics) {
	m := metrics.New(prometheus.NewRegistry())
	return &middleware.AuthGuard{
		Limiter: authlimit.NewLimiter(authlimit.Config{
			Enabled:          true,
			FailureThreshold: threshold,
			FailureWindow:    time.Minute,
			BlockDuration:    time.Minute,
			MaxBlockDuration: 10 * time.Minute,
		}),
		Metrics: m,
	}, m
}

// badRequest is one wrong-password attempt from source.
func badRequest(h http.Handler, source string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/loki/loki/api/v1/labels", nil)
	req.RemoteAddr = source + ":50000"
	req.Header.Set("Authorization", authtest.BasicHeader("testuser", "nope"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The defect this closes: repeated wrong credentials from one source must stop
// reaching the password hash, rather than costing a full bcrypt every time.
func TestBasicAuthThrottlesRepeatedFailures(t *testing.T) {
	inner, _ := newHandlerCalledFlag()
	guard, m := newGuard(3)
	h := middleware.BasicAuth(authtest.New(), guard, inner)

	for i := 0; i < 3; i++ {
		if rec := badRequest(h, "10.0.0.1"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401, got %d", i, rec.Code)
		}
	}

	rec := badRequest(h, "10.0.0.1")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 once throttled, got %d", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" || got == "0" {
		t.Errorf("expected an actionable Retry-After, got %q", got)
	}
	if got := m.AuthRejectedValue("throttled"); got != 1 {
		t.Errorf("expected 1 throttled rejection recorded, got %d", got)
	}
	if got := m.AuthFailureValue(); got != 3 {
		t.Errorf("expected 3 credential checks to have run, got %d", got)
	}
}

// A throttled source must not be able to starve everyone else.
func TestBasicAuthThrottleIsPerSource(t *testing.T) {
	inner, _ := newHandlerCalledFlag()
	guard, _ := newGuard(2)
	h := middleware.BasicAuth(authtest.New(), guard, inner)

	badRequest(h, "10.0.0.1")
	badRequest(h, "10.0.0.1")
	if rec := badRequest(h, "10.0.0.1"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected the offending source to be throttled, got %d", rec.Code)
	}

	if rec := badRequest(h, "10.0.0.2"); rec.Code != http.StatusUnauthorized {
		t.Errorf("expected an unrelated source to still be served, got %d", rec.Code)
	}
}

// A valid credential clears the source, so a user who mistypes a password a few
// times and then gets it right is not left locked out.
func TestBasicAuthSuccessClearsThrottle(t *testing.T) {
	inner, _ := newHandlerCalledFlag()
	guard, _ := newGuard(3)
	stub := authtest.New()
	h := middleware.BasicAuth(stub, guard, inner)

	badRequest(h, "10.0.0.1")
	badRequest(h, "10.0.0.1")

	req := httptest.NewRequest(http.MethodGet, "/loki/loki/api/v1/labels", nil)
	req.RemoteAddr = "10.0.0.1:50000"
	req.Header.Set("Authorization", stub.Header())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected the valid credential to be accepted, got %d", rec.Code)
	}

	// Two more failures would have crossed the threshold had the success not
	// reset the count.
	badRequest(h, "10.0.0.1")
	if rec := badRequest(h, "10.0.0.1"); rec.Code != http.StatusUnauthorized {
		t.Errorf("expected the counter to have been cleared, got %d", rec.Code)
	}
}

// A request with no credentials at all costs no hashing, so it must not count
// toward the throttle -- otherwise a browser loading the UI could lock itself out.
func TestBasicAuthMissingCredentialsAreNotCounted(t *testing.T) {
	inner, _ := newHandlerCalledFlag()
	guard, m := newGuard(2)
	h := middleware.BasicAuth(authtest.New(), guard, inner)

	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodGet, "/loki/loki/api/v1/labels", nil)
		req.RemoteAddr = "10.0.0.1:50000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401, got %d", i, rec.Code)
		}
	}
	if got := m.AuthFailureValue(); got != 0 {
		t.Errorf("expected no credential checks to be counted, got %d", got)
	}
}

// Probes must stay reachable: throttling them would make an attacker able to
// fail a container's health check.
func TestBasicAuthThrottleSkipsProbes(t *testing.T) {
	inner, _ := newHandlerCalledFlag()
	guard, _ := newGuard(1)
	h := middleware.BasicAuth(authtest.New(), guard, inner)

	badRequest(h, "10.0.0.1")
	badRequest(h, "10.0.0.1")

	for _, path := range []string{"/healthz", "/ready"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "10.0.0.1:50000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: expected the probe to bypass throttling, got %d", path, rec.Code)
		}
	}
}

// Each plane carries its own counter. Sharing one was tried and had to be
// reversed: with a single counter, flooding the data listener blocked the
// operator out of the admin API, removing the only means of changing anything
// -- including turning the throttle off -- while under attack. Losing the
// recovery path is worse than the CPU exhaustion being defended against.
//
// The admin plane still throttles the same source on its own counter, and the
// process-wide hashing gate still bounds what the two planes cost together.
func TestThrottleIsPerPlane(t *testing.T) {
	dataGuard, _ := newGuard(2)
	adminGuard, _ := newGuard(2)
	dataInner, _ := newHandlerCalledFlag()
	adminInner, _ := newHandlerCalledFlag()
	stub := authtest.NewAdmin()

	data := middleware.BasicAuth(stub, dataGuard, dataInner)
	admin := middleware.AdminAuth(stub, adminGuard, adminInner)

	// Flood the data plane until it blocks.
	badRequest(data, "10.0.0.1")
	badRequest(data, "10.0.0.1")
	if rec := badRequest(data, "10.0.0.1"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected the data plane to be throttled, got %d", rec.Code)
	}

	// The operator, arriving from the same address, can still reach admin.
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.RemoteAddr = "10.0.0.1:50000"
	req.Header.Set("Authorization", stub.Header())
	rec := httptest.NewRecorder()
	admin.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("a data-plane flood locked the operator out of the admin plane (got %d)", rec.Code)
	}
}

// Separate counters must not mean the admin plane is unthrottled: an attacker
// who does reach it is still held off, just on its own allowance.
func TestAdminPlaneThrottlesIndependently(t *testing.T) {
	adminGuard, _ := newGuard(2)
	adminInner, _ := newHandlerCalledFlag()
	admin := middleware.AdminAuth(authtest.NewAdmin(), adminGuard, adminInner)

	badAdmin := func() int {
		req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
		req.RemoteAddr = "10.0.0.1:50000"
		req.Header.Set("Authorization", authtest.BasicHeader("testuser", "nope"))
		rec := httptest.NewRecorder()
		admin.ServeHTTP(rec, req)
		return rec.Code
	}

	badAdmin()
	badAdmin()
	if got := badAdmin(); got != http.StatusTooManyRequests {
		t.Errorf("expected the admin plane to throttle on its own counter, got %d", got)
	}
}

// saturatedAuthorizer stands in for a gateway already hashing at capacity.
type saturatedAuthorizer struct{ *authtest.Stub }

func (saturatedAuthorizer) AuthenticateContext(context.Context, string, string) (*auth.User, error) {
	return nil, auth.ErrHashLimitReached
}

// A caller shed for want of a hashing slot had its credential never checked.
// Answering 401 would tell a client with a perfectly good password that it was
// wrong; 503 with a Retry-After is the honest answer.
func TestBasicAuthSheds503WhenHashingIsSaturated(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	guard, m := newGuard(3)
	h := middleware.BasicAuth(saturatedAuthorizer{authtest.New()}, guard, inner)

	rec := badRequest(h, "10.0.0.1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("expected a Retry-After on a shed request")
	}
	if *called {
		t.Error("inner handler ran for an unauthenticated request")
	}
	if got := m.AuthRejectedValue("saturated"); got != 1 {
		t.Errorf("expected 1 saturated rejection recorded, got %d", got)
	}
	// Being shed is the gateway's fault, not the caller's, so it must not count
	// against the source: an outage would otherwise lock out every client.
	if got := m.AuthFailureValue(); got != 0 {
		t.Errorf("expected a shed request not to count as a credential failure, got %d", got)
	}
}

// A nil guard is what every test that is not exercising throttling passes, and
// what a gateway with the feature disabled runs with.
func TestNilGuardDisablesThrottling(t *testing.T) {
	inner, _ := newHandlerCalledFlag()
	h := middleware.BasicAuth(authtest.New(), nil, inner)

	for i := 0; i < 50; i++ {
		if rec := badRequest(h, "10.0.0.1"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401 with no guard, got %d", i, rec.Code)
		}
	}
}

// ---- bearer API keys --------------------------------------------------------

func bearerRequest(h http.Handler, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "10.0.0.9:50000"
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A key authenticates on the data plane exactly as its owner's password would.
func TestBearerKeyAuthenticatesOnDataPlane(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	stub := authtest.New()
	stub.APIKey = "obsgw_abc123_the-secret_with-underscores"
	h := middleware.BasicAuth(stub, nil, inner)

	rec := bearerRequest(h, "/loki/loki/api/v1/labels", stub.APIKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected the key to authenticate, got %d", rec.Code)
	}
	if !*called {
		t.Error("inner handler was not reached")
	}
}

func TestBearerKeyRejectedWhenWrong(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	stub := authtest.New()
	stub.APIKey = "obsgw_abc123_the-secret"
	h := middleware.BasicAuth(stub, nil, inner)

	rec := bearerRequest(h, "/loki/loki/api/v1/labels", "obsgw_abc123_wrong")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if *called {
		t.Error("inner handler ran for a bad key")
	}
}

// Keys are long-lived credentials issued for unattended shippers. The admin
// plane creates users and edits routing, so it keeps requiring a password --
// the same instinct as the wildcard grant that excludes the admin object.
func TestBearerKeyIsRejectedOnAdminPlane(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	stub := authtest.NewAdmin()
	stub.APIKey = "obsgw_abc123_the-secret"
	h := middleware.AdminAuth(stub, nil, inner)

	rec := bearerRequest(h, "/api/config", stub.APIKey)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected the admin plane to refuse a bearer key, got %d", rec.Code)
	}
	if *called {
		t.Error("a bearer key reached the admin API")
	}
}

// A rejected key counts against the source throttle: guessing tokens must be
// rate limited like guessing passwords.
func TestBearerKeyFailuresAreThrottled(t *testing.T) {
	inner, _ := newHandlerCalledFlag()
	guard, m := newGuard(2)
	stub := authtest.New()
	stub.APIKey = "obsgw_abc123_the-secret"
	h := middleware.BasicAuth(stub, guard, inner)

	bearerRequest(h, "/loki/loki/api/v1/labels", "obsgw_abc123_wrong")
	bearerRequest(h, "/loki/loki/api/v1/labels", "obsgw_abc123_wrong")
	rec := bearerRequest(h, "/loki/loki/api/v1/labels", "obsgw_abc123_wrong")

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected repeated bad keys to be throttled, got %d", rec.Code)
	}
	if got := m.AuthRejectedValue("throttled"); got != 1 {
		t.Errorf("throttled rejections = %d, want 1", got)
	}
}

// Basic auth must keep working alongside bearer: the two are alternatives, not
// a migration.
func TestBasicAuthStillWorksAlongsideBearer(t *testing.T) {
	inner, _ := newHandlerCalledFlag()
	stub := authtest.New()
	stub.APIKey = "obsgw_abc123_the-secret"
	h := middleware.BasicAuth(stub, nil, inner)

	req := httptest.NewRequest(http.MethodGet, "/loki/loki/api/v1/labels", nil)
	req.Header.Set("Authorization", stub.Header())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("basic auth stopped working: %d", rec.Code)
	}
}

// The admin plane must never challenge. A WWW-Authenticate header makes the
// browser answer the 401 itself with the native basic-auth dialog, whose cached
// credential then overrides what the SPA's login form sends.
func TestAdminAuthNeverChallenges(t *testing.T) {
	cases := []struct {
		name  string
		creds [2]string
		stub  auth.Authorizer
	}{
		{name: "no credentials", stub: authtest.NewAdmin()},
		{name: "bad credentials", creds: [2]string{"admin", "wrong"}, stub: authtest.NewAdmin()},
		{name: "no admin grant", creds: [2]string{"user", "pass"}, stub: authtest.New()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner, _ := newHandlerCalledFlag()
			h := middleware.AdminAuth(tc.stub, nil, inner)

			req := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
			if tc.creds[0] != "" {
				req.SetBasicAuth(tc.creds[0], tc.creds[1])
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code == http.StatusOK {
				t.Fatalf("expected the request to be refused, got 200")
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != "" {
				t.Errorf("admin plane sent a challenge: WWW-Authenticate: %q", got)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("expected Content-Type application/json, got %q", ct)
			}
		})
	}
}
