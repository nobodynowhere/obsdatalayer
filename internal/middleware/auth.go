package middleware

import (
	"encoding/json"
	"net/http"
	"strings"

	"obsdatalayer/internal/auth"
)

// BasicAuth wraps a handler requiring HTTP Basic authentication backed by an
// auth.Source. /healthz bypasses auth without credentials. The backend is
// extracted from the URL path (/api/{instance}/{backend}/...); the action is
// derived from the HTTP method (GET→read, anything else→write).
//
// On success the resolved RequestAuth is attached to the request context for
// downstream handlers to use when injecting X-Scope-OrgID.
func BasicAuth(src auth.Source, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}

		username, password, ok := r.BasicAuth()
		if !ok {
			writeUnauthorized(w)
			return
		}

		uf := src.Get()
		if uf == nil {
			writeUnauthorized(w)
			return
		}

		user, err := auth.Authenticate(uf, username, password)
		if err != nil {
			writeUnauthorized(w)
			return
		}

		backend := extractBackend(r.URL.Path)
		action := "write"
		if r.Method == http.MethodGet {
			action = "read"
		}

		tenantIDs, ok := user.TenantIDsFor(backend, action)
		if !ok {
			writeForbidden(w)
			return
		}

		ra := &auth.RequestAuth{
			Username:  username,
			TenantIDs: tenantIDs,
			IsRead:    action == "read",
		}
		next.ServeHTTP(w, r.WithContext(auth.WithRequestAuth(r.Context(), ra)))
	})
}

// extractBackend parses the backend segment from /api/{instance}/{backend}/...
func extractBackend(path string) string {
	// Split off the leading slash then take up to 4 parts.
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
