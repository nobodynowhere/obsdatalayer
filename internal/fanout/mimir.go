package fanout

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/metrics"
	"obsdatalayer/internal/proxy"
	"obsdatalayer/internal/rewrite"
)

// MimirDSRoutes registers the routes a Grafana Prometheus data source
// addresses when it is pointed at Mimir.
//
// Data source URL: gateway:port/prometheus
//
// Mimir's own API prefix is also /prometheus, so the mount and the upstream
// prefix are the same string and nothing beneath the mount is rewritten. This
// is the only backend where the mapping is an identity.
//
// Covered: the Prometheus-compatible query API (instant, range, exemplars,
// series, labels, label values, metadata, remote read, format_query, build
// info, cardinality and the experimental search endpoints), the ruler
// configuration API, and rule and alert state.
//
// Not implemented:
//
//   - The Alertmanager, which is a separate Grafana data source and has its
//     own bundle; see AlertmanagerDSRoutes.
//   - Tenant limits and stats (/api/v1/user_limits, /api/v1/user_stats).
//   - Block upload (/api/v1/upload/block/...), which is an operator tool.
//
// Ingestion is a separate bundle; see IngestRoutes.
func MimirDSRoutes(mux *http.ServeMux, mount string, h *config.ConfigHolder, p *proxy.Proxy) {
	registerMimirRead(mux, "GET "+mount+"/api/v1/query_range", h, p, "query_range", "/prometheus/api/v1/query_range")
	registerMimirRead(mux, "POST "+mount+"/api/v1/query_range", h, p, "query_range", "/prometheus/api/v1/query_range")
	registerMimirRead(mux, "GET "+mount+"/api/v1/query", h, p, "query", "/prometheus/api/v1/query")
	registerMimirRead(mux, "POST "+mount+"/api/v1/query", h, p, "query", "/prometheus/api/v1/query")
	registerMimirRead(mux, "GET "+mount+"/api/v1/query_exemplars", h, p, "query_exemplars", "/prometheus/api/v1/query_exemplars")
	registerMimirRead(mux, "POST "+mount+"/api/v1/query_exemplars", h, p, "query_exemplars", "/prometheus/api/v1/query_exemplars")
	registerMimirRead(mux, "GET "+mount+"/api/v1/labels", h, p, "labels", "/prometheus/api/v1/labels")
	registerMimirRead(mux, "POST "+mount+"/api/v1/labels", h, p, "labels", "/prometheus/api/v1/labels")
	registerMimirRead(mux, "GET "+mount+"/api/v1/label/{name}/values", h, p, "label_values", "/prometheus/api/v1/label/{name}/values")
	registerMimirRead(mux, "POST "+mount+"/api/v1/label/{name}/values", h, p, "label_values", "/prometheus/api/v1/label/{name}/values")
	registerMimirRead(mux, "GET "+mount+"/api/v1/series", h, p, "series", "/prometheus/api/v1/series")
	registerMimirRead(mux, "POST "+mount+"/api/v1/series", h, p, "series", "/prometheus/api/v1/series")
	registerMimirRead(mux, "GET "+mount+"/api/v1/search/metric_names", h, p, "search", "/prometheus/api/v1/search/metric_names")
	registerMimirRead(mux, "POST "+mount+"/api/v1/search/metric_names", h, p, "search", "/prometheus/api/v1/search/metric_names")
	registerMimirRead(mux, "GET "+mount+"/api/v1/search/label_names", h, p, "search", "/prometheus/api/v1/search/label_names")
	registerMimirRead(mux, "POST "+mount+"/api/v1/search/label_names", h, p, "search", "/prometheus/api/v1/search/label_names")
	registerMimirRead(mux, "GET "+mount+"/api/v1/search/label_values", h, p, "search", "/prometheus/api/v1/search/label_values")
	registerMimirRead(mux, "POST "+mount+"/api/v1/search/label_values", h, p, "search", "/prometheus/api/v1/search/label_values")
	registerMimirRead(mux, "GET "+mount+"/api/v1/metadata", h, p, "metadata", "/prometheus/api/v1/metadata")
	registerMimirRead(mux, "POST "+mount+"/api/v1/metadata", h, p, "metadata", "/prometheus/api/v1/metadata")
	registerMimirRead(mux, "POST "+mount+"/api/v1/read", h, p, "read", "/prometheus/api/v1/read")
	registerMimirRead(mux, "GET "+mount+"/api/v1/cardinality/active_series", h, p, "cardinality", "/prometheus/api/v1/cardinality/active_series")
	registerMimirRead(mux, "POST "+mount+"/api/v1/cardinality/active_series", h, p, "cardinality", "/prometheus/api/v1/cardinality/active_series")
	registerMimirRead(mux, "GET "+mount+"/api/v1/cardinality/label_names", h, p, "cardinality", "/prometheus/api/v1/cardinality/label_names")
	registerMimirRead(mux, "POST "+mount+"/api/v1/cardinality/label_names", h, p, "cardinality", "/prometheus/api/v1/cardinality/label_names")
	registerMimirRead(mux, "GET "+mount+"/api/v1/cardinality/label_values", h, p, "cardinality", "/prometheus/api/v1/cardinality/label_values")
	registerMimirRead(mux, "POST "+mount+"/api/v1/cardinality/label_values", h, p, "cardinality", "/prometheus/api/v1/cardinality/label_values")
	registerMimirRead(mux, "GET "+mount+"/ready", h, p, "", "/ready")
	registerMimirRead(mux, "GET "+mount+"/api/v1/status/buildinfo", h, p, "", "/prometheus/api/v1/status/buildinfo")
	registerMimirRead(mux, "GET "+mount+"/api/v1/status/config", h, p, "", "/api/v1/status/config")
	registerMimirRead(mux, "GET "+mount+"/api/v1/status/flags", h, p, "", "/api/v1/status/flags")
	registerMimirRead(mux, "GET "+mount+"/api/v1/format_query", h, p, "", "/prometheus/api/v1/format_query")
	registerMimirRead(mux, "POST "+mount+"/api/v1/format_query", h, p, "", "/prometheus/api/v1/format_query")
	registerMimirRead(mux, "GET "+mount+"/api/v1/rules", h, p, "", "/prometheus/api/v1/rules")
	registerMimirRead(mux, "GET "+mount+"/api/v1/alerts", h, p, "", "/prometheus/api/v1/alerts")

	registerMimirTenantConfig(mux, "GET "+mount+"/config/v1/rules", h, p, "/prometheus/config/v1/rules")
	registerMimirTenantConfig(mux, "GET "+mount+"/config/v1/rules/{namespace}", h, p, "/prometheus/config/v1/rules/{namespace}")
	registerMimirTenantConfig(mux, "GET "+mount+"/config/v1/rules/{namespace}/{groupName}", h, p, "/prometheus/config/v1/rules/{namespace}/{groupName}")
	registerMimirTenantConfig(mux, "POST "+mount+"/config/v1/rules/{namespace}", h, p, "/prometheus/config/v1/rules/{namespace}")
	registerMimirTenantConfig(mux, "DELETE "+mount+"/config/v1/rules/{namespace}/{groupName}", h, p, "/prometheus/config/v1/rules/{namespace}/{groupName}")
	registerMimirTenantConfig(mux, "DELETE "+mount+"/config/v1/rules/{namespace}", h, p, "/prometheus/config/v1/rules/{namespace}")

	// The same ruler configuration API under Grafana's other spelling.
	//
	// Grafana's ruler client picks its prefix from the data source subtype:
	// "mimir" gives /config/v1/rules above, while "cortex", "prometheus" and
	// any unrecognised or absent value give /rules. Mimir itself serves only
	// the /config/v1 form, so these six are an alias rather than a passthrough
	// -- the one place the gateway deviates from "mount plus exact upstream
	// path", and it is deliberate: without them a data source that Grafana has
	// not been told is Mimir can list rules but not edit them, which reads as a
	// gateway fault rather than a data source setting.
	registerMimirTenantConfig(mux, "GET "+mount+"/rules", h, p, "/prometheus/config/v1/rules")
	registerMimirTenantConfig(mux, "GET "+mount+"/rules/{namespace}", h, p, "/prometheus/config/v1/rules/{namespace}")
	registerMimirTenantConfig(mux, "GET "+mount+"/rules/{namespace}/{groupName}", h, p, "/prometheus/config/v1/rules/{namespace}/{groupName}")
	registerMimirTenantConfig(mux, "POST "+mount+"/rules/{namespace}", h, p, "/prometheus/config/v1/rules/{namespace}")
	registerMimirTenantConfig(mux, "DELETE "+mount+"/rules/{namespace}/{groupName}", h, p, "/prometheus/config/v1/rules/{namespace}/{groupName}")
	registerMimirTenantConfig(mux, "DELETE "+mount+"/rules/{namespace}", h, p, "/prometheus/config/v1/rules/{namespace}")
}

