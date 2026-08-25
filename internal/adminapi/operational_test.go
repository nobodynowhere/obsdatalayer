package adminapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/fanout"
)

// TestOperationalCatalogMatchesTheRegisteredTable is what makes serving the
// catalog worth doing: the admin UI renders from this response, so it must
// describe exactly what the gateway registers -- every alias, its action, and
// the mount it answers under.
func TestOperationalCatalogMatchesTheRegisteredTable(t *testing.T) {
	env := newEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/operational-endpoints", nil)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	var doc struct {
		Mounts    map[string]string `json:"mounts"`
		Endpoints map[string][]struct {
			Alias  string `json:"alias"`
			Action string `json:"action"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(doc.Mounts) != len(fanout.BackendMounts) {
		t.Fatalf("catalog names %d mounts, gateway registers %d", len(doc.Mounts), len(fanout.BackendMounts))
	}
	for backend, mount := range fanout.BackendMounts {
		if doc.Mounts[backend] != mount {
			t.Errorf("mount for %s = %q, want %q", backend, doc.Mounts[backend], mount)
		}
		want := fanout.OperationalEndpoints(backend)
		got := doc.Endpoints[backend]
		if len(got) != len(want) {
			t.Errorf("%s: catalog lists %d endpoints, table has %d", backend, len(got), len(want))
			continue
		}
		for i := range want {
			if got[i].Alias != want[i].Alias || got[i].Action != want[i].Action {
				t.Errorf("%s[%d] = %s/%s, want %s/%s",
					backend, i, got[i].Alias, got[i].Action, want[i].Alias, want[i].Action)
			}
		}
	}
}

// TestCatalogServesTargetGroupsPerBackend is the anti-drift guarantee for the
// instance editor's Group dropdown. The SPA renders from this response rather
// than from its own copy of the rules, so it must agree with config exactly: a
// group offered but rejected on save, or accepted but never offered, is the
// failure this prevents.
func TestCatalogServesTargetGroupsPerBackend(t *testing.T) {
	env := newEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/operational-endpoints", nil)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	var doc struct {
		TargetGroups map[string][]string `json:"target_groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for backend := range fanout.BackendMounts {
		want := config.GroupsForBackend(backend)
		got := doc.TargetGroups[backend]
		if len(got) != len(want) {
			t.Fatalf("backend %q: served %v, config allows %v", backend, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("backend %q: served %v, config allows %v", backend, got, want)
				break
			}
		}
		// Every served group must actually validate, or the dropdown offers a
		// choice the save path refuses.
		for _, g := range got {
			if !config.BackendAllowsGroup(backend, g) {
				t.Errorf("backend %q: served group %q that validation rejects", backend, g)
			}
		}
	}

	// The legacy fallback is not a menu choice and must not be served as one.
	for backend, groups := range doc.TargetGroups {
		for _, g := range groups {
			if g == "" {
				t.Errorf("backend %q: the empty legacy group was served as a choice", backend)
			}
		}
	}
}
