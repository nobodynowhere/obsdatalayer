package proxy

import (
	"log/slog"
	"net/http"
	"sort"
	"strings"
)

// forwardableHeaders is the set of request headers the gateway will relay to a
// backend. It is an allowlist: anything absent is dropped.
//
// A denylist was tried first and is not defensible here. The gateway's whole
// job is tenant isolation, and a denylist forwards every header nobody thought
// to name -- including ones an intermediary between the gateway and the backend
// might trust for identity (X-Org-Id, X-Forwarded-For, Cookie). With an
// allowlist the default for an unknown header is "dropped", so a new header
// cannot become an identity channel by omission.
//
// Keys are in Go's canonical form, which is what http.Header uses after
// parsing, so casing variants from the client collapse onto these entries.
//
// X-Scope-OrgID is deliberately NOT here: it is the tenant assertion, and the
// gateway sets it itself from the caller's grants after this filter runs.
// Authorization is likewise absent -- the gateway terminates client auth and
// injects the backend's own credential.
var forwardableHeaders = map[string]bool{
	// Content negotiation and payload framing. Required for remote_write
	// (protobuf + snappy), OTLP (protobuf or JSON) and query responses.
	"Accept":           true,
	"Accept-Encoding":  true,
	"Content-Type":     true,
	"Content-Encoding": true,

	// Prometheus remote-write and remote-read protocol negotiation. Mimir
	// rejects a remote-write request that arrives without the version header.
	"X-Prometheus-Remote-Write-Version":          true,
	"X-Prometheus-Remote-Read-Version":           true,
	"X-Prometheus-Remote-Write-Protobuf-Message": true,

	// Query attribution and cache control. Loki logs X-Query-Tags against the
	// query; Mimir's query-frontend honours Cache-Control: no-store.
	"X-Query-Tags":  true,
	"Cache-Control": true,

	// W3C trace context, so a trace started by the caller continues through the
	// gateway into the backend. Carries correlation, not identity.
	"Traceparent": true,
	"Tracestate":  true,

	// Attribution in backend access logs.
	"User-Agent": true,
}

// Content-Length and Host are intentionally absent: Go derives both from the
// outbound request's body and URL, and copying a stale value corrupts the
// request rather than describing it.

// IsForwardable reports whether a canonical header key may be relayed upstream.
func IsForwardable(canonicalKey string) bool {
	return forwardableHeaders[canonicalKey]
}

// ForwardableHeaderNames returns the allowlist, sorted. Used by diagnostics and
// tests so the policy can be reported rather than re-typed.
func ForwardableHeaderNames() []string {
	names := make([]string, 0, len(forwardableHeaders))
	for k := range forwardableHeaders {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// upgradeHeaders are the headers a WebSocket handshake needs. They are
// deliberately absent from forwardableHeaders: on an ordinary request they must
// never reach a backend, because an intermediary that honours them would be
// asked to change protocols by a client the gateway is supposed to terminate.
//
// They survive SanitizeInbound only on a request that is genuinely an upgrade,
// and only the upgrade forwarding path relays them. CopyHeadersForUpstream is
// unchanged and still drops them, so every ordinary route keeps exactly the
// guarantee it had before.
var upgradeHeaders = map[string]bool{
	"Connection":               true,
	"Upgrade":                  true,
	"Sec-Websocket-Key":        true,
	"Sec-Websocket-Version":    true,
	"Sec-Websocket-Protocol":   true,
	"Sec-Websocket-Extensions": true,
}

// IsUpgradeRequest reports whether h asks to switch protocols to WebSocket.
// Both conditions are required: a Connection token list containing "upgrade",
// and an Upgrade header naming websocket.
func IsUpgradeRequest(h http.Header) bool {
	if !strings.EqualFold(strings.TrimSpace(h.Get("Upgrade")), "websocket") {
		return false
	}
	for _, token := range strings.Split(h.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
			return true
		}
	}
	return false
}

// IsUpgradeHeader reports whether a canonical header key belongs to the
// WebSocket handshake.
func IsUpgradeHeader(canonicalKey string) bool { return upgradeHeaders[canonicalKey] }

// SanitizeInbound removes every header that may not be relayed upstream,
// mutating h in place. It returns the names it dropped so the caller can log
// them: a silently dropped header is otherwise very hard to diagnose.
//
// This runs at the edge, after authentication has consumed Authorization, so
// that handlers and any future forwarding path only ever see a clean header
// set. CopyHeadersForUpstream applies the same predicate again at the point of
// forwarding, so neither layer is load-bearing on its own.
func SanitizeInbound(h http.Header) []string {
	// A genuine WebSocket handshake keeps the headers that make it one. Every
	// other request, including one that merely names those headers without
	// asking to upgrade, loses them exactly as before.
	keepUpgrade := IsUpgradeRequest(h)
	var dropped []string
	for key := range h {
		if keepUpgrade && upgradeHeaders[key] {
			continue
		}
		if !forwardableHeaders[key] {
			dropped = append(dropped, key)
			delete(h, key)
		}
	}
	sort.Strings(dropped)
	return dropped
}

// LogDropped emits a debug line naming the headers a request lost, so an
// operator chasing "my backend never sees header X" has something to find.
func LogDropped(path string, dropped []string) {
	if len(dropped) == 0 {
		return
	}
	slog.Debug("dropped non-forwardable request headers",
		"path", path, "headers", strings.Join(dropped, ","))
}
