package middleware

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/fanout"
	"obsdatalayer/internal/ui"
)

// BasicAuth guards the data plane. It authenticates with HTTP Basic, resolves
// the caller's tenant IDs for the requested backend and action via Casbin, and
// attaches the result to the request context for the proxy layer to inject as
// X-Scope-OrgID.
//
// The backend comes from the URL path (/api/{backend}/...), except the
// GEM-compatible /prometheus/... surface, which is routed to Mimir. GET requests
// are reads, and POST requests to known query endpoints are also reads because
// the Prometheus-compatible APIs allow form-encoded query requests.
// /healthz and /ready bypass auth so container probes work without
// credentials; both are answered by the gateway and never forwarded.
func BasicAuth(a auth.Authorizer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Container probes carry no credentials. Both are terminated by the
		// gateway and never forwarded, so nothing tenant-scoped is exposed.
		if r.URL.Path == "/healthz" || r.URL.Path == "/ready" {
			next.ServeHTTP(w, r)
			return
		}

		username, password, ok := r.BasicAuth()
		if !ok {
			slog.Debug("data plane request without credentials", "path", r.URL.Path, "method", r.Method)
			writeUnauthorized(w)
			return
		}
		if _, err := a.Authenticate(username, password); err != nil {
			slog.Debug("data plane authentication failed", "user", username, "path", r.URL.Path)
			writeUnauthorized(w)
			return
		}

		backend := extractBackend(r.URL.Path)
		action := actionForRequest(r)

		access, allowed := a.AccessFor(username, backend, action)
		if !allowed {
			// The single most common support question is "why did this 403?".
			slog.Debug("data plane request denied: no matching grant",
				"user", username, "backend", backend, "action", action, "path", r.URL.Path)
			writeForbidden(w)
			return
		}
		slog.Debug("data plane request authorized",
			"user", username, "backend", backend, "action", action, "tenants", access.TenantIDs,
			"label_selectors", access.LabelSelectors)

		ra := &auth.RequestAuth{
			Username:       username,
			TenantIDs:      access.TenantIDs,
			LabelSelectors: access.LabelSelectors,
			IsRead:         auth.ActionIsRead(action),
		}
		next.ServeHTTP(w, r.WithContext(auth.WithRequestAuth(r.Context(), ra)))
	})
}

// AdminAuth guards the admin plane. Every API endpoint requires HTTP Basic
// credentials plus an explicit admin grant, including /metrics and /healthz,
// because the metrics carry upstream backend URLs.
//
// The one exception is the embedded UI bundle. Static HTML, JS and CSS carry no
// tenant data, and the browser cannot supply credentials for the initial
// document load, so the SPA shell is served anonymously and then authenticates
// against these same endpoints for every piece of data it displays.
func AdminAuth(a auth.Authorizer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ui.IsUIPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		username, password, ok := r.BasicAuth()
		if !ok {
			slog.Debug("admin request without credentials", "path", r.URL.Path, "method", r.Method)
			writeUnauthorized(w)
			return
		}
		if _, err := a.Authenticate(username, password); err != nil {
			slog.Debug("admin authentication failed", "user", username, "path", r.URL.Path)
			writeUnauthorized(w)
			return
		}
		if !a.CanAdmin(username) {
			slog.Debug("admin request denied: no admin grant", "user", username, "path", r.URL.Path)
			writeForbidden(w)
			return
		}
		ra := &auth.RequestAuth{Username: username}
		next.ServeHTTP(w, r.WithContext(auth.WithRequestAuth(r.Context(), ra)))
	})
}

// extractBackend parses the backend segment from /api/{backend}/...
func extractBackend(path string) string {
	// Ingestion routes carry each upstream project's own path and so have no
	// /api/{backend}/ segment to read a backend from. The mapping is explicit.
	if backend, ok := fanout.IngestBackend(path); ok {
		return backend
	}
	if strings.HasPrefix(path, "/prometheus/") {
		return "mimir"
	}
	// Loki's native layout, served at the gateway root so a Grafana Loki data
	// source can address the gateway directly. /api/prom/ is Loki's legacy
	// spelling of the ruler API and has to be matched before the generic
	// /api/{backend}/ parse below, which would otherwise read "prom" as a
	// backend name. Claiming it for Loki means it is not available as Mimir's
	// Cortex-compatibility prefix.
	if strings.HasPrefix(path, "/loki/") {
		return "loki"
	}
	if strings.HasPrefix(path, "/tempo/") {
		return "tempo"
	}
	// The Alertmanager data source mount. Mimir owns it -- Loki has no
	// Alertmanager.
	if strings.HasPrefix(path, "/alertmanager/") {
		return "mimir"
	}
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
	// parts: ["api", "{backend}", "..."]
	if len(parts) >= 2 && parts[0] == "api" {
		return parts[1]
	}
	return ""
}

