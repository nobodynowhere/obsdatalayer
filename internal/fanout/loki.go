package fanout

import (
	"net/http"
	"net/url"
	"strings"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/metrics"
	"obsdatalayer/internal/proxy"
	"obsdatalayer/internal/rewrite"
)

// LokiDSRoutes registers the routes a Grafana Loki data source addresses.
//
// Data source URL: gateway:port/loki
//
// Grafana joins its own path onto the base URL's path, so a base of
// gateway:port/loki produces gateway:port/loki/loki/api/v1/query_range. The
// doubling is correct: the mount is a gateway concept and /loki/api/v1 is
// genuinely part of Loki's own paths.
//
// Covered: the query API, the ruler configuration API in both spellings Loki
// serves (its own and the legacy /api/prom one), and the Prometheus-compatible
// rule and alert state listings.
//
// Not implemented:
//
//   - The deprecated /api/prom query aliases (query, label, series, tail).
//
// Ingestion is a separate bundle; see IngestRoutes.
func LokiDSRoutes(mux *http.ServeMux, mount string, h *config.ConfigHolder, p *proxy.Proxy) {
	// Ingestion is a separate namespace; POST /loki/api/v1/push is registered
	// at Loki's exact upstream path by IngestRoutes, not under a mount.

	// The POST variants are genuine reads: a LogQL selector can outgrow a
	// practical URL, so Loki accepts these as form posts.
	// middleware.isQueryPost classifies them as reads for authorization.
	read := func(method, upstream string) {
		registerLokiQuery(mux, method+" "+mount+upstream, h, p, upstream)
	}
	read("GET", "/loki/api/v1/query")
	read("GET", "/loki/api/v1/query_range")
	read("GET", "/loki/api/v1/labels")
	read("GET", "/loki/api/v1/label/{name}/values")
	read("GET", "/loki/api/v1/series")
	read("POST", "/loki/api/v1/series")
	read("GET", "/loki/api/v1/index/stats")
	read("GET", "/loki/api/v1/index/volume")
	read("GET", "/loki/api/v1/index/volume_range")
	read("GET", "/loki/api/v1/patterns")
	read("GET", "/loki/api/v1/detected_fields")
	read("POST", "/loki/api/v1/detected_fields")
	read("GET", "/loki/api/v1/detected_field/{name}/values")
	read("POST", "/loki/api/v1/detected_field/{name}/values")
	read("GET", "/loki/api/v1/format_query")
	read("POST", "/loki/api/v1/format_query")
	read("GET", "/loki/api/v1/status/buildinfo")

	// Log deletion. Tenant-scoped and irreversible, so it sits behind its own
	// delete:read / delete:write actions rather than the ordinary write grant:
	// a log shipper needs write to ship, and must not thereby be able to delete
	// what it shipped. Single-tenant in both directions.
	for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
		mux.HandleFunc(method+" "+mount+"/loki/api/v1/delete", func(w http.ResponseWriter, r *http.Request) {
			if inst := getInstance(h, r, w, "loki"); inst != nil && requireSingleTenant(w, r) {
				forwardByMethod(w, r, inst, "/loki/api/v1/delete", h.Get().Gateway.MaxBodyBytes, p)
			}
		})
	}

	// Live tail. A WebSocket, so it takes the upgrade-aware forwarding path
	// rather than the buffering one. Single-tenant: Loki answers 400 to a tail
	// whose X-Scope-OrgID names more than one tenant.
	mux.HandleFunc("GET "+mount+"/loki/api/v1/tail", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "loki"); inst != nil && requireSingleTenant(w, r) {
			p.ForwardUpgrade(w, r, inst, "/loki/api/v1/tail")
		}
	})

	// Ruler configuration, in both spellings Loki serves. Grafana's data
	// source-managed alert rules use the legacy /api/prom form.
	registerLokiRuleRoutes(mux, mount+"/loki/api/v1/rules", h, p)
	registerLokiRuleRoutes(mux, mount+"/api/prom/rules", h, p)

	// Rule and alert state. Ruler endpoints, not query endpoints, so a
	// multi-tenant grant is refused rather than pipe-joined -- Loki honours a
	// joined X-Scope-OrgID on query endpoints only. Under the mount these no
	// longer compete with Mimir's identical paths at the gateway root.
	registerLokiTenantConfig(mux, "GET "+mount+"/prometheus/api/v1/rules", h, p, "/prometheus/api/v1/rules")
	registerLokiTenantConfig(mux, "GET "+mount+"/prometheus/api/v1/alerts", h, p, "/prometheus/api/v1/alerts")
}

