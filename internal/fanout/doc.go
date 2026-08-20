// Package fanout registers every data-plane route the gateway serves and
// forwards it upstream, fanning writes out to multiple targets where an
// instance is configured for it.
//
// Routes are registered in five bundles, so that what the gateway covers -- and
// what it does not -- can be read off one function each rather than inferred
// from a flat list:
//
//	IngestRoutes           every write path, all three backends
//	MimirDSRoutes          Prometheus data source,   base gateway:port/prometheus
//	LokiDSRoutes           Loki data source,         base gateway:port/loki
//	TempoDSRoutes          Tempo data source,        base gateway:port/tempo
//	AlertmanagerDSRoutes   Alertmanager data source, base gateway:port/alertmanager
//
// The split is not cosmetic. Ingestion and serving a data source impose
// different constraints on the URL:
//
//   - An ingestion client is configured with a *complete* URL, so the gateway
//     owns the whole path and mirrors each upstream project exactly.
//   - A data source is configured with a *base* URL that Grafana extends with
//     paths of its own choosing, so the gateway owns only the base and must
//     serve mount + whatever that data source appends.
//
// Conflating the two produces routes nothing can address: a path that looks
// reasonable but that no Grafana base URL will ever generate.
//
// Each bundle's doc comment lists the base URL it answers, what it covers, and
// what it deliberately does not. A gap belongs in that list, so it stays
// visible at the registration site instead of being discovered by a 404.
//
// See upstream.md at the repository root for the full API shape of all three
// backends and the mapping between the three layers.
package fanout