func forwardMimirPush(w http.ResponseWriter, r *http.Request, h *config.ConfigHolder, p *proxy.Proxy, m *metrics.Metrics, upstreamPath string, rewriteLabels bool) {
	inst := getInstance(h, r, w, "mimir")
	if inst == nil {
		return
	}
	maxBytes := h.Get().Gateway.MaxBodyBytes
	var rewriteFn func([]byte) ([]byte, rewrite.PayloadStats, error)
	if rewriteLabels {
		rewriteFn = func(body []byte) ([]byte, rewrite.PayloadStats, error) {
			return rewrite.RewriteMimirWithStats(body, inst.Labels)
		}
	}
	handlePush(w, r, inst, upstreamPath, rewriteFn, maxBytes, p, m)
}

func registerMimirRead(mux *http.ServeMux, pattern string, h *config.ConfigHolder, p *proxy.Proxy, endpoint, upstreamPath string) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "mimir"); inst != nil {
			forwardMimirRead(w, r, inst, endpoint, expandMimirPath(upstreamPath, r), p)
		}
	})
}

func registerMimirTenantConfig(mux *http.ServeMux, pattern string, h *config.ConfigHolder, p *proxy.Proxy, upstreamPath string) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "mimir"); inst != nil && requireSingleTenantForWrite(w, r) {
			forwardByMethod(w, r, inst, expandMimirPath(upstreamPath, r), h.Get().Gateway.MaxBodyBytes, p)
		}
	})
}

