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

// RegisterMimir registers all Mimir routes on mux.
func RegisterMimir(mux *http.ServeMux, h *config.ConfigHolder, p *proxy.Proxy, m *metrics.Metrics) {
	mux.HandleFunc("POST /api/mimir/push", func(w http.ResponseWriter, r *http.Request) {
		forwardMimirPush(w, r, h, p, m, "/api/v1/push", true)
	})
	mux.HandleFunc("POST /api/mimir/api/v1/push", func(w http.ResponseWriter, r *http.Request) {
		forwardMimirPush(w, r, h, p, m, "/api/v1/push", true)
	})
	mux.HandleFunc("POST /api/mimir/otlp/v1/metrics", func(w http.ResponseWriter, r *http.Request) {
		forwardMimirPush(w, r, h, p, m, "/otlp/v1/metrics", false)
	})
	mux.HandleFunc("POST /api/mimir/api/v1/push/influx/write", func(w http.ResponseWriter, r *http.Request) {
		forwardMimirPush(w, r, h, p, m, "/api/v1/push/influx/write", false)
	})

	registerMimirRead(mux, "GET /api/mimir/query_range", h, p, "query_range", "/prometheus/api/v1/query_range")
	registerMimirRead(mux, "POST /api/mimir/query_range", h, p, "query_range", "/prometheus/api/v1/query_range")
	registerMimirRead(mux, "GET /api/mimir/query", h, p, "query", "/prometheus/api/v1/query")
	registerMimirRead(mux, "POST /api/mimir/query", h, p, "query", "/prometheus/api/v1/query")
	registerMimirRead(mux, "GET /api/mimir/query_exemplars", h, p, "query_exemplars", "/prometheus/api/v1/query_exemplars")
	registerMimirRead(mux, "POST /api/mimir/query_exemplars", h, p, "query_exemplars", "/prometheus/api/v1/query_exemplars")
	registerMimirRead(mux, "GET /api/mimir/labels", h, p, "labels", "/prometheus/api/v1/labels")
	registerMimirRead(mux, "POST /api/mimir/labels", h, p, "labels", "/prometheus/api/v1/labels")
	registerMimirRead(mux, "GET /api/mimir/label/{name}/values", h, p, "label_values", "/prometheus/api/v1/label/{name}/values")
	registerMimirRead(mux, "POST /api/mimir/label/{name}/values", h, p, "label_values", "/prometheus/api/v1/label/{name}/values")
	registerMimirRead(mux, "GET /api/mimir/series", h, p, "series", "/prometheus/api/v1/series")
	registerMimirRead(mux, "POST /api/mimir/series", h, p, "series", "/prometheus/api/v1/series")
	registerMimirRead(mux, "GET /api/mimir/metadata", h, p, "metadata", "/prometheus/api/v1/metadata")
	registerMimirRead(mux, "POST /api/mimir/metadata", h, p, "metadata", "/prometheus/api/v1/metadata")
	registerMimirRead(mux, "POST /api/mimir/read", h, p, "read", "/prometheus/api/v1/read")
	registerMimirRead(mux, "GET /api/mimir/cardinality/active_series", h, p, "cardinality", "/prometheus/api/v1/cardinality/active_series")
	registerMimirRead(mux, "POST /api/mimir/cardinality/active_series", h, p, "cardinality", "/prometheus/api/v1/cardinality/active_series")
	registerMimirRead(mux, "GET /api/mimir/cardinality/label_names", h, p, "cardinality", "/prometheus/api/v1/cardinality/label_names")
	registerMimirRead(mux, "POST /api/mimir/cardinality/label_names", h, p, "cardinality", "/prometheus/api/v1/cardinality/label_names")
	registerMimirRead(mux, "GET /api/mimir/cardinality/label_values", h, p, "cardinality", "/prometheus/api/v1/cardinality/label_values")
	registerMimirRead(mux, "POST /api/mimir/cardinality/label_values", h, p, "cardinality", "/prometheus/api/v1/cardinality/label_values")
	registerMimirRead(mux, "GET /api/mimir/status/buildinfo", h, p, "", "/prometheus/api/v1/status/buildinfo")
	registerMimirRead(mux, "GET /api/mimir/format_query", h, p, "", "/prometheus/api/v1/format_query")
	registerMimirRead(mux, "POST /api/mimir/format_query", h, p, "", "/prometheus/api/v1/format_query")

	registerMimirPrometheusRoutes(mux, "/api/mimir/prometheus", h, p)
	registerMimirPrometheusRoutes(mux, "/prometheus", h, p)
	registerMimirTenantConfig(mux, "GET /api/mimir/config/v1/rules", h, p, "/prometheus/config/v1/rules")
	registerMimirTenantConfig(mux, "GET /api/mimir/config/v1/rules/{namespace}", h, p, "/prometheus/config/v1/rules/{namespace}")
	registerMimirTenantConfig(mux, "GET /api/mimir/config/v1/rules/{namespace}/{groupName}", h, p, "/prometheus/config/v1/rules/{namespace}/{groupName}")
	registerMimirTenantConfig(mux, "POST /api/mimir/config/v1/rules/{namespace}", h, p, "/prometheus/config/v1/rules/{namespace}")
	registerMimirTenantConfig(mux, "DELETE /api/mimir/config/v1/rules/{namespace}/{groupName}", h, p, "/prometheus/config/v1/rules/{namespace}/{groupName}")
	registerMimirTenantConfig(mux, "DELETE /api/mimir/config/v1/rules/{namespace}", h, p, "/prometheus/config/v1/rules/{namespace}")

	registerMimirTenantConfig(mux, "GET /api/mimir/api/v1/alerts", h, p, "/api/v1/alerts")
	registerMimirTenantConfig(mux, "POST /api/mimir/api/v1/alerts", h, p, "/api/v1/alerts")
	registerMimirTenantConfig(mux, "DELETE /api/mimir/api/v1/alerts", h, p, "/api/v1/alerts")
	registerMimirTenantConfig(mux, "GET /api/mimir/alertmanager/api/v1/alerts", h, p, "/api/v1/alerts")
	registerMimirTenantConfig(mux, "POST /api/mimir/alertmanager/api/v1/alerts", h, p, "/api/v1/alerts")
	registerMimirTenantConfig(mux, "DELETE /api/mimir/alertmanager/api/v1/alerts", h, p, "/api/v1/alerts")
}

