package fanout

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/config"
	"obsdatalayer/internal/proxy"
)

// maxOperationalBodyBytes caps how much of one target's answer is held and
// returned. The fan-out answers with every target at once, so the response is
// this times the number of targets in the worst case, and /metrics on a busy
// backend is the endpoint that gets there. A body that hits the cap comes back
// marked truncated rather than dropped: the head of a Prometheus exposition or
// a config dump is still worth reading.
const maxOperationalBodyBytes = 1 << 20

// BackendMounts is the data source mount each backend is served under, and the
// one place that mapping lives: main.go mounts the route bundles from it, the
// admin listener registers from it, and the authorization middleware resolves a
// mount back to a backend through it.
//
// Keys are backends, which is why /alertmanager is absent. That is a data
// source mount too, but it is a URL shape adapting Grafana's Alertmanager
// patterns onto Mimir instances rather than a system of its own -- see
// AlertmanagerDSRoutes.
var BackendMounts = map[string]string{
	"loki":  "/loki",
	"mimir": "/prometheus",
	"tempo": "/tempo",
}

// OperationalEndpoint is one allowlisted upstream operational endpoint: the
// short alias it is addressed by, the upstream path it maps to, and the grant
// action a caller needs for it.
//
// The alias is deliberately not the upstream path. These routes are a gateway
// API, not a mirror of anyone's URL space: Loki's build info lives at
// /loki/api/v1/status/buildinfo and Tempo's at /api/status/buildinfo, and a
// caller asking "what is this target running" should not have to know that.
type OperationalEndpoint struct {
	Alias    string
	Upstream string
	Action   string
}

// operationalEndpoints is the lookup table the registrar and the authorization
// middleware share. Each backend's entries are declared in that backend's own
// route file, beside the bundle that registers them -- mimirOperationalEndpoints
// in mimir.go, and so on -- so that "what does the /prometheus mount serve" has
// one answer in one place, which is the whole point of the per-backend
// grouping. This is only the index over them.
var operationalEndpoints = map[string][]OperationalEndpoint{
	"loki":  lokiOperationalEndpoints,
	"mimir": mimirOperationalEndpoints,
	"tempo": tempoOperationalEndpoints,
}

// legacyOperationalPaths maps endpoints that were already registered at their
// upstream paths under a data source mount, before these actions existed, onto
// the action they now need. Like the tables above, each backend declares its
// own in its route file.
//
// It is consulted by OperationalAction so that the middleware asks one question
// and this package answers it. The alternative -- middleware knowing which
// Mimir paths dump configuration -- puts a Mimir fact somewhere no one reading
// mimir.go would find it.
var legacyOperationalPaths = mergeStringMaps(
	lokiLegacyOperationalPaths,
	mimirLegacyOperationalPaths,
	tempoLegacyOperationalPaths,
)