// registerLokiRuleRoutes registers the six ruler configuration routes under
// prefix. Every spelling is normalized onto Loki's canonical upstream path, so
// the backend sees one form regardless of which alias the client used.
func registerLokiRuleRoutes(mux *http.ServeMux, prefix string, h *config.ConfigHolder, p *proxy.Proxy) {
	registerLokiTenantConfig(mux, "GET "+prefix, h, p, "/loki/api/v1/rules")
	registerLokiTenantConfig(mux, "GET "+prefix+"/{namespace}", h, p, "/loki/api/v1/rules/{namespace}")
	registerLokiTenantConfig(mux, "GET "+prefix+"/{namespace}/{groupName}", h, p, "/loki/api/v1/rules/{namespace}/{groupName}")
	registerLokiTenantConfig(mux, "POST "+prefix+"/{namespace}", h, p, "/loki/api/v1/rules/{namespace}")
	registerLokiTenantConfig(mux, "DELETE "+prefix+"/{namespace}/{groupName}", h, p, "/loki/api/v1/rules/{namespace}/{groupName}")
	registerLokiTenantConfig(mux, "DELETE "+prefix+"/{namespace}", h, p, "/loki/api/v1/rules/{namespace}")
}

func forwardLokiPush(w http.ResponseWriter, r *http.Request, h *config.ConfigHolder, p *proxy.Proxy, m *metrics.Metrics, upstreamPath string, rewriteLabels bool) {
	inst := getInstance(h, r, w, "loki")
	if inst == nil {
		return
	}
	ct := r.Header.Get("Content-Type")
	maxBytes := h.Get().Gateway.MaxBodyBytes
	var rewriteFn func([]byte) ([]byte, rewrite.PayloadStats, error)
	if rewriteLabels {
		rewriteFn = func(body []byte) ([]byte, rewrite.PayloadStats, error) {
			return rewrite.RewriteLokiWithStats(ct, body, inst.Labels)
		}
	}
	handlePush(w, r, inst, upstreamPath, rewriteFn, maxBytes, p, m)
}

func registerLokiQuery(mux *http.ServeMux, pattern string, h *config.ConfigHolder, p *proxy.Proxy, upstreamPath string) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "loki"); inst != nil {
			p.ForwardQuery(w, r, inst, expandLokiPath(upstreamPath, r))
		}
	})
}

func registerLokiTenantConfig(mux *http.ServeMux, pattern string, h *config.ConfigHolder, p *proxy.Proxy, upstreamPath string) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "loki"); inst != nil && requireSingleTenant(w, r) {
			forwardByMethod(w, r, inst, expandLokiPath(upstreamPath, r), h.Get().Gateway.MaxBodyBytes, p)
		}
	})
}

func expandLokiPath(path string, r *http.Request) string {
	path = strings.ReplaceAll(path, "{name}", escapedLokiPathValue(r, "name"))
	path = strings.ReplaceAll(path, "{namespace}", escapedLokiPathValue(r, "namespace"))
	path = strings.ReplaceAll(path, "{groupName}", escapedLokiPathValue(r, "groupName"))
	return path
}

func escapedLokiPathValue(r *http.Request, name string) string {
	value := r.PathValue(name)
	if value == "" {
		return ""
	}
	return url.PathEscape(value)
}
