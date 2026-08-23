package fanout

import (
	"obsdatalayer/internal/config"
	"obsdatalayer/internal/proxy"
)

// AlertmanagerDSRoutes registers the routes a Grafana Alertmanager data source
// addresses when its implementation is set to Mimir or to Cortex.
//
// Both, not either: the "cortex" and "mimir" entries in Grafana's endpoints map
// are identical path for path, and "cortex" is the default when a data source
// names no implementation, so one route set serves all three of the
// Mimir/Cortex/unset cases.
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
//	GET        /api/v1/status/buildinfo   (feature discovery; undoubled)
//
// Every route is single-tenant. Mimir's Alertmanager has no tenant federation,
// so a pipe-joined X-Scope-OrgID is meaningless here and a caller whose grant
// spans several tenants is refused rather than given an ambiguous scope.
//
// Authorization uses the alerts:read and alerts:write actions: GET is a read,
// anything else is a write. The backend is Mimir -- Loki has no Alertmanager.
//
// That covers the data source's health check too. Grafana tests an Alertmanager
// data source with GET /alertmanager/api/v2/status, which is on the list above
// and so needs alerts:read -- the same grant every other route on this mount
// needs. There is no second grant to remember, which is the problem that kept
// build info and Tempo's /api/echo on read rather than moving them to status.
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
//     These stay excluded even though the status, config and metrics actions
//     now gate other cluster-wide endpoints, because /configs is a different
//     order of disclosure: Mimir registers it auth=false and its handler lists
//     every tenant in the store and streams each one's full Alertmanager
//     configuration -- Slack webhook URLs, PagerDuty routing keys, SMTP
//     passwords. No grant should carry that.
//
//   - The "prometheus" implementation of the data source, which addresses a
//     vanilla Alertmanager at /api/v2/... with no prefix. Mimir does not serve
//     that shape, and serving it here would actively break the other two:
//     Grafana's testDatasource probes {url}/api/v2/status FIRST when the
//     implementation is Mimir or Cortex, and treats a 200 as proof of a
//     misconfiguration -- "you have chosen a Mimir or Cortex implementation,
//     but detected a Prometheus endpoint". The two shapes are mutually
//     exclusive on one base URL by Grafana's own design, so the 404 this mount
//     returns for /api/v2/status is load-bearing rather than a gap.
//
// On provenance: that Mimir serves the v2 API beneath its
// -http.alertmanager-http-prefix was previously inferred, because Mimir's HTTP
// reference documents the prefix and the UI without enumerating the v2
// endpoints. It is now confirmed from Mimir's source instead. RegisterAlertmanager
// mounts the whole Alertmanager under the prefix with a single
// RegisterRoutesWithPrefix(cfg.AlertmanagerHTTPPrefix, am, true, ...), which is
// why the individual v2 paths appear in no route table, and the true there is
// what puts them behind the tenant middleware. Its one exception is the
// buildinfo route above, registered explicitly and auth=false so the prefix
// handler does not swallow it.
// No targets/{instance}/{alias} routes, unlike the other three bundles. This
// mount adapts Grafana's Alertmanager patterns onto Mimir; it is a URL shape,
// not a system, and the Mimir instances behind it have their operational
// endpoints addressed under /prometheus. Routes here would reach the same
// targets and return the same answers.
//
// If that ever changes, note that the authorization middleware classifies these
// by the /{mount}/targets/... shape rather than by BackendMounts, so a route added
// here cannot quietly fall back to a plain read grant.
func AlertmanagerDSRoutes(mux Registrar, mount string, h *config.ConfigHolder, p *proxy.Proxy) {
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

	// Feature discovery. Grafana calls fetchPromBuildInfo before testing an
	// Alertmanager data source, and that helper is shared with the Prometheus
	// data source: it appends /api/v1/status/buildinfo to the base URL and does
	// not add the /alertmanager prefix the endpoints map adds to everything
	// above. So the gateway path is undoubled while the upstream path is not --
	// Mimir registers this one at path.Join(AlertmanagerHTTPPrefix,
	// "/api/v1/status/buildinfo"), ahead of the prefix catch-all.
	//
	// Without it Grafana reads the 404 as "this is Cortex, which has no
	// buildinfo endpoint", sets lazyConfigInit false, and then reports a failed
	// health check for a Mimir Alertmanager that simply has no configuration
	// for the tenant yet -- instead of the "Mimir Alertmanager without the
	// fallback configuration has been discovered" success it would report when
	// talking to Mimir directly.
	//
	// Registered as a read rather than through registerMimirTenantConfig:
	// Mimir registers it auth=false, so it is untenanted upstream, and routing
	// it through the tenant-config path would apply requireSingleTenant and
	// refuse feature discovery outright for a caller whose grant spans several
	// tenants.
	registerMimirRead(mux, "GET "+mount+"/api/v1/status/buildinfo", h, p, "", "/alertmanager/api/v1/status/buildinfo")
}
