package fanout_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/auth/authtest"
	"obsdatalayer/internal/config"
	"obsdatalayer/internal/fanout"
	"obsdatalayer/internal/metrics"
	"obsdatalayer/internal/middleware"
	"obsdatalayer/internal/proxy"
)

// operationalMux builds a data plane whose authorization is driven by the given
// stub, so a test can say exactly which grants a caller holds.
func operationalMux(cfg *config.Config, transport http.RoundTripper, stub *authtest.Stub) http.Handler {
	h := config.NewHolder(cfg, "")
	client := &http.Client{Transport: transport}
	p := proxy.New(client, client)
	mux := http.NewServeMux()
	fanout.LokiDSRoutes(mux, "/loki", h, p)
	fanout.MimirDSRoutes(mux, "/prometheus", h, p)
	fanout.TempoDSRoutes(mux, "/tempo", h, p)
	return middleware.BasicAuth(stub, nil, middleware.SanitizeHeaders(mux))
}

// allowing returns a stub granting exactly the named "backend:action" pairs.
func allowing(tenants []string, pairs ...string) *authtest.Stub {
	stub := authtest.New()
	stub.Tenants = tenants
	stub.Allow = map[string]bool{}
	for _, pair := range pairs {
		stub.Allow[pair] = true
	}
	return stub
}

func getOperational(t *testing.T, h http.Handler, stub *authtest.Stub, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", stub.Header())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeDoc(t *testing.T, rec *httptest.ResponseRecorder) fanout.OperationalDoc {
	t.Helper()
	var doc fanout.OperationalDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode response: %v (body %q)", err, rec.Body.String())
	}
	return doc
}

// ---- routing and the endpoint table ----------------------------------------

// TestOperationalAliasesMapToUpstreamPaths pins every entry in the allowlist to
// the upstream path it claims, for all three backends. The aliases are a
// gateway API and the upstream paths differ per project, so this is the table
// that would otherwise only be checked by hand against upstream.md.
func TestOperationalAliasesMapToUpstreamPaths(t *testing.T) {
	for _, tc := range []struct {
		backend, mount, alias, upstream string
	}{
		{"loki", "/loki", "ready", "/ready"},
		{"loki", "/loki", "services", "/services"},
		{"loki", "/loki", "buildinfo", "/loki/api/v1/status/buildinfo"},
		{"loki", "/loki", "config", "/config"},
		{"loki", "/loki", "log_level", "/log_level"},
		{"loki", "/loki", "metrics", "/metrics"},

		{"mimir", "/prometheus", "ready", "/ready"},
		{"mimir", "/prometheus", "services", "/services"},
		{"mimir", "/prometheus", "buildinfo", "/api/v1/status/buildinfo"},
		{"mimir", "/prometheus", "config", "/config"},
		{"mimir", "/prometheus", "status_config", "/api/v1/status/config"},
		{"mimir", "/prometheus", "flags", "/api/v1/status/flags"},
		{"mimir", "/prometheus", "runtime_config", "/runtime_config"},
		{"mimir", "/prometheus", "metrics", "/metrics"},

		{"tempo", "/tempo", "ready", "/ready"},
		{"tempo", "/tempo", "status", "/status"},
		{"tempo", "/tempo", "buildinfo", "/api/status/buildinfo"},
		{"tempo", "/tempo", "echo", "/api/echo"},
		{"tempo", "/tempo", "config", "/status/config"},
		{"tempo", "/tempo", "runtime_config", "/status/runtime_config"},
		{"tempo", "/tempo", "metrics", "/metrics"},
	} {
		t.Run(tc.backend+"/"+tc.alias, func(t *testing.T) {
			capture := &captureTransport{}
			cfg := newTestConfig([]*config.InstanceConfig{{
				Name: "inst", Backend: tc.backend, URL: "http://one.local",
			}})
			stub := allowing([]string{"tenant-a"}, tc.backend+":status", tc.backend+":config", tc.backend+":metrics")
			h := operationalMux(cfg, capture, stub)

			rec := getOperational(t, h, stub, tc.mount+"/targets/inst/"+tc.alias)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
			if capture.path != tc.upstream {
				t.Fatalf("forwarded to %q, want %q", capture.path, tc.upstream)
			}
		})
	}
}

