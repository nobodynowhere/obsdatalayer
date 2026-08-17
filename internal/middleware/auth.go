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
// The backend comes from the URL path (/api/{instance}/{backend}/...) and the
// action from the HTTP method (GET is read, anything else is write).
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
		action := auth.ActionWrite
		if r.Method == http.MethodGet {
			action = auth.ActionRead
		}

		tenantIDs, allowed := a.TenantIDsFor(username, backend, action)
		if !allowed {
			// The single most common support question is "why did this 403?".
			slog.Debug("data plane request denied: no matching grant",
				"user", username, "backend", backend, "action", action, "path", r.URL.Path)
			writeForbidden(w)
			return
		}
		slog.Debug("data plane request authorized",
			"user", username, "backend", backend, "action", action, "tenants", tenantIDs)

		ra := &auth.RequestAuth{
			Username:  username,
			TenantIDs: tenantIDs,
			IsRead:    action == auth.ActionRead,
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

// extractBackend parses the backend segment from /api/{instance}/{backend}/...
func extractBackend(path string) string {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 4)
	// parts: ["api", "{instance}", "{backend}", "..."]
	if len(parts) >= 3 {
		return parts[2]
	}
	return ""
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
