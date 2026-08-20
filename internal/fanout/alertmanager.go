package fanout

import (
	"net/http"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/proxy"
)

// AlertmanagerDSRoutes registers the routes a Grafana Alertmanager data source
// addresses when its implementation is set to Mimir.
//
// Data source URL: gateway:port/alertmanager
//
// Grafana joins its own path onto the base URL's path, so a base of
// gateway:port/alertmanager produces gateway:port/alertmanager/alertmanager/
// api/v2/status. The doubling is correct for the same reason as Loki's: the
// mount is a gateway concept and /alertmanager is genuinely Mimir's own prefix
// for the Alertmanager API. The tenant configuration endpoint is a sibling of
// that prefix upstream, not a child of it, which is why one mount has to carry
// both shapes.
//
// The route list is taken verbatim from the endpoints["mimir"] map in
// Grafana's pkg/services/ngalert/api/lotex_am.go, which is what the data source
// actually requests. That map gives paths only; the methods below are from the
// Prometheus Alertmanager v2 OpenAPI specification, which matches it path for
// path. The /api/v1/alerts methods are from Mimir's HTTP API reference.
//
// Covered, matching what Grafana's Alertmanager data source requests:
//
//	GET,POST   /alertmanager/api/v2/silences
//	GET,DELETE /alertmanager/api/v2/silence/{silenceID}
//	GET        /alertmanager/api/v2/status
//	GET        /alertmanager/api/v2/alerts/groups
//	GET,POST   /alertmanager/api/v2/alerts
//	GET,POST,DELETE /api/v1/alerts        (tenant Alertmanager configuration)
//
// Every route is single-tenant. Mimir's Alertmanager has no tenant federation,
// so a pipe-joined X-Scope-OrgID is meaningless here and a caller whose grant
// spans several tenants is refused rather than given an ambiguous scope.
//
// Authorization uses the alerts:read and alerts:write actions: GET is a read,
// anything else is a write. The backend is Mimir -- Loki has no Alertmanager.
//
// Not implemented:
//
//   - GET /alertmanager/api/v2/receivers. It exists in the Alertmanager v2 API
//     but is absent from Grafana's lotex_am.go map, so the data source never
//     asks for it.
//
//   - The Alertmanager web UI at /alertmanager. It serves HTML and assets
//     rather than tenant data, and Grafana does not proxy it.
//
//   - The operator endpoints /multitenant_alertmanager/{status,configs,ring}
//     and delete_tenant_config, which are cluster-wide, not tenant-scoped.
//
//   - The "prometheus" implementation of the data source, which addresses a
//     vanilla Alertmanager at /api/v2/... with no prefix. Mimir does not serve
//     that shape.
//
// One caveat on provenance: that Mimir serves the v2 API beneath its
// -http.alertmanager-http-prefix is inferred rather than transcribed. Mimir's
// reference documents the prefix and the UI but does not enumerate the v2
// endpoints. The inference is that Grafana requests them of Mimir and that
// Mimir's Alertmanager is Cortex-derived; it has not been confirmed against a
// running Mimir.
func AlertmanagerDSRoutes(mux *http.ServeMux, mount string, h *config.ConfigHolder, p *proxy.Proxy) {
	am := func(methods []string, upstream string) {
		for _, method := range methods {
			registerMimirTenantConfig(mux, method+" "+mount+upstream, h, p, upstream)
		}
	}
	get := []string{"GET"}

	am([]string{"GET", "POST"}, "/alertmanager/api/v2/silences")
	am([]string{"GET", "DELETE"}, "/alertmanager/api/v2/silence/{silenceID}")
	am(get, "/alertmanager/api/v2/status")
	am(get, "/alertmanager/api/v2/alerts/groups")
	am([]string{"GET", "POST"}, "/alertmanager/api/v2/alerts")

	// Mimir serves the tenant configuration API at its root, outside the
	// /alertmanager prefix, so it sits directly beneath the mount.
	am([]string{"GET", "POST", "DELETE"}, "/api/v1/alerts")
}
