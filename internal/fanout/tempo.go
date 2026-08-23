package fanout

import (
	"net/http"
	"strings"

	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/config"
	"obsdatalayer/internal/proxy"
)

// TempoDSRoutes registers the routes a Grafana Tempo data source addresses.
//
// Data source URL: gateway:port/tempo
//
// Grafana joins its own path onto the base URL's path, so the gateway serves
// the mount plus Tempo's exact query API. Tempo's API already lives under
// /api, so the mount simply prefixes it.
//
// Covered: trace lookup by ID (v1, v2), search, search tags (v1, v2), tag
// values (v1, v2), TraceQL metrics (range and instant), echo, build info, and
// the per-tenant overrides API.
//
// Not implemented:
//
//   - Nothing outstanding. Tempo's query surface is covered in full.
//
// Ingestion is a separate bundle; see IngestRoutes.
func TempoDSRoutes(mux Registrar, mount string, h *config.ConfigHolder, p *proxy.Proxy) {
	// Per-target operational endpoints; see operationalEndpoints.
	RegisterOperationalRoutes(mux, "tempo", h, p, OperationalOptions{})

	read := func(upstream string) {
		mux.HandleFunc("GET "+mount+upstream, func(w http.ResponseWriter, r *http.Request) {
			if inst := getInstance(h, r, w, "tempo"); inst != nil {
				p.ForwardQuery(w, r, inst, expandTempoPath(upstream, r))
			}
		})
	}
	read("/api/traces/{traceID}")
	read("/api/v2/traces/{traceID}")
	read("/api/search")
	read("/api/search/tags")
	read("/api/v2/search/tags")
	read("/api/search/tag/{name}/values")
	read("/api/v2/search/tag/{name}/values")
	read("/api/metrics/query_range")
	read("/api/metrics/query")
	read("/api/echo")
	read("/api/status/buildinfo")

	// Per-tenant overrides. Tempo serves all four methods on one path.
	for _, method := range []string{"GET", "POST", "PATCH", "DELETE"} {
		mux.HandleFunc(method+" "+mount+"/api/overrides", func(w http.ResponseWriter, r *http.Request) {
			if inst := getInstance(h, r, w, "tempo"); inst != nil && requireSingleTenant(w, r) {
				forwardByMethod(w, r, inst, "/api/overrides", h.Get().Gateway.MaxBodyBytes, p)
			}
		})
	}
}

func expandTempoPath(path string, r *http.Request) string {
	path = strings.ReplaceAll(path, "{traceID}", escapedLokiPathValue(r, "traceID"))
	path = strings.ReplaceAll(path, "{name}", escapedLokiPathValue(r, "name"))
	return path
}

// tempoOperationalEndpoints is what the /tempo mount serves beside Tempo's
// query API. Tempo registers these with bare handlers while its query routes go
// through base.Wrap(...), which is where its tenant middleware lives, so these
// are untenanted upstream and the gateway sends no X-Scope-OrgID.
//
// No legacy paths move: /tempo/api/echo stays on read, because it is what
// Grafana's Tempo data source uses for its health check.
var tempoOperationalEndpoints = []OperationalEndpoint{
	{Alias: "ready", Upstream: "/ready", Action: auth.ActionStatus},
	{Alias: "status", Upstream: "/status", Action: auth.ActionStatus},
	{Alias: "buildinfo", Upstream: "/api/status/buildinfo", Action: auth.ActionStatus},
	{Alias: "echo", Upstream: "/api/echo", Action: auth.ActionStatus},
	{Alias: "config", Upstream: "/status/config", Action: auth.ActionConfig},
	{Alias: "runtime_config", Upstream: "/status/runtime_config", Action: auth.ActionConfig},
	{Alias: "metrics", Upstream: "/metrics", Action: auth.ActionMetrics},
}

var tempoLegacyOperationalPaths = map[string]string{}