// TestOperationalEndpointTableIsFullyExercised fails when an entry is added to
// the allowlist without a row in the mapping test above, so the table cannot
// grow a route that nothing checks.
func TestOperationalEndpointTableIsFullyExercised(t *testing.T) {
	covered := map[string]bool{
		"loki/ready": true, "loki/services": true, "loki/buildinfo": true,
		"loki/config": true, "loki/log_level": true, "loki/metrics": true,
		"mimir/ready": true, "mimir/services": true, "mimir/buildinfo": true,
		"mimir/config": true, "mimir/status_config": true, "mimir/flags": true,
		"mimir/runtime_config": true, "mimir/metrics": true,
		"tempo/ready": true, "tempo/status": true, "tempo/buildinfo": true,
		"tempo/echo": true, "tempo/config": true, "tempo/runtime_config": true,
		"tempo/metrics": true,
	}
	for _, backend := range []string{"loki", "mimir", "tempo"} {
		for _, e := range fanout.OperationalEndpoints(backend) {
			if !covered[backend+"/"+e.Alias] {
				t.Errorf("endpoint %s/%s is registered but not covered by TestOperationalAliasesMapToUpstreamPaths", backend, e.Alias)
			}
		}
	}
}

func TestOperationalUnknownAliasIsNotRegistered(t *testing.T) {
	capture := &captureTransport{}
	cfg := newTestConfig([]*config.InstanceConfig{{Name: "inst", Backend: "mimir", URL: "http://one.local"}})
	stub := allowing([]string{"tenant-a"}, "mimir:status", "mimir:config", "mimir:metrics")
	h := operationalMux(cfg, capture, stub)

	// Mimir has no /log_level alias, and asking for one must not fall through
	// to some other backend's table.
	rec := getOperational(t, h, stub, "/prometheus/targets/inst/log_level")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if capture.path != "" {
		t.Fatalf("request unexpectedly forwarded to %q", capture.path)
	}
}

// TestAlertmanagerMountHasNoOperationalRoutes pins the deliberate absence. The
// mount resolves to a Mimir instance, so these routes would duplicate the ones
// under /prometheus, and the endpoints specific to Mimir's Alertmanager stream
// every tenant's notification config. See the AlertmanagerDSRoutes comment.
func TestAlertmanagerMountHasNoOperationalRoutes(t *testing.T) {
	h := config.NewHolder(newTestConfig([]*config.InstanceConfig{
		{Name: "mimir-prod", Backend: "mimir", URL: "http://one.local"},
	}), "")
	client := &http.Client{Transport: &captureTransport{}}
	mux := http.NewServeMux()
	fanout.AlertmanagerDSRoutes(mux, "/alertmanager", h, proxy.New(client, client))

	for _, alias := range []string{"ready", "config", "metrics", "configs", "status"} {
		req := httptest.NewRequest(http.MethodGet, "/alertmanager/targets/mimir-prod/"+alias, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("/alertmanager/targets/mimir-prod/%s answered %d; the mount registers no operational routes", alias, rec.Code)
		}
	}

	if _, ok := fanout.BackendMounts["alertmanager"]; ok {
		t.Error("alertmanager is a mount onto Mimir instances, not a backend; it must not appear in BackendMounts")
	}
}