func mergeStringMaps(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// OperationalEndpoints returns the allowlist for one backend, for tests and for
// the admin API to describe to the UI. It returns a copy: the table itself is
// read without synchronisation by every request passing through authorization,
// so handing out a slice into it would make a caller's tidy-up a data race.
func OperationalEndpoints(backend string) []OperationalEndpoint {
	return append([]OperationalEndpoint(nil), operationalEndpoints[backend]...)
}

// OperationalAction reports the grant action a request path requires, for the
// authorization middleware. It returns "" for anything that is not shaped like
// one of these routes, which is what tells the caller to fall back to its
// ordinary read/write classification.
//
// The middleware asks this rather than pattern-matching the path itself,
// because the mapping from alias to action lives in the table above and there
// must not be a second copy of it that can disagree about, say, whether
// runtime_config is config or status.
//
// What decides that a path is one of these routes is its *shape* --
// /{anything}/targets/{instance}/{alias} -- and not whether the mount is one
// this package currently registers. That is deliberate. If it keyed off
// BackendMounts, then mounting this route family on a new mount without adding it to
// BackendMounts would leave every one of its routes classified as a plain read: the
// authorization would silently fall back to the weakest grant while the routes
// themselves worked fine, which is the failure mode these actions exist to
// prevent. An unrecognised mount or alias resolves to the most restrictive
// action instead, and the request goes on to 404 the same as before.
func OperationalAction(path string) string {
	if action, ok := legacyOperationalPaths[path]; ok {
		return action
	}
	backend, alias, ok := parseOperationalPath(path)
	if !ok {
		return ""
	}
	for _, e := range operationalEndpoints[backend] {
		if e.Alias == alias {
			return e.Action
		}
	}
	return auth.ActionMetrics
}

// parseOperationalPath splits /{mount}/targets/{instance}/{alias}. It reports
// ok for the shape; backend is empty when the mount is not one this package
// registers.
func parseOperationalPath(path string) (backend, alias string, ok bool) {
	// This runs for every data-plane request on its way through authorization,
	// and almost none of them are these routes. Scan before allocating.
	if !strings.Contains(path, "/targets/") {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) != 4 || parts[1] != "targets" {
		return "", "", false
	}
	for b, mount := range BackendMounts {
		if "/"+parts[0] == mount {
			return b, parts[3], true
		}
	}
	return "", parts[3], true
}

// OperationalOptions configures one registration of the operational routes.
type OperationalOptions struct {
	// AdminPlane marks the registration on the admin listener, where the
	// request has already been through AdminAuth and therefore carries an admin
	// grant -- strictly more than any data-plane grant conveys.
	//
	// It names the listener rather than any one of its effects because there
	// are three, and they stand or fall together:
	//
	//   - Instance scoping is skipped. On the data plane a grant's tenant IDs
	//     decide which instances the holder may address; an admin holds no such
	//     grant, and gating on one would hide exactly the tenant-dedicated
	//     instances an operator most needs to look at.
	//   - Target URLs are returned. An operator is looking at them in the
	//     Instances page already; a data-plane caller holding a status grant is
	//     owed the health of its backends, not the addresses they live at.
	//     AdminAuth protects the admin /metrics on the same grounds.
	//   - Transport failures are described in full. "upstream unavailable" is
	//     all a tenant needs; an operator working out why a replica is
	//     unreachable needs to know whether it was DNS, a refused connection or
	//     a certificate.
	//
	// An earlier version of this called itself RevealTargetURLs and keyed only
	// the second. That left the admin listener applying data-plane instance
	// scoping against the empty tenant set AdminAuth puts in the context, so
	// every tenant-dedicated instance answered 404 to the admin UI.
	AdminPlane bool
}

// RegisterOperationalRoutes mounts one route per allowlisted endpoint for one
// backend:
//
//	GET {mount}/targets/{instance}/{alias}
//
// "targets" is a literal second segment rather than the instance name, so that
// none of these patterns holds a wildcard where a backend's own API holds a
// literal. With the instance first, GET /prometheus/{instance}/targets/ready
// and Grafana's ruler spelling GET /prometheus/rules/{namespace}/{groupName}
// both match /prometheus/rules/targets/ready and net/http refuses to register
// the pair at all.
//
// Both listeners call this, with the same table, so the data plane and the
// admin UI address these endpoints identically and there is no second list to
// keep in step.
//
// The instance is named in the path rather than resolved from the caller's
// grants the way every data API route is. That is not an inconsistency here:
// instance selection elsewhere picks the instance holding a tenant's data, and
// these endpoints hold no tenant's data. What the grant still decides is which
// instances the caller may address at all -- see operationalInstance.
func RegisterOperationalRoutes(mux Registrar, backend string, h *config.ConfigHolder, p OperationalFetcher, opts OperationalOptions) {
	mount, ok := BackendMounts[backend]
	if !ok {
		return
	}
	for _, endpoint := range operationalEndpoints[backend] {
		endpoint := endpoint
		mux.HandleFunc("GET "+mount+"/targets/{instance}/"+endpoint.Alias, func(w http.ResponseWriter, r *http.Request) {
			serveOperational(w, r, h, p, backend, endpoint, opts)
		})
	}
}

// TargetResultDoc is one target's answer.
type TargetResultDoc struct {
	Rank int `json:"rank"`
	// URL is present only when the caller is entitled to see it; see
	// OperationalOptions.AdminPlane.
	URL         string `json:"url,omitempty"`
	Status      int    `json:"status,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	DurationMS  int64  `json:"duration_ms"`
	Body        string `json:"body,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
	// Error is set when the target could not be reached at all. A target that
	// answered 503 is not an error: it answered, and Status says what it said.
	Error string `json:"error,omitempty"`
}

// OperationalDoc is the whole answer: one entry per configured target, in
// configuration order.
type OperationalDoc struct {
	Instance string            `json:"instance"`
	Backend  string            `json:"backend"`
	Endpoint string            `json:"endpoint"`
	Targets  []TargetResultDoc `json:"targets"`
}

func serveOperational(w http.ResponseWriter, r *http.Request, h *config.ConfigHolder, p OperationalFetcher, backend string, endpoint OperationalEndpoint, opts OperationalOptions) {
	cfg := h.Get()
	if cfg == nil {
		proxy.WriteJSONError(w, http.StatusServiceUnavailable, map[string]string{"error": "no configuration loaded"})
		return
	}

	inst, ok := operationalInstance(w, r, cfg, backend, r.PathValue("instance"), opts)
	if !ok {
		return
	}

	targets := inst.GetPushTargets()
	results := make([]TargetResultDoc, len(targets))

	// Every target is asked at once. Serially, one hung target would delay the
	// answer for all the healthy ones, and a hung target is precisely what the
	// caller is trying to find out about.
	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Add(1)
		go func(i int, target config.PushTarget) {
			defer wg.Done()
			results[i] = fetchOneTarget(r, p, inst, target, i+1, endpoint, opts)
		}(i, target)
	}
	wg.Wait()

	// 200 means "the gateway asked every target", not "every target is well".
	// The per-target status is in the body, because a partial answer is the
	// normal and useful case: one replica down out of three is exactly what
	// this endpoint exists to show, and collapsing that into a single gateway
	// status code would throw away which one.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(OperationalDoc{
		Instance: inst.Name,
		Backend:  backend,
		Endpoint: endpoint.Alias,
		Targets:  results,
	})
}

