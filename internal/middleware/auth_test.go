package middleware_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/auth/authtest"
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

// ---- BasicAuth (data plane) -------------------------------------------------

func TestBasicAuthValidCreds(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	stub := authtest.New()
	h := middleware.BasicAuth(stub, inner)

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
	h := middleware.BasicAuth(authtest.New(), inner)

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
	h := middleware.BasicAuth(authtest.New(), inner)

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
	h := middleware.BasicAuth(stub, inner)

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
	h := middleware.BasicAuth(authtest.New(), inner)

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
	h := middleware.BasicAuth(stub, inner)

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
	h := middleware.BasicAuth(stub, inner)

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
			h := middleware.BasicAuth(stub, inner)

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
			h := middleware.BasicAuth(stub, inner)

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
			h := middleware.BasicAuth(stub, inner)

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
			h := middleware.BasicAuth(stub, inner)

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

func TestBasicAuthUnauthorizedHeaders(t *testing.T) {
	inner, _ := newHandlerCalledFlag()
	h := middleware.BasicAuth(authtest.New(), inner)

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
	h := middleware.AdminAuth(stub, inner)

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
	h := middleware.AdminAuth(stub, inner)

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
			h := middleware.AdminAuth(authtest.NewAdmin(), inner)

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
	h := middleware.AdminAuth(authtest.NewAdmin(), inner)

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
			h := middleware.AdminAuth(authtest.NewAdmin(), inner)

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
			h := middleware.AdminAuth(authtest.NewAdmin(), inner)

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
			h := middleware.BasicAuth(stub, inner)

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
			h := middleware.BasicAuth(stub, inner)

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
	h := middleware.BasicAuth(stub, inner)

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
	h := middleware.BasicAuth(stub, inner)

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
			h := middleware.BasicAuth(stub, inner)

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
	h := middleware.BasicAuth(stub, inner)

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
	h := middleware.BasicAuth(stub, inner)

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
			h := middleware.BasicAuth(stub, inner)

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
			middleware.BasicAuth(denied, inner).ServeHTTP(
				httptest.NewRecorder(), authedRequest(tc.method, tc.path, denied))
			if *called {
				t.Fatal("a plain loki data grant must not reach the ruler API")
			}

			inner2, called2 := newHandlerCalledFlag()
			stub := authtest.New()
			stub.Allow = map[string]bool{"loki:" + tc.action: true}
			req := authedRequest(tc.method, tc.path, stub)
			rec := httptest.NewRecorder()
			middleware.BasicAuth(stub, inner2).ServeHTTP(rec, req)

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
			middleware.BasicAuth(stub, inner).ServeHTTP(rec, authedRequest(http.MethodPost, tc.path, stub))
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
			middleware.BasicAuth(stub2, inner2).ServeHTTP(rec2, authedRequest(http.MethodPost, tc.path, stub2))
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
			middleware.BasicAuth(stub, inner).ServeHTTP(rec, authedRequest(http.MethodPost, tc.path, stub))
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
			middleware.BasicAuth(stub, inner).ServeHTTP(rec, authedRequest(tc.method, tc.path, stub))
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
			middleware.BasicAuth(denied, inner2).ServeHTTP(rec2, authedRequest(tc.method, tc.path, denied))
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
			middleware.BasicAuth(loki, inner3).ServeHTTP(rec3, authedRequest(tc.method, tc.path, loki))
			if rec3.Code != http.StatusForbidden {
				t.Fatalf("expected 403 with a loki grant, got %d", rec3.Code)
			}
			if *called3 {
				t.Fatal("handler must not run for a denied request")
			}
		})
	}
}
