package adminapi_test

import (
	"net/http"
	"strings"
	"testing"
)

// Reordering push targets while their credentials are masked must not attach a
// credential to the wrong upstream.
func TestRedactedCredentialFollowsItsTarget(t *testing.T) {
	e := newEnv(t)

	create := map[string]any{
		"name": "mimir-prod", "backend": "mimir", "fan_out_mode": "any",
		"push_urls": []map[string]any{
			{"url": "http://a.local", "basic_auth": "user:AAA"},
			{"url": "http://b.local", "basic_auth": "user:BBB"},
		},
	}
	if rec := e.do(t, http.MethodPost, "/instances", create); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}

	// Swap the order, leaving both credentials masked as the UI would.
	update := map[string]any{
		"name": "mimir-prod", "backend": "mimir", "fan_out_mode": "any",
		"push_urls": []map[string]any{
			{"url": "http://b.local", "basic_auth": "<redacted>"},
			{"url": "http://a.local", "basic_auth": "<redacted>"},
		},
	}
	if rec := e.do(t, http.MethodPut, "/instances/mimir-prod", update); rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}

	inst := e.cfg.Get().ByName["mimir-prod"]
	byURL := map[string]string{}
	for _, pt := range inst.PushURLs {
		byURL[pt.URL] = pt.BasicAuth
	}
	if got := byURL["http://a.local"]; got != "user:AAA" {
		t.Errorf("a.local should keep user:AAA, got %q", got)
	}
	if got := byURL["http://b.local"]; got != "user:BBB" {
		t.Errorf("b.local should keep user:BBB, got %q", got)
	}
}

// A mask that cannot be tied to a stored credential is refused rather than
// resolved by guesswork.
func TestUnresolvableRedactedCredentialIsRejected(t *testing.T) {
	e := newEnv(t)

	create := map[string]any{
		"name": "mimir-prod", "backend": "mimir", "fan_out_mode": "any",
		"push_urls": []map[string]any{{"url": "http://a.local", "basic_auth": "user:AAA"}},
	}
	if rec := e.do(t, http.MethodPost, "/instances", create); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}

	// A brand new URL carrying the mask has no credential to inherit.
	update := map[string]any{
		"name": "mimir-prod", "backend": "mimir", "fan_out_mode": "any",
		"push_urls": []map[string]any{{"url": "http://new.local", "basic_auth": "<redacted>"}},
	}
	rec := e.do(t, http.MethodPut, "/instances/mimir-prod", update)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "no stored credential") {
		t.Errorf("expected an explanatory error, got %s", rec.Body)
	}
}

// An unchanged round-trip keeps every credential exactly where it was.
func TestRedactedCredentialSurvivesNoOpUpdate(t *testing.T) {
	e := newEnv(t)

	create := map[string]any{
		"name": "mimir-prod", "backend": "mimir", "fan_out_mode": "any",
		"basic_auth": "inst:SECRET",
		"push_urls": []map[string]any{
			{"url": "http://a.local", "basic_auth": "user:AAA"},
			{"url": "http://b.local"},
		},
	}
	if rec := e.do(t, http.MethodPost, "/instances", create); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}

	// Echo back exactly what a GET returns, masks included.
	rec := e.do(t, http.MethodGet, "/instances/mimir-prod", nil)
	var doc map[string]any
	decodeInto(t, rec, &doc)
	if rec := e.do(t, http.MethodPut, "/instances/mimir-prod", doc); rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}

	inst := e.cfg.Get().ByName["mimir-prod"]
	if inst.BasicAuth != "inst:SECRET" {
		t.Errorf("instance credential lost: %q", inst.BasicAuth)
	}
	if inst.PushURLs[0].BasicAuth != "user:AAA" {
		t.Errorf("target credential lost: %q", inst.PushURLs[0].BasicAuth)
	}
}