func fetchOneTarget(r *http.Request, p OperationalFetcher, inst *config.InstanceConfig, target config.PushTarget, rank int, endpoint OperationalEndpoint, opts OperationalOptions) TargetResultDoc {
	out := TargetResultDoc{Rank: rank}
	if opts.AdminPlane {
		out.URL = target.URL
	}

	resp, err := p.FetchOperational(r.Context(), inst, target, endpoint.Alias, endpoint.Upstream, r.URL.RawQuery, r.Header, maxOperationalBodyBytes)
	if err != nil {
		out.Error = describeFetchError(err, opts.AdminPlane)
		return out
	}
	out.Status = resp.StatusCode
	out.ContentType = resp.ContentType
	out.DurationMS = resp.Duration.Milliseconds()
	out.Body = string(resp.Body)
	out.Truncated = resp.Truncated
	return out
}

// describeFetchError turns a transport failure into text for the response.
//
// A data-plane caller gets the same two phrases the rest of the gateway uses,
// because the underlying error names the upstream URL and often resolves its
// address, which is exactly what the data plane withholds. An operator gets the
// error itself: "upstream unavailable" does not distinguish a name that does
// not resolve from a port that refuses from a certificate that does not verify,
// and telling those apart is the whole reason to look.
func describeFetchError(err error, adminPlane bool) string {
	if adminPlane {
		return err.Error()
	}
	if proxy.IsTimeout(err) {
		return "upstream timeout"
	}
	return "upstream unavailable"
}

// operationalInstance resolves the named instance and decides whether this
// caller may address it.
//
// The check is access, not scoping. An instance dedicated to tenants is one
// those tenants' callers may look at; an instance with no tenant configured on
// any target is shared and any holder of the action may look at it. What does
// not happen either way is the tenant assertion: nothing here writes to
// ra.TenantIDs, and the forwarder cannot send an X-Scope-OrgID at all.
//
// A caller whose grant does not cover the instance gets 404 rather than 403, so
// that the route cannot be used to enumerate which instance names exist.
func operationalInstance(w http.ResponseWriter, r *http.Request, cfg *config.Config, backend, name string, opts OperationalOptions) (*config.InstanceConfig, bool) {
	var inst *config.InstanceConfig
	for _, candidate := range cfg.Instances {
		if candidate.Name == name && candidate.Backend == backend {
			inst = candidate
			break
		}
	}
	if inst == nil {
		proxy.WriteJSONError(w, http.StatusNotFound, map[string]string{"error": "instance not found"})
		return nil, false
	}

	// The admin listener authorized the caller through AdminAuth, which is a
	// stronger statement than any grant checked below. Note that it does put a
	// RequestAuth in the context -- one with no tenant IDs -- so this cannot be
	// inferred from auth.FromContext returning nil.
	if opts.AdminPlane {
		return inst, true
	}
	ra := auth.FromContext(r.Context())
	if ra == nil {
		return inst, true
	}

	wanted := instanceTenantIDs(inst)
	if len(wanted) == 0 {
		return inst, true
	}
	allowed := make(map[string]struct{}, len(ra.TenantIDs))
	for _, id := range ra.TenantIDs {
		allowed[id] = struct{}{}
	}
	for _, id := range wanted {
		if _, ok := allowed[id]; !ok {
			proxy.WriteJSONError(w, http.StatusNotFound, map[string]string{"error": "instance not found"})
			return nil, false
		}
	}
	return inst, true
}

// instanceTenantIDs is the set of tenants an instance is dedicated to, across
// every target. It differs from targetTenantIDs in taking the union over all
// targets regardless of direction: an operational endpoint is a property of the
// whole instance, so a caller who may not see one of its targets may not see
// any of it.
func instanceTenantIDs(inst *config.InstanceConfig) []string {
	seen := make(map[string]struct{})
	var ids []string
	for _, target := range inst.GetPushTargets() {
		if target.TenantID == "" {
			continue
		}
		if _, dup := seen[target.TenantID]; dup {
			continue
		}
		seen[target.TenantID] = struct{}{}
		ids = append(ids, target.TenantID)
	}
	return ids
}
