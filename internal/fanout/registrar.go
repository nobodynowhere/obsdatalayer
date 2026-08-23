package fanout

import (
	"context"
	"net/http"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/proxy"
)

// Registrar is the part of *http.ServeMux the route bundles use. Taking an
// interface rather than the concrete mux is what lets a test register every
// bundle against a recorder and read back the full list of patterns: net/http
// offers no way to enumerate a ServeMux after the fact, so without this the
// route inventory in allroutes_test.go could only ever check that the routes it
// names exist -- never that no route exists which it does not name, which is
// the direction that matters.
//
// *http.ServeMux satisfies it, and every caller passes one.
type Registrar interface {
	Handle(pattern string, handler http.Handler)
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// OperationalFetcher is the part of *proxy.Proxy the operational routes use: it
// asks one target one allowlisted question and hands back the answer.
//
// It is an interface for the same reason the routes exist at all. The admin
// listener serves these routes too, so main.go has to hand adminHandler
// something that can reach an upstream -- and handing it the whole *proxy.Proxy
// would say, in the signature, that the admin handler may forward queries,
// pushes and protocol upgrades to any instance. It may not, and does not. This
// narrows what the admin plane is given to the one capability it uses.
//
// The concrete Proxy is still what gets passed, deliberately: it is the same
// instance the data plane uses, so the connection pool, the per-target
// timeouts, and the TLS-verification switch are the ones the operator
// configured, and a settings reload retunes the admin listener's fetches at the
// same moment it retunes the data plane's. A second client built here would
// drift from both.
type OperationalFetcher interface {
	FetchOperational(
		ctx context.Context,
		inst *config.InstanceConfig,
		target config.PushTarget,
		endpoint, upstreamPath, rawQuery string,
		inbound http.Header,
		maxBody int64,
	) (proxy.OperationalResponse, error)
}
