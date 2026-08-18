package fanout

import (
	"errors"
	"net/http"
	"strings"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/metrics"
	"obsdatalayer/internal/proxy"
	"obsdatalayer/internal/rewrite"
)

// RegisterMimir registers all Mimir routes on mux.
func RegisterMimir(mux *http.ServeMux, h *config.ConfigHolder, p *proxy.Proxy, m *metrics.Metrics) {
	mux.HandleFunc("POST /api/mimir/push", func(w http.ResponseWriter, r *http.Request) {
		forwardMimirPush(w, r, h, p, m)
	})
	mux.HandleFunc("POST /api/mimir/api/v1/push", func(w http.ResponseWriter, r *http.Request) {
		forwardMimirPush(w, r, h, p, m)
	})

	registerMimirRead(mux, "GET /api/mimir/query_range", h, p, "query_range", "/prometheus/api/v1/query_range")
	registerMimirRead(mux, "POST /api/mimir/query_range", h, p, "query_range", "/prometheus/api/v1/query_range")
	registerMimirRead(mux, "GET /api/mimir/query", h, p, "query", "/prometheus/api/v1/query")
	registerMimirRead(mux, "POST /api/mimir/query", h, p, "query", "/prometheus/api/v1/query")
	registerMimirRead(mux, "GET /api/mimir/labels", h, p, "labels", "/prometheus/api/v1/labels")
	registerMimirRead(mux, "POST /api/mimir/labels", h, p, "labels", "/prometheus/api/v1/labels")
	registerMimirRead(mux, "GET /api/mimir/label/{name}/values", h, p, "label_values", "/prometheus/api/v1/label/{name}/values")
	registerMimirRead(mux, "POST /api/mimir/label/{name}/values", h, p, "label_values", "/prometheus/api/v1/label/{name}/values")
	registerMimirRead(mux, "GET /api/mimir/series", h, p, "series", "/prometheus/api/v1/series")
	registerMimirRead(mux, "POST /api/mimir/series", h, p, "series", "/prometheus/api/v1/series")
	registerMimirRead(mux, "GET /api/mimir/metadata", h, p, "metadata", "/prometheus/api/v1/metadata")
	registerMimirRead(mux, "POST /api/mimir/metadata", h, p, "metadata", "/prometheus/api/v1/metadata")

	registerMimirRead(mux, "GET /api/mimir/prometheus/api/v1/query_range", h, p, "query_range", "/prometheus/api/v1/query_range")
	registerMimirRead(mux, "POST /api/mimir/prometheus/api/v1/query_range", h, p, "query_range", "/prometheus/api/v1/query_range")
	registerMimirRead(mux, "GET /api/mimir/prometheus/api/v1/query", h, p, "query", "/prometheus/api/v1/query")
	registerMimirRead(mux, "POST /api/mimir/prometheus/api/v1/query", h, p, "query", "/prometheus/api/v1/query")
	registerMimirRead(mux, "GET /api/mimir/prometheus/api/v1/labels", h, p, "labels", "/prometheus/api/v1/labels")
	registerMimirRead(mux, "POST /api/mimir/prometheus/api/v1/labels", h, p, "labels", "/prometheus/api/v1/labels")
	registerMimirRead(mux, "GET /api/mimir/prometheus/api/v1/label/{name}/values", h, p, "label_values", "/prometheus/api/v1/label/{name}/values")
	registerMimirRead(mux, "POST /api/mimir/prometheus/api/v1/label/{name}/values", h, p, "label_values", "/prometheus/api/v1/label/{name}/values")
	registerMimirRead(mux, "GET /api/mimir/prometheus/api/v1/series", h, p, "series", "/prometheus/api/v1/series")
	registerMimirRead(mux, "POST /api/mimir/prometheus/api/v1/series", h, p, "series", "/prometheus/api/v1/series")
	registerMimirRead(mux, "GET /api/mimir/prometheus/api/v1/metadata", h, p, "metadata", "/prometheus/api/v1/metadata")
	registerMimirRead(mux, "POST /api/mimir/prometheus/api/v1/metadata", h, p, "metadata", "/prometheus/api/v1/metadata")
}

func forwardMimirPush(w http.ResponseWriter, r *http.Request, h *config.ConfigHolder, p *proxy.Proxy, m *metrics.Metrics) {
	inst := getInstance(h, r, w, "mimir")
	if inst == nil {
		return
	}
	maxBytes := h.Get().Gateway.MaxBodyBytes
	handlePush(w, r, inst, "/api/v1/push", func(body []byte) ([]byte, error) {
		return rewrite.RewriteMimir(body, inst.Labels)
	}, maxBytes, p, m)
}

func registerMimirRead(mux *http.ServeMux, pattern string, h *config.ConfigHolder, p *proxy.Proxy, endpoint, upstreamPath string) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "mimir"); inst != nil {
			forwardMimirRead(w, r, inst, endpoint, expandMimirPath(upstreamPath, r), p)
		}
	})
}

func expandMimirPath(path string, r *http.Request) string {
	return strings.ReplaceAll(path, "{name}", r.PathValue("name"))
}

func forwardMimirRead(w http.ResponseWriter, r *http.Request, inst *config.InstanceConfig, endpoint, upstreamPath string, p *proxy.Proxy) {
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
	p.ForwardQuery(w, r, inst, upstreamPath)
}
