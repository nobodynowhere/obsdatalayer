package adminapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