// TestAlertmanagerBuildInfoIsUndoubledAndPrefixedUpstream pins the one route on
// this mount whose gateway path and upstream path disagree about the
// /alertmanager prefix. Grafana's fetchPromBuildInfo is shared with the
// Prometheus data source and appends to the base URL without the prefix its
// Alertmanager endpoints map adds, while Mimir registers this route beneath
// AlertmanagerHTTPPrefix like everything else.
func TestAlertmanagerBuildInfoIsUndoubledAndPrefixedUpstream(t *testing.T) {
	capture := &captureTransport{}
	cfg := newTestConfig([]*config.InstanceConfig{{
		Name: "mimir-prod", Backend: "mimir", URL: "http://one.local",
	}})
	stub := allowing([]string{"tenant-a"}, "mimir:alerts:read")
	h := config.NewHolder(cfg, "")
	client := &http.Client{Transport: capture}
	mux := http.NewServeMux()
	fanout.AlertmanagerDSRoutes(mux, "/alertmanager", h, proxy.New(client, client))
	handler := middleware.BasicAuth(stub, nil, middleware.SanitizeHeaders(mux))

	rec := getOperational(t, handler, stub, "/alertmanager/api/v1/status/buildinfo")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	if capture.path != "/alertmanager/api/v1/status/buildinfo" {
		t.Fatalf("forwarded to %q, want /alertmanager/api/v1/status/buildinfo", capture.path)
	}

	// Grafana's Mimir/Cortex health check probes {url}/api/v2/status first and
	// treats a 200 as proof the endpoint is a vanilla Alertmanager. This mount
	// must keep answering 404 there.
	rec = getOperational(t, handler, stub, "/alertmanager/api/v2/status")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/alertmanager/api/v2/status answered %d; a 200 here fails Grafana's Mimir/Cortex health check", rec.Code)
	}
}

func TestOperationalRoutesAreGetOnly(t *testing.T) {
	capture := &captureTransport{}
	cfg := newTestConfig([]*config.InstanceConfig{{Name: "inst", Backend: "loki", URL: "http://one.local"}})
	stub := allowing([]string{"tenant-a"}, "loki:config")
	h := operationalMux(cfg, capture, stub)

	// dskit serves POST /log_level to change the running log level. The gateway
	// registers GET only, so this must not reach the upstream by any route.
	req := httptest.NewRequest(http.MethodPost, "/loki/targets/inst/log_level", strings.NewReader("log_level=debug"))
	req.Header.Set("Authorization", stub.Header())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("POST to log_level was served with %d", rec.Code)
	}
	if capture.path != "" {
		t.Fatalf("POST reached the upstream at %q", capture.path)
	}
}

// ---- authorization ----------------------------------------------------------

// TestOperationalActionsAreDiscrete is the guarantee that these endpoints are
// not reachable on an ordinary data grant, and that each of the three actions
// authorizes only its own endpoints.
func TestOperationalActionsAreDiscrete(t *testing.T) {
	paths := map[string]string{
		auth.ActionStatus:  "/loki/targets/inst/ready",
		auth.ActionConfig:  "/loki/targets/inst/config",
		auth.ActionMetrics: "/loki/targets/inst/metrics",
	}
	grants := []string{
		"loki:read", "loki:write",
		"loki:" + auth.ActionStatus,
		"loki:" + auth.ActionConfig,
		"loki:" + auth.ActionMetrics,
	}

	for action, path := range paths {
		for _, grant := range grants {
			want := grant == "loki:"+action
			t.Run(action+"/"+grant, func(t *testing.T) {
				capture := &captureTransport{}
				cfg := newTestConfig([]*config.InstanceConfig{{Name: "inst", Backend: "loki", URL: "http://one.local"}})
				stub := allowing([]string{"tenant-a"}, grant)
				h := operationalMux(cfg, capture, stub)

				rec := getOperational(t, h, stub, path)
				got := rec.Code == http.StatusOK
				if got != want {
					t.Fatalf("grant %s on %s: status %d (reached upstream: %v), want allowed=%v",
						grant, path, rec.Code, capture.path != "", want)
				}
			})
		}
	}
}

