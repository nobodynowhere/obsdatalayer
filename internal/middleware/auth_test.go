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

	req := httptest.NewRequest(http.MethodGet, "/api/loki/labels", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/api/loki/labels", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/api/loki/labels", nil)
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

	req := httptest.NewRequest(http.MethodPost, "/api/loki/push", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/api/loki/labels", nil)
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

	req := httptest.NewRequest(http.MethodPost, "/api/loki/push", nil)
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
		"/api/mimir/prometheus/api/v1/query",
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

func TestBasicAuthUnauthorizedHeaders(t *testing.T) {
	inner, _ := newHandlerCalledFlag()
	h := middleware.BasicAuth(authtest.New(), inner)

	req := httptest.NewRequest(http.MethodGet, "/api/loki/labels", nil)
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
