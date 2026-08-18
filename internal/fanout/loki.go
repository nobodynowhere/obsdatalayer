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

// RegisterLoki registers all Loki routes on mux.
func RegisterLoki(mux *http.ServeMux, h *config.ConfigHolder, p *proxy.Proxy, m *metrics.Metrics) {
	mux.HandleFunc("POST /api/loki/push", func(w http.ResponseWriter, r *http.Request) {
		forwardLokiPush(w, r, h, p, m, "/loki/api/v1/push", true)
	})
	mux.HandleFunc("POST /api/loki/loki/api/v1/push", func(w http.ResponseWriter, r *http.Request) {
		forwardLokiPush(w, r, h, p, m, "/loki/api/v1/push", true)
	})
	mux.HandleFunc("POST /api/loki/otlp/v1/logs", func(w http.ResponseWriter, r *http.Request) {
		forwardLokiPush(w, r, h, p, m, "/otlp/v1/logs", false)
	})

	registerLokiQuery(mux, "GET /api/loki/query_range", h, p, "/loki/api/v1/query_range")
	registerLokiQuery(mux, "GET /api/loki/query", h, p, "/loki/api/v1/query")
	registerLokiQuery(mux, "GET /api/loki/labels", h, p, "/loki/api/v1/labels")
	registerLokiQuery(mux, "GET /api/loki/label/{name}/values", h, p, "/loki/api/v1/label/{name}/values")
	registerLokiQuery(mux, "GET /api/loki/series", h, p, "/loki/api/v1/series")
	registerLokiQuery(mux, "GET /api/loki/index/stats", h, p, "/loki/api/v1/index/stats")
	registerLokiQuery(mux, "GET /api/loki/index/volume", h, p, "/loki/api/v1/index/volume")
	registerLokiQuery(mux, "GET /api/loki/index/volume_range", h, p, "/loki/api/v1/index/volume_range")
	registerLokiQuery(mux, "GET /api/loki/patterns", h, p, "/loki/api/v1/patterns")
	registerLokiQuery(mux, "GET /api/loki/format_query", h, p, "/loki/api/v1/format_query")
	registerLokiQuery(mux, "GET /api/loki/status/buildinfo", h, p, "/loki/api/v1/status/buildinfo")

	registerLokiTenantConfig(mux, "GET /api/loki/rules", h, p, "/loki/api/v1/rules")
	registerLokiTenantConfig(mux, "GET /api/loki/rules/{namespace}", h, p, "/loki/api/v1/rules/{namespace}")
	registerLokiTenantConfig(mux, "GET /api/loki/rules/{namespace}/{groupName}", h, p, "/loki/api/v1/rules/{namespace}/{groupName}")
	registerLokiTenantConfig(mux, "POST /api/loki/rules/{namespace}", h, p, "/loki/api/v1/rules/{namespace}")
	registerLokiTenantConfig(mux, "DELETE /api/loki/rules/{namespace}/{groupName}", h, p, "/loki/api/v1/rules/{namespace}/{groupName}")
	registerLokiTenantConfig(mux, "DELETE /api/loki/rules/{namespace}", h, p, "/loki/api/v1/rules/{namespace}")
	registerLokiQuery(mux, "GET /api/loki/prometheus/api/v1/rules", h, p, "/prometheus/api/v1/rules")
	registerLokiQuery(mux, "GET /api/loki/prometheus/api/v1/alerts", h, p, "/prometheus/api/v1/alerts")
}

func forwardLokiPush(w http.ResponseWriter, r *http.Request, h *config.ConfigHolder, p *proxy.Proxy, m *metrics.Metrics, upstreamPath string, rewriteLabels bool) {
	inst := getInstance(h, r, w, "loki")
	if inst == nil {
		return
	}
	ct := r.Header.Get("Content-Type")
	maxBytes := h.Get().Gateway.MaxBodyBytes
	var rewriteFn func([]byte) ([]byte, error)
	if rewriteLabels {
		rewriteFn = func(body []byte) ([]byte, error) {
			return rewrite.RewriteLoki(ct, body, inst.Labels)
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
