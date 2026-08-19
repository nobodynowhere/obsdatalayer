package middleware

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"obsdatalayer/internal/auth"
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
// /healthz bypasses auth so container probes work without credentials.
func BasicAuth(a auth.Authorizer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
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
			IsRead:         action == auth.ActionRead,
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
	if path == "/ready" {
		return "mimir"
	}
	if strings.HasPrefix(path, "/prometheus/") {
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
	if r.Method == http.MethodGet {
		return auth.ActionRead
	}
	if r.Method == http.MethodPost && isQueryPost(r.URL.Path) {
		return auth.ActionRead
	}
	return auth.ActionWrite
}

func isQueryPost(path string) bool {
	switch path {
	case "/api/mimir/query",
		"/api/mimir/query_range",
		"/api/mimir/query_exemplars",
		"/api/mimir/labels",
		"/api/mimir/series",
		"/api/mimir/search/metric_names",
		"/api/mimir/search/label_names",
		"/api/mimir/search/label_values",
		"/api/mimir/metadata",
		"/api/mimir/read",
		"/api/mimir/cardinality/active_series",
		"/api/mimir/cardinality/label_names",
		"/api/mimir/cardinality/label_values",
		"/api/mimir/format_query",
		"/api/mimir/prometheus/api/v1/query",
		"/api/mimir/prometheus/api/v1/query_range",
		"/api/mimir/prometheus/api/v1/query_exemplars",
		"/api/mimir/prometheus/api/v1/labels",
		"/api/mimir/prometheus/api/v1/series",
		"/api/mimir/prometheus/api/v1/search/metric_names",
		"/api/mimir/prometheus/api/v1/search/label_names",
		"/api/mimir/prometheus/api/v1/search/label_values",
		"/api/mimir/prometheus/api/v1/metadata",
		"/api/mimir/prometheus/api/v1/read",
		"/api/mimir/prometheus/api/v1/cardinality/active_series",
		"/api/mimir/prometheus/api/v1/cardinality/label_names",
		"/api/mimir/prometheus/api/v1/cardinality/label_values",
		"/api/mimir/prometheus/api/v1/format_query",
		"/prometheus/api/v1/query",
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
		"/api/loki/query",
		"/api/loki/query_range",
		"/api/loki/labels",
		"/api/loki/series",
		"/api/loki/index/stats",
		"/api/loki/index/volume",
		"/api/loki/index/volume_range",
		"/api/loki/patterns",
		"/api/loki/format_query":
		return true
	default:
		return strings.HasPrefix(path, "/api/mimir/label/") && strings.HasSuffix(path, "/values") ||
			strings.HasPrefix(path, "/api/mimir/prometheus/api/v1/label/") && strings.HasSuffix(path, "/values") ||
			strings.HasPrefix(path, "/prometheus/api/v1/label/") && strings.HasSuffix(path, "/values") ||
			strings.HasPrefix(path, "/api/loki/label/") && strings.HasSuffix(path, "/values")
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
