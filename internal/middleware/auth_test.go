package middleware_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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

func TestBearerAuthValidToken(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	h := middleware.BearerAuth("secret", inner)

	req := httptest.NewRequest(http.MethodGet, "/api/loki-prod/loki/labels", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if !*called {
		t.Error("expected inner handler to be called")
	}
}

func TestBearerAuthMissingHeader(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	h := middleware.BearerAuth("secret", inner)

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

func TestBearerAuthWrongToken(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	h := middleware.BearerAuth("secret", inner)

	req := httptest.NewRequest(http.MethodGet, "/api/loki-prod/loki/labels", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if *called {
		t.Error("expected inner handler NOT to be called")
	}
}

func TestBearerAuthNoPrefixJustToken(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	h := middleware.BearerAuth("secret", inner)

	req := httptest.NewRequest(http.MethodGet, "/api/loki-prod/loki/labels", nil)
	req.Header.Set("Authorization", "secret") // No "Bearer " prefix
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if *called {
		t.Error("expected inner handler NOT to be called")
	}
}

func TestBearerAuthEmptyTokenEmptyHeader(t *testing.T) {
	inner, _ := newHandlerCalledFlag()
	h := middleware.BearerAuth("", inner)

	req := httptest.NewRequest(http.MethodGet, "/api/loki-prod/loki/labels", nil)
	// No Authorization header set
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Missing "Bearer " prefix still fails
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing header even with empty token, got %d", rec.Code)
	}
}

func TestBearerAuthSkipsHealthz(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	h := middleware.BearerAuth("secret", inner)

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

func TestBearerAuthSkipsMetrics(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	h := middleware.BearerAuth("secret", inner)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	// No Authorization header
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for /metrics without auth, got %d", rec.Code)
	}
	if !*called {
		t.Error("expected inner handler to be called for /metrics")
	}
}

func TestBearerAuthValidTokenOnPushRoute(t *testing.T) {
	inner, called := newHandlerCalledFlag()
	h := middleware.BearerAuth("my-token", inner)

	req := httptest.NewRequest(http.MethodPost, "/api/foo/loki/push", nil)
	req.Header.Set("Authorization", "Bearer my-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if !*called {
		t.Error("expected inner handler to be called")
	}
}

func TestBearerAuthUnauthorizedContentType(t *testing.T) {
	inner, _ := newHandlerCalledFlag()
	h := middleware.BearerAuth("secret", inner)

	req := httptest.NewRequest(http.MethodGet, "/api/loki-prod/loki/labels", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type application/json on 401, got %q", ct)
	}
}
