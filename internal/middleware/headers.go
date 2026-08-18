package middleware

import (
	"net/http"

	"obsdatalayer/internal/proxy"
)

// SanitizeHeaders strips every request header that may not be relayed to a
// backend, before any handler sees the request.
//
// It must be mounted inside authentication: BasicAuth consumes Authorization,
// which is not forwardable, so stripping first would break sign-in. Mounted
// after, it guarantees that by the time a request reaches any data-plane
// handler its header set already satisfies the forwarding policy -- so a
// handler cannot relay something the policy would have refused, and the
// caller's credential cannot leak upstream through a future code path.
//
// The same predicate is applied again in proxy.CopyHeadersForUpstream at the
// moment of forwarding, so the guarantee does not depend on this middleware
// being mounted.
func SanitizeHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dropped := proxy.SanitizeInbound(r.Header)
		proxy.LogDropped(r.URL.Path, dropped)
		next.ServeHTTP(w, r)
	})
}
