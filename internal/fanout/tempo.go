package fanout

import (
	"net/http"
	"strings"

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
func TempoDSRoutes(mux *http.ServeMux, mount string, h *config.ConfigHolder, p *proxy.Proxy) {
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
