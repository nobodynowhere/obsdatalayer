package fanout

import (
	"net/http"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/metrics"
	"obsdatalayer/internal/proxy"
	"obsdatalayer/internal/rewrite"
)

// RegisterLoki registers all Loki routes on mux.
func RegisterLoki(mux *http.ServeMux, h *config.ConfigHolder, p *proxy.Proxy, client *http.Client, m *metrics.Metrics) {
	mux.HandleFunc("POST /api/{instance}/loki/push", func(w http.ResponseWriter, r *http.Request) {
		inst := getInstance(h, r, w, "loki")
		if inst == nil {
			return
		}
		ct := r.Header.Get("Content-Type")
		maxBytes := h.Get().Gateway.MaxBodyBytes
		handlePush(w, r, inst, "/loki/api/v1/push", func(body []byte) ([]byte, error) {
			return rewrite.RewriteLoki(ct, body, inst.Labels)
		}, maxBytes, client, m)
	})

	mux.HandleFunc("GET /api/{instance}/loki/query_range", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "loki"); inst != nil {
			p.ForwardQuery(w, r, inst, "/loki/api/v1/query_range")
		}
	})
	mux.HandleFunc("GET /api/{instance}/loki/query", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "loki"); inst != nil {
			p.ForwardQuery(w, r, inst, "/loki/api/v1/query")
		}
	})
	mux.HandleFunc("GET /api/{instance}/loki/labels", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "loki"); inst != nil {
			p.ForwardQuery(w, r, inst, "/loki/api/v1/labels")
		}
	})
	mux.HandleFunc("GET /api/{instance}/loki/label/{name}/values", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "loki"); inst != nil {
			p.ForwardQuery(w, r, inst, "/loki/api/v1/label/"+r.PathValue("name")+"/values")
		}
	})
	mux.HandleFunc("GET /api/{instance}/loki/series", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "loki"); inst != nil {
			p.ForwardQuery(w, r, inst, "/loki/api/v1/series")
		}
	})
}
