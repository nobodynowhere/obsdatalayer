package middleware_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/middleware"
)

// ---- helpers ---------------------------------------------------------------

func newHandlerCalledFlag() (http.Handler, *bool) {
	called := false
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	return h, &called
}

// makeTestUserFile creates a *auth.UserFile with a single user "testuser"/"testpass"
// who has wildcard access to all backends/actions for tenant "test-tenant".
func makeTestUserFile(t *testing.T) *auth.UserFile {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("testpass"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	uf, err := auth.New([]*auth.User{{
		Name:           "testuser",
		PasswordBcrypt: string(hash),
		Policies: []auth.Policy{{
			Backends:  []string{"*"},
			Actions:   []string{"read", "write"},
			TenantIDs: []string{"test-tenant"},
		}},
	}})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	return uf
}

// makeTestUserFileNoWrite creates a user who can only read.
func makeTestUserFileNoWrite(t *testing.T) *auth.UserFile {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("testpass"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	uf, err := auth.New([]*auth.User{{
		Name:           "readonly",
		PasswordBcrypt: string(hash),
		Policies: []auth.Policy{{
			Backends:  []string{"loki"},
			Actions:   []string{"read"},
			TenantIDs: []string{"test-tenant"},
		}},
	}})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	return uf
}

func basicAuthHeader(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", user, pass)))
}

// ---- tests -----------------------------------------------------------------

func TestBasicAuthValidCreds(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	h := middleware.BasicAuth(makeTestUserFile(t), inner)

	req := httptest.NewRequest(http.MethodGet, "/api/loki-prod/loki/labels", nil)
	req.Header.Set("Authorization", basicAuthHeader("testuser", "testpass"))
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
	h := middleware.BasicAuth(makeTestUserFile(t), inner)

	req := httptest.NewRequest(http.MethodGet, "/api/loki-prod/loki/labels", nil)
	req.Header.Set("Authorization", basicAuthHeader("testuser", "wrongpassword"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if *called {
		t.Error("expected inner handler NOT to be called")
	}
}

func TestBasicAuthUnknownUser(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	h := middleware.BasicAuth(makeTestUserFile(t), inner)

	req := httptest.NewRequest(http.MethodGet, "/api/loki-prod/loki/labels", nil)
	req.Header.Set("Authorization", basicAuthHeader("nobody", "testpass"))
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
	h := middleware.BasicAuth(makeTestUserFile(t), inner)

	req := httptest.NewRequest(http.MethodGet, "/api/loki-prod/loki/labels", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if *called {
		t.Error("expected inner handler NOT to be called")
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "unauthorized" {
		t.Errorf("expected error='unauthorized', got %q", body["error"])
	}
}

func TestBasicAuthForbiddenNoPolicy(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	// User can only read loki, not write.
	h := middleware.BasicAuth(makeTestUserFileNoWrite(t), inner)

	req := httptest.NewRequest(http.MethodPost, "/api/loki-prod/loki/push", nil)
	req.Header.Set("Authorization", basicAuthHeader("readonly", "testpass"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
	if *called {
		t.Error("expected inner handler NOT to be called on 403")
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "forbidden" {
		t.Errorf("expected error='forbidden', got %q", body["error"])
	}
}

func TestBasicAuthSkipsHealthz(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	h := middleware.BasicAuth(makeTestUserFile(t), inner)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	// No Authorization header
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for /healthz without auth, got %d", rec.Code)
	}
	if !*called {
		t.Error("expected inner handler to be called for /healthz")
	}
}

func TestBasicAuthContextHasRequestAuth(t *testing.T) {
	var capturedRA *auth.RequestAuth
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedRA = auth.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := middleware.BasicAuth(makeTestUserFile(t), inner)

	req := httptest.NewRequest(http.MethodGet, "/api/loki-prod/loki/labels", nil)
	req.Header.Set("Authorization", basicAuthHeader("testuser", "testpass"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if capturedRA == nil {
		t.Fatal("expected RequestAuth in context, got nil")
	}
	if capturedRA.Username != "testuser" {
		t.Errorf("expected username 'testuser', got %q", capturedRA.Username)
	}
	if !capturedRA.IsRead {
		t.Error("expected IsRead=true for GET request")
	}
	if len(capturedRA.TenantIDs) == 0 {
		t.Error("expected non-empty TenantIDs")
	}
	if capturedRA.TenantIDs[0] != "test-tenant" {
		t.Errorf("expected TenantIDs[0]='test-tenant', got %q", capturedRA.TenantIDs[0])
	}
}

func TestBasicAuthWriteContextNotIsRead(t *testing.T) {
	var capturedRA *auth.RequestAuth
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedRA = auth.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := middleware.BasicAuth(makeTestUserFile(t), inner)

	req := httptest.NewRequest(http.MethodPost, "/api/loki-prod/loki/push", nil)
	req.Header.Set("Authorization", basicAuthHeader("testuser", "testpass"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if capturedRA == nil {
		t.Fatal("expected RequestAuth in context")
	}
	if capturedRA.IsRead {
		t.Error("expected IsRead=false for POST request")
	}
}

func TestBasicAuthUnauthorizedHasWWWAuthenticate(t *testing.T) {
	inner, _ := newHandlerCalledFlag()
	h := middleware.BasicAuth(makeTestUserFile(t), inner)

	req := httptest.NewRequest(http.MethodGet, "/api/loki-prod/loki/labels", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("expected WWW-Authenticate header on 401")
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type application/json on 401, got %q", rec.Header().Get("Content-Type"))
	}
}
