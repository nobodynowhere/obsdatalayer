package fanout_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/proxy"
)

// recordedRequest is what one upstream saw.
type recordedRequest struct {
	path  string
	orgID string
}

// tenantRecorder is an upstream that records the tenant assertion on every
// request it receives.
func tenantRecorder(t *testing.T) (*httptest.Server, func() []recordedRequest, func()) {
	t.Helper()
	var mu sync.Mutex
	var seen []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, recordedRequest{path: r.URL.Path, orgID: r.Header.Get("X-Scope-OrgID")})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	get := func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedRequest(nil), seen...)
	}
	reset := func() {
		mu.Lock()
		seen = nil
		mu.Unlock()
	}
	return srv, get, reset
}

// TestBackendDedicatedToOneTenant is the deployment where a whole Mimir, Loki
// or Tempo belongs to a single tenant, rather than the tenant being one of many
// inside a shared cluster.
//
// Two separate guarantees, proven for every backend because each has its own
// route bundle and its own ingest path, and nothing but a test says they behave
// alike:
//
//   - Selection. Only a grant naming the instance's tenant may address it. A
//     grant naming a different tenant gets 404 and the backend is never
//     contacted, which is what stops one tenant's dedicated cluster from being
//     reachable by another.
//   - Assertion. Every request that does reach it carries that tenant in
//     X-Scope-OrgID -- on the write path and the read path alike, since a
//     dedicated cluster with multitenancy still enabled needs the header to
//     land in the right place.
func TestBackendDedicatedToOneTenant(t *testing.T) {
	const dedicatedTenant = "acme"

	cases := []struct {
		backend   string
		writePath string
		readPath  string
	}{
		{"loki", "/loki/api/v1/push", "/loki/loki/api/v1/labels"},
		{"mimir", "/api/v1/push", "/prometheus/api/v1/labels"},
		{"tempo", "/v1/traces", "/tempo/api/search"},
	}

	for _, tc := range cases {
		t.Run(tc.backend, func(t *testing.T) {
			upstream, recorded, reset := tenantRecorder(t)

			inst := &config.InstanceConfig{
				Name:     tc.backend + "-acme",
				Backend:  tc.backend,
				TenantID: dedicatedTenant,
				URL:      upstream.URL,
			}
			client := &http.Client{Timeout: 5 * time.Second}

			newHandler := func() http.Handler {
				return newTestMux(newTestConfig([]*config.InstanceConfig{inst}), proxy.New(client, client), newTestMetrics())
			}

			// The tenant the cluster belongs to reaches it, and the gateway
			// asserts that tenant upstream on both surfaces.
			t.Run("granted tenant reaches it", func(t *testing.T) {
				withAuthTenants(t, dedicatedTenant)
				reset()
				h := newHandler()

				write := httptest.NewRequest(http.MethodPost, tc.writePath, strings.NewReader("payload"))
				write.Header.Set("Authorization", authHeader())
				write.Header.Set("Content-Type", "application/json")
				wrec := httptest.NewRecorder()
				h.ServeHTTP(wrec, write)
				if wrec.Code < 200 || wrec.Code >= 300 {
					t.Fatalf("write: expected 2xx, got %d: %s", wrec.Code, strings.TrimSpace(wrec.Body.String()))
				}

				read := httptest.NewRequest(http.MethodGet, tc.readPath, nil)
				read.Header.Set("Authorization", authHeader())
				rrec := httptest.NewRecorder()
				h.ServeHTTP(rrec, read)
				if rrec.Code != http.StatusOK {
					t.Fatalf("read: expected 200, got %d: %s", rrec.Code, strings.TrimSpace(rrec.Body.String()))
				}

				got := recorded()
				if len(got) != 2 {
					t.Fatalf("expected the backend to see both requests, saw %d: %+v", len(got), got)
				}
				for _, r := range got {
					if r.orgID != dedicatedTenant {
						t.Errorf("%s carried X-Scope-OrgID %q, want %q", r.path, r.orgID, dedicatedTenant)
					}
				}
			})

			// A grant naming the cluster's tenant *and others* must still
			// assert the cluster's, not an arbitrary member of the grant.
			//
			// "beta" is listed first deliberately: a gateway reading the grant
			// instead of the instance would send it, whereas with the
			// instance's tenant first both answers coincide and the assertion
			// proves nothing.
			//
			// Three code paths stamp the instance tenant -- resolveTarget for
			// the push_urls form, the single-url literal in GetTargets, and
			// scopeRequestToInstance narrowing the grant -- so no single one of
			// them is observable on its own. This pins the behaviour they
			// jointly produce, which is the thing that must not change.
			t.Run("wider grant still asserts the cluster tenant", func(t *testing.T) {
				withAuthTenants(t, "beta", dedicatedTenant)
				reset()
				h := newHandler()

				write := httptest.NewRequest(http.MethodPost, tc.writePath, strings.NewReader("payload"))
				write.Header.Set("Authorization", authHeader())
				write.Header.Set("Content-Type", "application/json")
				wrec := httptest.NewRecorder()
				h.ServeHTTP(wrec, write)
				if wrec.Code < 200 || wrec.Code >= 300 {
					t.Fatalf("write: expected 2xx, got %d: %s", wrec.Code, strings.TrimSpace(wrec.Body.String()))
				}
				got := recorded()
				if len(got) != 1 {
					t.Fatalf("expected one upstream request, saw %+v", got)
				}
				if got[0].orgID != dedicatedTenant {
					t.Errorf("write carried X-Scope-OrgID %q, want the cluster's tenant %q", got[0].orgID, dedicatedTenant)
				}

				// The read is refused rather than served: pipe-joining the whole
				// grant would ask a cluster that holds one tenant's data for two
				// tenants' worth, and the gateway does not merge results across
				// instances to make up the difference.
				reset()
				read := httptest.NewRequest(http.MethodGet, tc.readPath, nil)
				read.Header.Set("Authorization", authHeader())
				rrec := httptest.NewRecorder()
				h.ServeHTTP(rrec, read)
				if rrec.Code != http.StatusConflict {
					t.Errorf("read: expected 409 for a multi-tenant grant on a dedicated cluster, got %d: %s",
						rrec.Code, strings.TrimSpace(rrec.Body.String()))
				}
				if got := recorded(); len(got) != 0 {
					t.Errorf("the refused read still reached the backend: %+v", got)
				}
			})

			// A grant for a different tenant must not be able to address this
			// cluster at all -- not merely be filtered by it.
			t.Run("other tenant cannot address it", func(t *testing.T) {
				withAuthTenants(t, "beta")
				reset()
				h := newHandler()

				for _, req := range []*http.Request{
					httptest.NewRequest(http.MethodPost, tc.writePath, strings.NewReader("payload")),
					httptest.NewRequest(http.MethodGet, tc.readPath, nil),
				} {
					req.Header.Set("Authorization", authHeader())
					req.Header.Set("Content-Type", "application/json")
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
					if rec.Code != http.StatusNotFound {
						t.Errorf("%s %s: expected 404, got %d: %s",
							req.Method, req.URL.Path, rec.Code, strings.TrimSpace(rec.Body.String()))
					}
				}
				if got := recorded(); len(got) != 0 {
					t.Errorf("a tenant with no grant for this cluster reached it: %+v", got)
				}
			})
		})
	}
}

