package fanout

import (
	"net/http"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/proxy"
)

// RegisterTempo registers all Tempo routes on mux.
func RegisterTempo(mux *http.ServeMux, h *config.ConfigHolder, p *proxy.Proxy) {
	mux.HandleFunc("POST /api/tempo/otlp/v1/traces", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "tempo"); inst != nil {
			p.ForwardPush(w, r, inst, "/otlp/v1/traces", h.Get().Gateway.MaxBodyBytes)
		}
	})
	mux.HandleFunc("POST /api/tempo/jaeger/v1/traces", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "tempo"); inst != nil {
			p.ForwardPush(w, r, inst, "/api/traces", h.Get().Gateway.MaxBodyBytes)
		}
	})
	mux.HandleFunc("GET /api/tempo/search", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "tempo"); inst != nil {
			p.ForwardQuery(w, r, inst, "/api/search")
		}
	})
	mux.HandleFunc("GET /api/tempo/search/tags", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "tempo"); inst != nil {
			p.ForwardQuery(w, r, inst, "/api/search/tags")
		}
	})
	mux.HandleFunc("GET /api/tempo/search/tag/{name}/values", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "tempo"); inst != nil {
			p.ForwardQuery(w, r, inst, "/api/search/tag/"+r.PathValue("name")+"/values")
		}
	})
	mux.HandleFunc("GET /api/tempo/traces/{traceID}", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "tempo"); inst != nil {
			p.ForwardQuery(w, r, inst, "/api/traces/"+r.PathValue("traceID"))
		}
	})
	mux.HandleFunc("GET /api/tempo/v2/traces/{traceID}", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "tempo"); inst != nil {
			p.ForwardQuery(w, r, inst, "/api/v2/traces/"+r.PathValue("traceID"))
		}
	})
	mux.HandleFunc("GET /api/tempo/v2/search/tags", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "tempo"); inst != nil {
			p.ForwardQuery(w, r, inst, "/api/v2/search/tags")
		}
	})
	mux.HandleFunc("GET /api/tempo/v2/search/tag/{name}/values", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "tempo"); inst != nil {
			p.ForwardQuery(w, r, inst, "/api/v2/search/tag/"+r.PathValue("name")+"/values")
		}
	})
	mux.HandleFunc("GET /api/tempo/metrics/query_range", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "tempo"); inst != nil {
			p.ForwardQuery(w, r, inst, "/api/metrics/query_range")
		}
	})
	mux.HandleFunc("GET /api/tempo/metrics/query", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "tempo"); inst != nil {
			p.ForwardQuery(w, r, inst, "/api/metrics/query")
		}
	})
	mux.HandleFunc("GET /api/tempo/echo", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "tempo"); inst != nil {
			p.ForwardQuery(w, r, inst, "/api/echo")
		}
	})
	mux.HandleFunc("GET /api/tempo/overrides", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "tempo"); inst != nil {
			p.ForwardQuery(w, r, inst, "/api/overrides")
		}
	})
	mux.HandleFunc("POST /api/tempo/overrides", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "tempo"); inst != nil {
			p.ForwardPush(w, r, inst, "/api/overrides", h.Get().Gateway.MaxBodyBytes)
		}
	})
	mux.HandleFunc("PATCH /api/tempo/overrides", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "tempo"); inst != nil {
			p.ForwardPush(w, r, inst, "/api/overrides", h.Get().Gateway.MaxBodyBytes)
		}
	})
	mux.HandleFunc("DELETE /api/tempo/overrides", func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "tempo"); inst != nil {
			p.ForwardPush(w, r, inst, "/api/overrides", h.Get().Gateway.MaxBodyBytes)
		}
	})
}