// TestOperationalTenantScoping covers the access half of a status grant: the
// grant's tenants decide which instances a caller may address, and nothing
// else. A caller must not be able to tell a dedicated instance it cannot see
// from one that does not exist, so both answer 404.
func TestOperationalTenantScoping(t *testing.T) {
	instances := []*config.InstanceConfig{
		{Name: "shared", Backend: "loki", URL: "http://shared.local"},
		{Name: "dedicated-a", Backend: "loki", TenantID: "tenant-a", PushURLs: []config.PushTarget{{URL: "http://a.local"}}},
		{Name: "dedicated-b", Backend: "loki", TenantID: "tenant-b", PushURLs: []config.PushTarget{{URL: "http://b.local"}}},
	}

	for _, tc := range []struct {
		instance string
		want     int
	}{
		{instance: "shared", want: http.StatusOK},
		{instance: "dedicated-a", want: http.StatusOK},
		{instance: "dedicated-b", want: http.StatusNotFound},
		{instance: "no-such-instance", want: http.StatusNotFound},
	} {
		t.Run(tc.instance, func(t *testing.T) {
			capture := &captureTransport{}
			cfg := newTestConfig(instances)
			stub := allowing([]string{"tenant-a"}, "loki:status")
			h := operationalMux(cfg, capture, stub)

			rec := getOperational(t, h, stub, "/loki/targets/"+tc.instance+"/ready")
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// ---- the untenanted guarantee -----------------------------------------------

// TestOperationalRoutesSendNoTenantHeader is the counterpart to
// TestEveryRouteStripsClientHeadersAndInjectsTenancy: everywhere else the
// gateway must assert a tenant, and here it must assert none -- including when
// the instance's target carries a configured tenant ID, which is the fallback
// that would otherwise reintroduce the header.
func TestOperationalRoutesSendNoTenantHeader(t *testing.T) {
	for _, tc := range []struct {
		name string
		inst *config.InstanceConfig
	}{
		{
			name: "target has no tenant",
			inst: &config.InstanceConfig{Name: "inst", Backend: "loki", URL: "http://one.local"},
		},
		{
			name: "target has a configured tenant",
			inst: &config.InstanceConfig{Name: "inst", Backend: "loki", PushURLs: []config.PushTarget{
				{URL: "http://one.local", TenantID: "tenant-a"},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := &captureTransport{}
			cfg := newTestConfig([]*config.InstanceConfig{tc.inst})
			stub := allowing([]string{"tenant-a"}, "loki:status")
			h := operationalMux(cfg, capture, stub)

			req := httptest.NewRequest(http.MethodGet, "/loki/targets/inst/ready", nil)
			req.Header.Set("Authorization", stub.Header())
			req.Header.Set("X-Scope-OrgID", "EVIL")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if org := capture.orgID(); org != "" {
				t.Fatalf("operational request carried X-Scope-OrgID %q; it must carry none", org)
			}
			if got := capture.header.Get("Authorization"); got != "" {
				t.Fatal("client Authorization leaked upstream")
			}
		})
	}
}

// ---- fan-out ----------------------------------------------------------------

// TestOperationalFansOutToEveryTarget is the shape of the answer: one entry per
// configured target, in configuration order, whether or not each answered.
func TestOperationalFansOutToEveryTarget(t *testing.T) {
	upstreams := map[string]http.HandlerFunc{
		"first": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ready\n")) },
		"second": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(503)
			_, _ = w.Write([]byte("not ready\n"))
		},
	}
	servers := map[string]*httptest.Server{}
	for name, handler := range upstreams {
		srv := httptest.NewServer(handler)
		t.Cleanup(srv.Close)
		servers[name] = srv
	}
	// A third target that is not listening at all.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	cfg := newTestConfig([]*config.InstanceConfig{{
		Name: "loki-ha", Backend: "loki",
		PushURLs: []config.PushTarget{
			{URL: servers["first"].URL},
			{URL: servers["second"].URL},
			{URL: deadURL},
		},
	}})
	stub := allowing([]string{"tenant-a"}, "loki:status")
	h := operationalMux(cfg, http.DefaultTransport, stub)

	rec := getOperational(t, h, stub, "/loki/targets/loki-ha/ready")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the gateway asked every target", rec.Code)
	}
	doc := decodeDoc(t, rec)
	if len(doc.Targets) != 3 {
		t.Fatalf("got %d target results, want 3", len(doc.Targets))
	}
	if doc.Instance != "loki-ha" || doc.Backend != "loki" || doc.Endpoint != "ready" {
		t.Fatalf("unexpected envelope: %+v", doc)
	}
	for i, want := range []int{1, 2, 3} {
		if doc.Targets[i].Rank != want {
			t.Errorf("target %d has rank %d, want %d", i, doc.Targets[i].Rank, want)
		}
	}
	if doc.Targets[0].Status != 200 || !strings.Contains(doc.Targets[0].Body, "ready") {
		t.Errorf("target 1 = %+v, want a 200 carrying its body", doc.Targets[0])
	}
	// A target that answers 503 answered. It is not an error.
	if doc.Targets[1].Status != 503 || doc.Targets[1].Error != "" {
		t.Errorf("target 2 = %+v, want status 503 and no error", doc.Targets[1])
	}
	// A target that could not be reached has no status, and its error names no
	// upstream URL.
	if doc.Targets[2].Error == "" || doc.Targets[2].Status != 0 {
		t.Errorf("target 3 = %+v, want an error and no status", doc.Targets[2])
	}
	if strings.Contains(doc.Targets[2].Error, deadURL) {
		t.Errorf("error text leaked the upstream URL: %q", doc.Targets[2].Error)
	}
}