func registerMimirPrometheusRoutes(mux *http.ServeMux, prefix string, h *config.ConfigHolder, p *proxy.Proxy) {
	registerMimirRead(mux, "GET "+prefix+"/api/v1/query_range", h, p, "query_range", "/prometheus/api/v1/query_range")
	registerMimirRead(mux, "POST "+prefix+"/api/v1/query_range", h, p, "query_range", "/prometheus/api/v1/query_range")
	registerMimirRead(mux, "GET "+prefix+"/api/v1/query", h, p, "query", "/prometheus/api/v1/query")
	registerMimirRead(mux, "POST "+prefix+"/api/v1/query", h, p, "query", "/prometheus/api/v1/query")
	registerMimirRead(mux, "GET "+prefix+"/api/v1/query_exemplars", h, p, "query_exemplars", "/prometheus/api/v1/query_exemplars")
	registerMimirRead(mux, "POST "+prefix+"/api/v1/query_exemplars", h, p, "query_exemplars", "/prometheus/api/v1/query_exemplars")
	registerMimirRead(mux, "GET "+prefix+"/api/v1/labels", h, p, "labels", "/prometheus/api/v1/labels")
	registerMimirRead(mux, "POST "+prefix+"/api/v1/labels", h, p, "labels", "/prometheus/api/v1/labels")
	registerMimirRead(mux, "GET "+prefix+"/api/v1/label/{name}/values", h, p, "label_values", "/prometheus/api/v1/label/{name}/values")
	registerMimirRead(mux, "POST "+prefix+"/api/v1/label/{name}/values", h, p, "label_values", "/prometheus/api/v1/label/{name}/values")
	registerMimirRead(mux, "GET "+prefix+"/api/v1/series", h, p, "series", "/prometheus/api/v1/series")
	registerMimirRead(mux, "POST "+prefix+"/api/v1/series", h, p, "series", "/prometheus/api/v1/series")
	registerMimirRead(mux, "GET "+prefix+"/api/v1/metadata", h, p, "metadata", "/prometheus/api/v1/metadata")
	registerMimirRead(mux, "POST "+prefix+"/api/v1/metadata", h, p, "metadata", "/prometheus/api/v1/metadata")
	registerMimirRead(mux, "POST "+prefix+"/api/v1/read", h, p, "read", "/prometheus/api/v1/read")
	registerMimirRead(mux, "GET "+prefix+"/api/v1/cardinality/active_series", h, p, "cardinality", "/prometheus/api/v1/cardinality/active_series")
	registerMimirRead(mux, "POST "+prefix+"/api/v1/cardinality/active_series", h, p, "cardinality", "/prometheus/api/v1/cardinality/active_series")
	registerMimirRead(mux, "GET "+prefix+"/api/v1/cardinality/label_names", h, p, "cardinality", "/prometheus/api/v1/cardinality/label_names")
	registerMimirRead(mux, "POST "+prefix+"/api/v1/cardinality/label_names", h, p, "cardinality", "/prometheus/api/v1/cardinality/label_names")
	registerMimirRead(mux, "GET "+prefix+"/api/v1/cardinality/label_values", h, p, "cardinality", "/prometheus/api/v1/cardinality/label_values")
	registerMimirRead(mux, "POST "+prefix+"/api/v1/cardinality/label_values", h, p, "cardinality", "/prometheus/api/v1/cardinality/label_values")
	registerMimirRead(mux, "GET "+prefix+"/api/v1/status/buildinfo", h, p, "", "/prometheus/api/v1/status/buildinfo")
	registerMimirRead(mux, "GET "+prefix+"/api/v1/format_query", h, p, "", "/prometheus/api/v1/format_query")
	registerMimirRead(mux, "POST "+prefix+"/api/v1/format_query", h, p, "", "/prometheus/api/v1/format_query")
	registerMimirRead(mux, "GET "+prefix+"/api/v1/rules", h, p, "", "/prometheus/api/v1/rules")
	registerMimirRead(mux, "GET "+prefix+"/api/v1/alerts", h, p, "", "/prometheus/api/v1/alerts")

	registerMimirTenantConfig(mux, "GET "+prefix+"/config/v1/rules", h, p, "/prometheus/config/v1/rules")
	registerMimirTenantConfig(mux, "GET "+prefix+"/config/v1/rules/{namespace}", h, p, "/prometheus/config/v1/rules/{namespace}")
	registerMimirTenantConfig(mux, "GET "+prefix+"/config/v1/rules/{namespace}/{groupName}", h, p, "/prometheus/config/v1/rules/{namespace}/{groupName}")
	registerMimirTenantConfig(mux, "POST "+prefix+"/config/v1/rules/{namespace}", h, p, "/prometheus/config/v1/rules/{namespace}")
	registerMimirTenantConfig(mux, "DELETE "+prefix+"/config/v1/rules/{namespace}/{groupName}", h, p, "/prometheus/config/v1/rules/{namespace}/{groupName}")
	registerMimirTenantConfig(mux, "DELETE "+prefix+"/config/v1/rules/{namespace}", h, p, "/prometheus/config/v1/rules/{namespace}")
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
		if inst := getInstance(h, r, w, "mimir"); inst != nil && requireSingleTenant(w, r) {
			forwardByMethod(w, r, inst, expandMimirPath(upstreamPath, r), h.Get().Gateway.MaxBodyBytes, p)
		}
	})
}

func expandMimirPath(path string, r *http.Request) string {
	path = strings.ReplaceAll(path, "{name}", escapedPathValue(r, "name"))
	path = strings.ReplaceAll(path, "{namespace}", escapedPathValue(r, "namespace"))
	path = strings.ReplaceAll(path, "{groupName}", escapedPathValue(r, "groupName"))
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
