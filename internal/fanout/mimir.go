package fanout

import (
	"net/http"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/metrics"
	"obsdatalayer/internal/proxy"
	"obsdatalayer/internal/rewrite"
)

// RegisterMimir registers all Mimir routes on mux.
func RegisterMimir(mux *http.ServeMux, h *config.ConfigHolder, p *proxy.Proxy, client *http.Client, m *metrics.Metrics) {
	mux.HandleFunc("POST /api/{instance}/mimir/push", func(w http.ResponseWriter, r *http.Request) {
		inst := getInstance(h, r, w, "mimir")
		if inst == nil {
			return
		}
		maxBytes := h.Get().Gateway.MaxBodyBytes
		handlePush(w, r, inst, "/api/v1/push", func(body []byte) ([]byte, error) {
			return rewrite.RewriteMimir(body, inst.Labels)
		}, maxBytes, client, m)
	})

	mux.HandleFunc("GET /api/{instance}/mimir/query_range", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "mimir"); inst != nil {
			p.ForwardQuery(w, r, inst, "/prometheus/api/v1/query_range")
		}
	})
	mux.HandleFunc("GET /api/{instance}/mimir/query", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "mimir"); inst != nil {
			p.ForwardQuery(w, r, inst, "/prometheus/api/v1/query")
		}
	})
	mux.HandleFunc("GET /api/{instance}/mimir/labels", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "mimir"); inst != nil {
			p.ForwardQuery(w, r, inst, "/prometheus/api/v1/labels")
		}
	})
	mux.HandleFunc("GET /api/{instance}/mimir/label/{name}/values", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "mimir"); inst != nil {
			p.ForwardQuery(w, r, inst, "/prometheus/api/v1/label/"+r.PathValue("name")+"/values")
		}
	})
	mux.HandleFunc("GET /api/{instance}/mimir/series", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "mimir"); inst != nil {
			p.ForwardQuery(w, r, inst, "/prometheus/api/v1/series")
		}
	})
	mux.HandleFunc("GET /api/{instance}/mimir/metadata", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "mimir"); inst != nil {
			p.ForwardQuery(w, r, inst, "/prometheus/api/v1/metadata")
		}
	})
}