// TestOperationalHidesTargetURLsFromTheDataPlane pins the split between the two
// listeners: the admin UI is shown which target is which, a data-plane caller
// holding a status grant is not.
func TestOperationalHidesTargetURLsFromTheDataPlane(t *testing.T) {
	cfg := newTestConfig([]*config.InstanceConfig{{
		Name: "loki-ha", Backend: "loki",
		PushURLs: []config.PushTarget{{URL: "http://first.local"}, {URL: "http://second.local"}},
	}})
	stub := allowing([]string{"tenant-a"}, "loki:status")

	rec := getOperational(t, operationalMux(cfg, &captureTransport{}, stub), stub, "/loki/targets/loki-ha/ready")
	for _, target := range decodeDoc(t, rec).Targets {
		if target.URL != "" {
			t.Fatalf("data plane response exposed target URL %q", target.URL)
		}
	}

	// The admin registration asks for them, and gets them.
	h := config.NewHolder(cfg, "")
	client := &http.Client{Transport: &captureTransport{}}
	adminMux := http.NewServeMux()
	fanout.RegisterOperationalRoutes(adminMux, "loki", h, proxy.New(client, client), fanout.OperationalOptions{AdminPlane: true})

	req := httptest.NewRequest(http.MethodGet, "/loki/targets/loki-ha/ready", nil)
	adminRec := httptest.NewRecorder()
	adminMux.ServeHTTP(adminRec, req)
	targets := decodeDoc(t, adminRec).Targets
	if len(targets) != 2 || targets[0].URL != "http://first.local" || targets[1].URL != "http://second.local" {
		t.Fatalf("admin response = %+v, want both target URLs", targets)
	}
}

