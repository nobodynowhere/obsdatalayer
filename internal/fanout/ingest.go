package fanout

import (
	"net/http"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/metrics"
	"obsdatalayer/internal/proxy"
)

// IngestRoutes registers every write path for every backend, in one bundle.
//
// Ingestion is a separate concern from serving a Grafana data source, and the
// two must not be conflated. A data source is given a *base* URL which Grafana
// then extends with paths of its own choosing, so the gateway controls only the
// base. An ingestion client is given a *complete* URL, typed once into an Alloy,
// Promtail, Prometheus or OTLP exporter config, so the gateway controls the
// whole path.
//
// Because the whole path is ours, these routes mirror each upstream project
// exactly: the gateway is addressable as if it were Mimir, Loki or Tempo
// itself, and an existing shipper config works by changing only the host.
//
// Covered:
//
//	metrics  POST /api/v1/push                 Prometheus remote write
//	metrics  POST /api/v1/push/influx/write    Influx line protocol
//	metrics  POST /otlp/v1/metrics             OTLP HTTP
//	logs     POST /loki/api/v1/push            Loki push
//	logs     POST /otlp/v1/logs                OTLP HTTP
//	traces   POST /v1/traces                   OTLP HTTP
//	traces   POST /api/traces                  Jaeger Thrift HTTP
//	traces   POST /api/v2/spans                Zipkin
//
// These paths do not collide with each other. See ingestBackends.
//
// Not implemented:
//
//   - POST /api/prom/push. Both Loki and Mimir serve it for Cortex
//     compatibility, so it is the one genuine collision between the three
//     projects' ingestion surfaces. Both are deprecated; use /loki/api/v1/push
//     or /api/v1/push instead.
//   - OTLP over gRPC, for any signal. The gateway is HTTP only.
//   - Jaeger over Thrift Compact, Thrift Binary or gRPC, which are not HTTP.
//
// The OTLP paths are the one place the shape is not ours to choose: an OTLP
// exporter given OTEL_EXPORTER_OTLP_ENDPOINT appends /v1/<signal> itself. Note
// that Mimir and Loki namespace theirs under /otlp while Tempo serves a bare
// OTel receiver at /v1/traces, so a single base endpoint cannot reach all three
// at upstream-exact paths. Point the three per-signal variables
// (OTEL_EXPORTER_OTLP_{TRACES,METRICS,LOGS}_ENDPOINT) at the full URLs instead;
// the OTLP spec treats those as complete URLs rather than bases.
func IngestRoutes(mux *http.ServeMux, h *config.ConfigHolder, p *proxy.Proxy, m *metrics.Metrics) {
	// ---- Mimir ----------------------------------------------------------
	mux.HandleFunc("POST /api/v1/push", func(w http.ResponseWriter, r *http.Request) {
		forwardMimirPush(w, r, h, p, m, "/api/v1/push", true)
	})
	mux.HandleFunc("POST /api/v1/push/influx/write", func(w http.ResponseWriter, r *http.Request) {
		forwardMimirPush(w, r, h, p, m, "/api/v1/push/influx/write", false)
	})
	mux.HandleFunc("POST /otlp/v1/metrics", func(w http.ResponseWriter, r *http.Request) {
		forwardMimirPush(w, r, h, p, m, "/otlp/v1/metrics", false)
	})

	// ---- Loki -----------------------------------------------------------
	mux.HandleFunc("POST /loki/api/v1/push", func(w http.ResponseWriter, r *http.Request) {
		forwardLokiPush(w, r, h, p, m, "/loki/api/v1/push", true)
	})
	mux.HandleFunc("POST /otlp/v1/logs", func(w http.ResponseWriter, r *http.Request) {
		forwardLokiPush(w, r, h, p, m, "/otlp/v1/logs", false)
	})

	// ---- Tempo ----------------------------------------------------------
	// Tempo's OTLP HTTP receiver is a bare OTel receiver at /v1/traces, not
	// under an /otlp prefix as in Mimir and Loki.
	mux.HandleFunc("POST /v1/traces", func(w http.ResponseWriter, r *http.Request) {
		forwardTempoPush(w, r, h, p, m, "/v1/traces")
	})
	mux.HandleFunc("POST /api/traces", func(w http.ResponseWriter, r *http.Request) {
		forwardTempoPush(w, r, h, p, m, "/api/traces")
	})
	mux.HandleFunc("POST /api/v2/spans", func(w http.ResponseWriter, r *http.Request) {
		forwardTempoPush(w, r, h, p, m, "/api/v2/spans")
	})
}

// IngestBackend returns the backend an ingestion path belongs to, and whether
// the path is an ingestion path at all.
//
// Authorization resolves a backend from the request path, and these paths carry
// no /api/{backend}/ segment to read it from -- they are the upstream projects'
// own paths. The mapping is therefore explicit rather than parsed, which also
// means a new ingestion route cannot silently acquire a backend by the shape of
// its path.
func IngestBackend(path string) (string, bool) {
	backend, ok := ingestBackends[path]
	return backend, ok
}

var ingestBackends = map[string]string{
	"/api/v1/push":              "mimir",
	"/api/v1/push/influx/write": "mimir",
	"/otlp/v1/metrics":          "mimir",

	"/loki/api/v1/push": "loki",
	"/otlp/v1/logs":     "loki",

	"/v1/traces":    "tempo",
	"/api/traces":   "tempo",
	"/api/v2/spans": "tempo",
}

func forwardTempoPush(w http.ResponseWriter, r *http.Request, h *config.ConfigHolder, p *proxy.Proxy, m *metrics.Metrics, upstreamPath string) {
	inst := getInstance(h, r, w, "tempo")
	if inst == nil {
		return
	}
	handlePush(w, r, inst, upstreamPath, nil, h.Get().Gateway.MaxBodyBytes, p, m)
}
