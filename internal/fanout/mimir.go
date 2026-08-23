package fanout

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"obsdatalayer/internal/auth"
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
func MimirDSRoutes(mux Registrar, mount string, h *config.ConfigHolder, p *proxy.Proxy) {
	// Per-target operational endpoints; see operationalEndpoints.
	RegisterOperationalRoutes(mux, "mimir", h, p, OperationalOptions{})

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
	// The two configuration dumps are Prometheus-compatible paths carrying
	// operational answers, so they keep their passthrough contract and are
	// forwarded operationally rather than as reads. See registerMimirLegacyOperational.
	registerMimirLegacyOperational(mux, "GET "+mount+"/api/v1/status/config", h, p, "status_config", "/api/v1/status/config")
	registerMimirLegacyOperational(mux, "GET "+mount+"/api/v1/status/flags", h, p, "flags", "/api/v1/status/flags")
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

func registerMimirRead(mux Registrar, pattern string, h *config.ConfigHolder, p *proxy.Proxy, endpoint, upstreamPath string) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if inst := getInstance(h, r, w, "mimir"); inst != nil {
			forwardMimirRead(w, r, inst, endpoint, expandMimirPath(upstreamPath, r), p)
		}
	})
}

func registerMimirTenantConfig(mux Registrar, pattern string, h *config.ConfigHolder, p *proxy.Proxy, upstreamPath string) {
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

// registerMimirLegacyOperational serves an operational endpoint at its
// Prometheus-compatible path, keeping the passthrough response an existing
// caller expects while giving it the operational forwarding guarantees.
//
// These paths were registered as reads before the operational actions existed,
// and moving only their authorization would have left them half migrated:
// gated as config while still injecting a tenant header Mimir does not read,
// still counted as reads, and -- the part that actually matters -- still
// feeding the read cool-off, so a failing config dump could park a healthy
// replica and degrade real query traffic. That is the exact side effect these
// endpoints were split out to avoid, and it applied to them least defensibly of
// all: nothing calls them on a schedule, so the failures would be sporadic and
// their effect on queries hard to attribute.
//
// Everything a caller can observe is carried over from ForwardQuery: targets
// are tried in configuration order, a transport failure or a 5xx moves on to
// the next while a 4xx is relayed as the upstream's own answer, the last real
// answer is reported when every target fails, and the upstream's response
// headers are copied through rather than reduced to a content type.
//
// What is deliberately not carried over is invisible to the caller and is the
// point of the exercise: no tenant header, no read counters, and no entry in
// the read cool-off. The request is counted against the operational counter
// under the alias its per-target twin uses, so the two spellings of one
// question aggregate into one series.
//
// The one difference a caller could detect is that the body is collected rather
// than streamed, so it is capped; a response over the cap is refused instead of
// passed through short.
func registerMimirLegacyOperational(mux Registrar, pattern string, h *config.ConfigHolder, p *proxy.Proxy, alias, upstreamPath string) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		inst := getInstance(h, r, w, "mimir")
		if inst == nil {
			return
		}

		relay := func(resp proxy.OperationalResponse) {
			// A verbatim passthrough has no field in which to say the body was
			// cut, and a silently shortened configuration parses. Refuse it.
			if resp.Truncated {
				proxy.WriteJSONError(w, http.StatusBadGateway, map[string]string{
					"error":    "upstream configuration response exceeded the gateway's limit",
					"instance": inst.Name,
				})
				return
			}
			for key, vals := range resp.Header {
				for _, v := range vals {
					w.Header().Add(key, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(resp.Body)
		}

		var lastResp *proxy.OperationalResponse
		var lastErr error
		for _, target := range inst.GetReadTargets() {
			resp, err := p.FetchOperational(r.Context(), inst, target, alias, upstreamPath, r.URL.RawQuery, r.Header, maxOperationalBodyBytes)
			if err != nil {
				lastErr, lastResp = err, nil
				continue
			}
			if resp.StatusCode >= 500 {
				// Same retry rule ForwardQuery applied to these paths before:
				// a transport failure or a 5xx moves on, a 4xx is the upstream
				// answering and is relayed as-is.
				lastResp, lastErr = &resp, nil
				continue
			}
			relay(resp)
			return
		}

		// Every target failed. Report the last real answer where there was one,
		// rather than inventing a status the upstream never sent.
		if lastResp != nil {
			relay(*lastResp)
			return
		}
		if proxy.IsTimeout(lastErr) {
			proxy.WriteJSONError(w, http.StatusGatewayTimeout, map[string]string{
				"error": "upstream timeout", "instance": inst.Name,
			})
			return
		}
		proxy.WriteJSONError(w, http.StatusBadGateway, map[string]string{
			"error": "upstream unavailable", "instance": inst.Name,
		})
	})
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

// mimirOperationalEndpoints is what the /prometheus mount serves beside Mimir's
// data API: the endpoints Mimir registers outside its tenant middleware, each
// under the grant its answer warrants.
//
// Mimir's own route table is the authority for the second column. Every path
// here is registered with auth=false, which is what decides whether the tenant
// middleware wraps the handler, so none of them reads X-Scope-OrgID and the
// gateway sends none.
//
// runtime_config is config rather than status because Mimir's index page calls
// it "Entire runtime config (including overrides)" -- the per-tenant limits map,
// which enumerates the cluster's tenants. metrics is its own action because the
// exposition carries a "user" label holding the tenant ID.
var mimirOperationalEndpoints = []OperationalEndpoint{
	{Alias: "ready", Upstream: "/ready", Action: auth.ActionStatus},
	{Alias: "services", Upstream: "/services", Action: auth.ActionStatus},
	{Alias: "buildinfo", Upstream: "/api/v1/status/buildinfo", Action: auth.ActionStatus},
	{Alias: "config", Upstream: "/config", Action: auth.ActionConfig},
	{Alias: "status_config", Upstream: "/api/v1/status/config", Action: auth.ActionConfig},
	{Alias: "flags", Upstream: "/api/v1/status/flags", Action: auth.ActionConfig},
	{Alias: "runtime_config", Upstream: "/runtime_config", Action: auth.ActionConfig},
	{Alias: "metrics", Upstream: "/metrics", Action: auth.ActionMetrics},
}

// mimirLegacyOperationalPaths are routes registered above at their Mimir paths,
// before the operational actions existed, that answer with the same untenanted
// cluster-wide information and are therefore gated the same way.
//
// Only the two configuration dumps. The health checks -- /prometheus/ready and
// /prometheus/api/v1/status/buildinfo -- deliberately stay on read: they are
// what a client calls to decide whether the backend is usable at all, Grafana's
// Prometheus data source reads build info unprompted for feature detection, and
// moving them would break every existing read-only data source to gate an
// answer that is not a disclosure.
var mimirLegacyOperationalPaths = map[string]string{
	"/prometheus/api/v1/status/config": auth.ActionConfig,
	"/prometheus/api/v1/status/flags":  auth.ActionConfig,
}