// TestAdminPlaneSeesTenantDedicatedInstances is a regression test. AdminAuth
// puts a RequestAuth with no tenant IDs into the context -- it is not nil -- so
// an admin-listener request that went through the data plane's instance scoping
// would be checked against an empty tenant set and 404 on every tenant-dedicated
// instance. That is precisely the set of instances an operator most needs to
// inspect, and the admin UI's status buttons are the caller.
func TestAdminPlaneSeesTenantDedicatedInstances(t *testing.T) {
	cfg := newTestConfig([]*config.InstanceConfig{{
		Name: "loki-ha", Backend: "loki", TenantID: "tenant-a",
		PushURLs: []config.PushTarget{
			{URL: "http://a.local"},
			{URL: "http://b.local"},
		},
	}})
	h := config.NewHolder(cfg, "")
	client := &http.Client{Transport: &captureTransport{}}
	mux := http.NewServeMux()
	fanout.RegisterOperationalRoutes(mux, "loki", h, proxy.New(client, client), fanout.OperationalOptions{AdminPlane: true})

	req := httptest.NewRequest(http.MethodGet, "/loki/targets/loki-ha/ready", nil)
	req = req.WithContext(auth.WithRequestAuth(req.Context(), &auth.RequestAuth{Username: "admin"}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: an admin grant outranks any tenant scoping (body %q)", rec.Code, rec.Body.String())
	}
	doc := decodeDoc(t, rec)
	if len(doc.Targets) != 2 {
		t.Fatalf("got %d targets, want both", len(doc.Targets))
	}

	// The same instance is invisible to a data-plane caller scoped elsewhere.
	stub := allowing([]string{"tenant-c"}, "loki:status")
	dataRec := getOperational(t, operationalMux(cfg, &captureTransport{}, stub), stub, "/loki/targets/loki-ha/ready")
	if dataRec.Code != http.StatusNotFound {
		t.Fatalf("data plane status = %d, want 404", dataRec.Code)
	}
}

// TestTransportErrorDetailIsAdminOnly pins the third thing AdminPlane governs.
func TestTransportErrorDetailIsAdminOnly(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	cfg := newTestConfig([]*config.InstanceConfig{{
		Name: "loki-ha", Backend: "loki", URL: deadURL,
	}})

	stub := allowing([]string{"tenant-a"}, "loki:status")
	h := config.NewHolder(cfg, "")
	p := proxy.New(http.DefaultClient, http.DefaultClient)
	dataMux := http.NewServeMux()
	fanout.RegisterOperationalRoutes(dataMux, "loki", h, p, fanout.OperationalOptions{})
	dataHandler := middleware.BasicAuth(stub, nil, dataMux)

	doc := decodeDoc(t, getOperational(t, dataHandler, stub, "/loki/targets/loki-ha/ready"))
	if got := doc.Targets[0].Error; got != "upstream unavailable" {
		t.Fatalf("data plane error = %q, want the opaque phrase; it must not carry the upstream address", got)
	}
	if strings.Contains(doc.Targets[0].Error, deadURL) {
		t.Fatal("data plane error leaked the upstream URL")
	}

	adminMux := http.NewServeMux()
	fanout.RegisterOperationalRoutes(adminMux, "loki", h, p, fanout.OperationalOptions{AdminPlane: true})
	req := httptest.NewRequest(http.MethodGet, "/loki/targets/loki-ha/ready", nil)
	req = req.WithContext(auth.WithRequestAuth(req.Context(), &auth.RequestAuth{Username: "admin"}))
	adminRec := httptest.NewRecorder()
	adminMux.ServeHTTP(adminRec, req)

	adminDoc := decodeDoc(t, adminRec)
	if adminDoc.Targets[0].Error == "upstream unavailable" || adminDoc.Targets[0].Error == "" {
		t.Fatalf("admin error = %q, want the underlying transport failure", adminDoc.Targets[0].Error)
	}
}

// TestOperationalRequestsAreCounted pins the three outcomes apart. A counter
// that only knew success from failure would report a replica refusing
// connections and a replica honestly answering "not ready" as the same event,
// and those call for different action.
func TestOperationalRequestsAreCounted(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ready\n"))
	}))
	t.Cleanup(up.Close)
	notReady := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(notReady.Close)
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	goneURL := gone.URL
	gone.Close()

	m := newTestMetrics()
	cfg := newTestConfig([]*config.InstanceConfig{{
		Name: "loki-ha", Backend: "loki",
		PushURLs: []config.PushTarget{{URL: up.URL}, {URL: notReady.URL}, {URL: goneURL}},
	}})
	h := config.NewHolder(cfg, "")
	p := proxy.New(http.DefaultClient, http.DefaultClient)
	p.SetMetrics(m)
	mux := http.NewServeMux()
	fanout.RegisterOperationalRoutes(mux, "loki", h, p, fanout.OperationalOptions{})

	stub := allowing([]string{"tenant-a"}, "loki:status")
	handler := middleware.BasicAuth(stub, nil, mux)
	if rec := getOperational(t, handler, stub, "/loki/targets/loki-ha/ready"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	for _, tc := range []struct{ target, result string }{
		{up.URL, metrics.OperationalSuccess},
		{notReady.URL, metrics.OperationalError},
		{goneURL, metrics.OperationalUnreachable},
	} {
		if got := m.OperationalValue("loki-ha", tc.target, "ready", tc.result); got != 1 {
			t.Errorf("counter for %s/%s = %d, want 1", tc.target, tc.result, got)
		}
	}

	// The endpoint label is the gateway's alias, not the upstream path, so the
	// same question compares across backends.
	if got := m.OperationalValue("loki-ha", up.URL, "/ready", metrics.OperationalSuccess); got != 0 {
		t.Errorf("counter is labeled by upstream path, not alias: %d", got)
	}

	// And none of it touched the read counters: asking target 2 whether it is
	// ready must not make the gateway report that target 2 failed a read.
	for _, target := range []string{up.URL, notReady.URL, goneURL} {
		if got := m.ReadValue("loki-ha", target, "failure"); got != 0 {
			t.Errorf("operational request recorded a read failure for %s (%d)", target, got)
		}
	}
}