func actionForRequest(r *http.Request) string {
	if action := controlAction(r.Method, r.URL.Path); action != "" {
		return action
	}
	if r.Method == http.MethodGet {
		return auth.ActionRead
	}
	if r.Method == http.MethodPost && isQueryPost(r.URL.Path) {
		return auth.ActionRead
	}
	return auth.ActionWrite
}

// controlAction maps rule and alert management endpoints onto the discrete
// rules:*/alerts:* actions, so managing alerting rules is a separate permission
// from reading or writing series and logs. A plain read or write grant does not
// satisfy them: the Casbin matcher compares actions for equality, so only the
// specific action or the "*" wildcard matches.
func controlAction(method, path string) string {
	switch {
	case isMimirRulesPath(path), isLokiRulesPath(path):
		if method == http.MethodGet {
			return auth.ActionRulesRead
		}
		return auth.ActionRulesWrite
	case isMimirAlertsPath(path), isLokiAlertsPath(path):
		if method == http.MethodGet {
			return auth.ActionAlertsRead
		}
		return auth.ActionAlertsWrite
	default:
		return ""
	}
}

func isMimirRulesPath(path string) bool {
	return path == "/prometheus/api/v1/rules" ||
		strings.HasPrefix(path, "/prometheus/config/v1/rules")
}

func isMimirAlertsPath(path string) bool {
	return path == "/prometheus/api/v1/alerts" ||
		// Everything beneath the Alertmanager data source mount: the v2 API
		// and the tenant configuration endpoint. GET is alerts:read, anything
		// else is alerts:write.
		strings.HasPrefix(path, "/alertmanager/")
}

// isLokiRulesPath covers the /loki mount a Grafana Loki data source addresses.
// Beneath it Loki serves its ruler config at three spellings --
// its own, the legacy /api/prom one, and the Prometheus-compatible state
// listing -- all of which are rule management, not data reads.
func isLokiRulesPath(path string) bool {
	return path == "/loki/loki/api/v1/rules" ||
		strings.HasPrefix(path, "/loki/loki/api/v1/rules/") ||
		path == "/loki/api/prom/rules" ||
		strings.HasPrefix(path, "/loki/api/prom/rules/") ||
		path == "/loki/prometheus/api/v1/rules"
}

// Loki has no alertmanager, so its only alert surface is the read-only
// Prometheus-compatible listing of currently firing alerts. A non-GET here
// still resolves to alerts:write and is therefore refused, because no grant can
// carry alerts:write on loki.
func isLokiAlertsPath(path string) bool {
	return path == "/loki/prometheus/api/v1/alerts"
}

func isQueryPost(path string) bool {
	switch path {
	case "/prometheus/api/v1/query",
		"/prometheus/api/v1/query_range",
		"/prometheus/api/v1/query_exemplars",
		"/prometheus/api/v1/labels",
		"/prometheus/api/v1/series",
		"/prometheus/api/v1/search/metric_names",
		"/prometheus/api/v1/search/label_names",
		"/prometheus/api/v1/search/label_values",
		"/prometheus/api/v1/metadata",
		"/prometheus/api/v1/read",
		"/prometheus/api/v1/cardinality/active_series",
		"/prometheus/api/v1/cardinality/label_names",
		"/prometheus/api/v1/cardinality/label_values",
		"/prometheus/api/v1/format_query",
		"/loki/loki/api/v1/series",
		"/loki/loki/api/v1/detected_fields",
		"/loki/loki/api/v1/format_query":
		return true
	default:
		return strings.HasPrefix(path, "/prometheus/api/v1/label/") && strings.HasSuffix(path, "/values") ||
			strings.HasPrefix(path, "/loki/loki/api/v1/detected_field/") && strings.HasSuffix(path, "/values")
	}
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("WWW-Authenticate", `Basic realm="gateway"`)
	w.WriteHeader(http.StatusUnauthorized)
	data, _ := json.Marshal(map[string]string{"error": "unauthorized"})
	_, _ = w.Write(data)
}

func writeForbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	data, _ := json.Marshal(map[string]string{"error": "forbidden"})
	_, _ = w.Write(data)
}
