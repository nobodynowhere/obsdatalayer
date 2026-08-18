package fanout

import (
	"errors"
	"net/http"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/metrics"
	"obsdatalayer/internal/proxy"
	"obsdatalayer/internal/rewrite"
)

// RegisterMimir registers all Mimir routes on mux.
func RegisterMimir(mux *http.ServeMux, h *config.ConfigHolder, p *proxy.Proxy, m *metrics.Metrics) {
	mux.HandleFunc("POST /api/{instance}/mimir/push", func(w http.ResponseWriter, r *http.Request) {
		inst := getInstance(h, r, w, "mimir")
		if inst == nil {
			return
		}
		maxBytes := h.Get().Gateway.MaxBodyBytes
		handlePush(w, r, inst, "/api/v1/push", func(body []byte) ([]byte, error) {
			return rewrite.RewriteMimir(body, inst.Labels)
		}, maxBytes, p, m)
	})

	mux.HandleFunc("GET /api/{instance}/mimir/query_range", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "mimir"); inst != nil {
			forwardMimirRead(w, r, inst, "query_range", "/prometheus/api/v1/query_range", p)
		}
	})
	mux.HandleFunc("GET /api/{instance}/mimir/query", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "mimir"); inst != nil {
			forwardMimirRead(w, r, inst, "query", "/prometheus/api/v1/query", p)
		}
	})
	mux.HandleFunc("GET /api/{instance}/mimir/labels", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "mimir"); inst != nil {
			forwardMimirRead(w, r, inst, "labels", "/prometheus/api/v1/labels", p)
		}
	})
	mux.HandleFunc("GET /api/{instance}/mimir/label/{name}/values", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "mimir"); inst != nil {
			forwardMimirRead(w, r, inst, "label_values", "/prometheus/api/v1/label/"+r.PathValue("name")+"/values", p)
		}
	})
	mux.HandleFunc("GET /api/{instance}/mimir/series", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "mimir"); inst != nil {
			forwardMimirRead(w, r, inst, "series", "/prometheus/api/v1/series", p)
		}
	})
	mux.HandleFunc("GET /api/{instance}/mimir/metadata", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "mimir"); inst != nil {
			forwardMimirRead(w, r, inst, "metadata", "/prometheus/api/v1/metadata", p)
		}
	})
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