// TestLegacyMimirConfigPathsDoNotFeedReadHealth is the regression test for the
// half-migrated state: these two paths were gated by the config action while
// still being forwarded as reads, so a failing configuration dump recorded a
// read failure and pushed a healthy replica into the read cool-off, degrading
// real query traffic on the strength of an endpoint nothing calls on a
// schedule.
func TestLegacyMimirConfigPathsDoNotFeedReadHealth(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	for _, path := range []string{
		"/prometheus/api/v1/status/config",
		"/prometheus/api/v1/status/flags",
	} {
		t.Run(path, func(t *testing.T) {
			m := newTestMetrics()
			cfg := newTestConfig([]*config.InstanceConfig{{
				Name: "mimir-prod", Backend: "mimir", URL: deadURL,
			}})
			h := config.NewHolder(cfg, "")
			p := proxy.New(http.DefaultClient, http.DefaultClient)
			p.SetMetrics(m)
			mux := http.NewServeMux()
			fanout.MimirDSRoutes(mux, "/prometheus", h, p)

			stub := allowing([]string{"tenant-a"}, "mimir:config")
			rec := getOperational(t, middleware.BasicAuth(stub, nil, mux), stub, path)
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502 from an unreachable target", rec.Code)
			}

			if got := m.ReadValue("mimir-prod", deadURL, "failure"); got != 0 {
				t.Errorf("recorded %d read failures; a configuration dump must not feed read health", got)
			}
			if got := m.OperationalValue("mimir-prod", deadURL, aliasFor(path), metrics.OperationalUnreachable); got != 1 {
				t.Errorf("operational counter = %d, want 1 under the same alias its per-target twin uses", got)
			}
		})
	}
}

func aliasFor(path string) string {
	if strings.HasSuffix(path, "/flags") {
		return "flags"
	}
	return "status_config"
}