func expandMimirPath(path string, r *http.Request) string {
	path = strings.ReplaceAll(path, "{name}", escapedPathValue(r, "name"))
	path = strings.ReplaceAll(path, "{namespace}", escapedPathValue(r, "namespace"))
	path = strings.ReplaceAll(path, "{groupName}", escapedPathValue(r, "groupName"))
	path = strings.ReplaceAll(path, "{silenceID}", escapedPathValue(r, "silenceID"))
	return path
}

func escapedPathValue(r *http.Request, name string) string {
	value := r.PathValue(name)
	if value == "" {
		return ""
	}
	return url.PathEscape(value)
}

func forwardMimirRead(w http.ResponseWriter, r *http.Request, inst *config.InstanceConfig, endpoint, upstreamPath string, p *proxy.Proxy) {
	if endpoint != "" {
		if err := rewrite.ApplyMimirReadPolicy(r, endpoint); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, rewrite.ErrReadPolicyUnsupported) || errors.Is(err, rewrite.ErrReadPolicyAmbiguous) {
				status = http.StatusForbidden
			}
			proxy.WriteJSONError(w, status, map[string]string{
				"error":  "mimir read policy rejected request",
				"detail": err.Error(),
			})
			return
		}
	}
	p.ForwardQuery(w, r, inst, upstreamPath)
}