// TestDedicatedTempoAssertsOneTenantAcrossSurfaces is the same guarantee where
// it is hardest to hold: a dedicated Tempo whose receivers and HTTP API are
// different listeners, so the write and the read leave the gateway for
// different origins.
//
// Both must assert the same tenant. Targets take the instance's tenant and have
// none of their own, so writing as one tenant and reading as another is not
// expressible -- this proves that end to end rather than at the accessor.
func TestDedicatedTempoAssertsOneTenantAcrossSurfaces(t *testing.T) {
	const dedicatedTenant = "acme"

	ingest, ingestSaw, _ := tenantRecorder(t)
	api, apiSaw, _ := tenantRecorder(t)

	inst := &config.InstanceConfig{
		Name:     "tempo-acme",
		Backend:  "tempo",
		TenantID: dedicatedTenant,
		PushURLs: []config.PushTarget{
			{URL: ingest.URL, Group: config.TargetGroupOTLPHTTP},
			{URL: api.URL, Group: config.TargetGroupQuery},
		},
	}

	withAuthTenants(t, dedicatedTenant)
	client := &http.Client{Timeout: 5 * time.Second}
	h := newTestMux(newTestConfig([]*config.InstanceConfig{inst}), proxy.New(client, client), newTestMetrics())

	write := httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader("spans"))
	write.Header.Set("Authorization", authHeader())
	write.Header.Set("Content-Type", "application/x-protobuf")
	wrec := httptest.NewRecorder()
	h.ServeHTTP(wrec, write)
	if wrec.Code < 200 || wrec.Code >= 300 {
		t.Fatalf("trace push: expected 2xx, got %d: %s", wrec.Code, strings.TrimSpace(wrec.Body.String()))
	}

	read := httptest.NewRequest(http.MethodGet, "/tempo/api/search", nil)
	read.Header.Set("Authorization", authHeader())
	rrec := httptest.NewRecorder()
	h.ServeHTTP(rrec, read)
	if rrec.Code != http.StatusOK {
		t.Fatalf("search: expected 200, got %d: %s", rrec.Code, strings.TrimSpace(rrec.Body.String()))
	}

	// Each surface saw exactly its own request, so the groups routed, and both
	// carried the instance's tenant.
	for _, s := range []struct {
		name string
		saw  []recordedRequest
		path string
	}{
		{"receiver", ingestSaw(), "/v1/traces"},
		{"HTTP API", apiSaw(), "/api/search"},
	} {
		if len(s.saw) != 1 || s.saw[0].path != s.path {
			t.Fatalf("%s saw %+v, want exactly one request to %s", s.name, s.saw, s.path)
		}
		if s.saw[0].orgID != dedicatedTenant {
			t.Errorf("%s carried X-Scope-OrgID %q, want %q", s.name, s.saw[0].orgID, dedicatedTenant)
		}
	}
}
