package fanout

import (
	"net/http"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/proxy"
)

// RegisterTempo registers all Tempo routes on mux.
func RegisterTempo(mux *http.ServeMux, h *config.ConfigHolder, p *proxy.Proxy) {
	mux.HandleFunc("POST /api/{instance}/tempo/otlp/v1/traces", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "tempo"); inst != nil {
			p.ForwardPush(w, r, inst, "/otlp/v1/traces", h.Get().Gateway.MaxBodyBytes)
		}
	})
	mux.HandleFunc("POST /api/{instance}/tempo/jaeger/v1/traces", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "tempo"); inst != nil {
			p.ForwardPush(w, r, inst, "/api/traces", h.Get().Gateway.MaxBodyBytes)
		}
	})
	mux.HandleFunc("GET /api/{instance}/tempo/search", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "tempo"); inst != nil {
			p.ForwardQuery(w, r, inst, "/api/search")
		}
	})
	mux.HandleFunc("GET /api/{instance}/tempo/traces/{traceID}", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "tempo"); inst != nil {
			p.ForwardQuery(w, r, inst, "/api/traces/"+r.PathValue("traceID"))
		}
	})
	mux.HandleFunc("GET /api/{instance}/tempo/v2/search/tags", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "tempo"); inst != nil {
			p.ForwardQuery(w, r, inst, "/api/v2/search/tags")
		}
	})
	mux.HandleFunc("GET /api/{instance}/tempo/v2/search/tag/{name}/values", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "tempo"); inst != nil {
			p.ForwardQuery(w, r, inst, "/api/v2/search/tag/"+r.PathValue("name")+"/values")
		}
	})
}