// TestLegacyMimirConfigPreservesForwardQuerySemantics pins the parts of the old
// read path that a caller can actually observe, because moving these routes off
// ForwardQuery is only safe if none of them changed: a 5xx moves on to the next
// target, a 4xx does not, the last real answer is reported when every target
// fails, and the upstream's response headers arrive intact.
func TestLegacyMimirConfigPreservesForwardQuerySemantics(t *testing.T) {
	newUpstream := func(t *testing.T, status int, body string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Mimir-Marker", body)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}

	serve := func(t *testing.T, urls ...string) *httptest.ResponseRecorder {
		t.Helper()
		targets := make([]config.PushTarget, 0, len(urls))
		for _, u := range urls {
			targets = append(targets, config.PushTarget{URL: u})
		}
		cfg := newTestConfig([]*config.InstanceConfig{{
			Name: "mimir-prod", Backend: "mimir", PushURLs: targets,
		}})
		mux := http.NewServeMux()
		fanout.MimirDSRoutes(mux, "/prometheus", config.NewHolder(cfg, ""), proxy.New(http.DefaultClient, http.DefaultClient))
		stub := allowing([]string{"tenant-a"}, "mimir:config")
		return getOperational(t, middleware.BasicAuth(stub, nil, mux), stub, "/prometheus/api/v1/status/config")
	}

	t.Run("a 5xx moves on to the next target", func(t *testing.T) {
		rec := serve(t, newUpstream(t, 500, "broken"), newUpstream(t, 200, "good"))
		if rec.Code != http.StatusOK || rec.Body.String() != "good" {
			t.Fatalf("status=%d body=%q, want the second target to have covered", rec.Code, rec.Body.String())
		}
	})

	t.Run("a 4xx is the upstream answering and is relayed", func(t *testing.T) {
		rec := serve(t, newUpstream(t, 404, "nope"), newUpstream(t, 200, "good"))
		if rec.Code != http.StatusNotFound || rec.Body.String() != "nope" {
			t.Fatalf("status=%d body=%q, want the 404 relayed without trying target 2", rec.Code, rec.Body.String())
		}
	})

	t.Run("every target 5xx reports the last real answer", func(t *testing.T) {
		rec := serve(t, newUpstream(t, 500, "first"), newUpstream(t, 503, "second"))
		if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "second" {
			t.Fatalf("status=%d body=%q, want the last upstream answer rather than an invented one", rec.Code, rec.Body.String())
		}
	})

	t.Run("upstream response headers arrive intact", func(t *testing.T) {
		rec := serve(t, newUpstream(t, 200, "good"))
		if got := rec.Header().Get("X-Mimir-Marker"); got != "good" {
			t.Errorf("X-Mimir-Marker = %q; headers beyond Content-Type must not be dropped", got)
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
	})
}

// TestLegacyMimirConfigRefusesAnOversizeBody keeps the passthrough honest.
// Collecting a body that used to be streamed means holding it whole, so it is
// capped -- and a verbatim passthrough has no field in which to say it was cut,
// so a body over the cap has to be an error rather than a short answer that
// parses.
func TestLegacyMimirConfigRefusesAnOversizeBody(t *testing.T) {
	huge := strings.Repeat("y", (1<<20)+1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(huge))
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{{
		Name: "mimir-prod", Backend: "mimir", URL: upstream.URL,
	}})
	h := config.NewHolder(cfg, "")
	mux := http.NewServeMux()
	fanout.MimirDSRoutes(mux, "/prometheus", h, proxy.New(http.DefaultClient, http.DefaultClient))

	stub := allowing([]string{"tenant-a"}, "mimir:config")
	rec := getOperational(t, middleware.BasicAuth(stub, nil, mux), stub, "/prometheus/api/v1/status/config")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 rather than a silently truncated config", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "yyyy") {
		t.Fatal("a truncated configuration was passed through to the client")
	}
}

// TestOperationalBodyIsCapped keeps one enormous /metrics from being held and
// returned whole, and requires the answer to say that it was cut.
func TestOperationalBodyIsCapped(t *testing.T) {
	huge := strings.Repeat("x", (1<<20)+4096)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(huge))
	}))
	t.Cleanup(upstream.Close)

	cfg := newTestConfig([]*config.InstanceConfig{{Name: "inst", Backend: "loki", URL: upstream.URL}})
	stub := allowing([]string{"tenant-a"}, "loki:metrics")
	h := operationalMux(cfg, http.DefaultTransport, stub)

	doc := decodeDoc(t, getOperational(t, h, stub, "/loki/targets/inst/metrics"))
	if len(doc.Targets) != 1 {
		t.Fatalf("got %d results, want 1", len(doc.Targets))
	}
	if got := len(doc.Targets[0].Body); got != 1<<20 {
		t.Fatalf("body is %d bytes, want it capped at %d", got, 1<<20)
	}
	if !doc.Targets[0].Truncated {
		t.Fatal("a capped body must report truncated")
	}
}
