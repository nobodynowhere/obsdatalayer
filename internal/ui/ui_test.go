package ui_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"obsdatalayer/internal/ui"
)

// requireBundle skips tests that need a real build. The bundle is a build
// artifact, so `go test ./...` on a fresh clone runs without one.
func requireBundle(t *testing.T) {
	t.Helper()
	rec := get(t, "/ui/")
	if rec.Code == http.StatusNotFound {
		t.Skip("no UI bundle built; run npm run build in ui/")
	}
}

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	ui.Handler().ServeHTTP(rec, req)
	return rec
}

func TestServesIndex(t *testing.T) {
	requireBundle(t)
	rec := get(t, "/ui/")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("expected html content type, got %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "<html") {
		t.Error("expected an HTML document")
	}
}

// Client-side routes are not files on disk. A refresh or a deep link has to
// return the SPA shell rather than a 404.
func TestUnknownPathFallsBackToIndex(t *testing.T) {
	requireBundle(t)
	for _, path := range []string{"/ui/tenants", "/ui/users", "/ui/roles/deep/link"} {
		t.Run(path, func(t *testing.T) {
			rec := get(t, path)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200 for SPA route, got %d", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "<html") {
				t.Error("expected the SPA shell")
			}
		})
	}
}

// The shell must not be cached: a redeployed gateway ships new hashed asset
// names, and a stale shell would reference assets that no longer exist.
func TestIndexIsNotCached(t *testing.T) {
	requireBundle(t)
	rec := get(t, "/ui/")
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("expected Cache-Control no-cache on the shell, got %q", cc)
	}
}

func TestShellSetsHardeningHeaders(t *testing.T) {
	requireBundle(t)
	rec := get(t, "/ui/")
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("expected nosniff, got %q", got)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("expected DENY, got %q", got)
	}
}

func TestIsUIPath(t *testing.T) {
	cases := map[string]bool{
		"/":                     true,
		"/ui":                   true,
		"/ui/":                  true,
		"/ui/assets/app.js":     true,
		"/ui/tenants":           true,
		"/tenants":              false,
		"/users":                false,
		"/config":               false,
		"/metrics":              false,
		"/healthz":              false,
		"/uiconfig":             false, // prefix-adjacent, must not match
		"/api/inst/loki/labels": false,
	}
	for path, want := range cases {
		if got := ui.IsUIPath(path); got != want {
			t.Errorf("IsUIPath(%q) = %v, want %v", path, got, want)
		}
	}
}

// A traversal attempt must not escape the embedded filesystem. Anything that
// does not resolve to a real embedded file falls back to the shell.
func TestPathTraversalIsContained(t *testing.T) {
	for _, path := range []string{"/ui/../../etc/passwd", "/ui/./../main.go"} {
		rec := get(t, path)
		body := rec.Body.String()
		if strings.Contains(body, "root:") || strings.Contains(body, "package main") {
			t.Fatalf("path %q escaped the embedded filesystem", path)
		}
	}
}
