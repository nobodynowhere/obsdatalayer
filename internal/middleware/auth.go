package middleware

import (
	"encoding/json"
	"net/http"
	"strings"
)

// BearerAuth wraps a handler with Bearer token validation.
// /healthz and /metrics skip auth.
func BearerAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != token {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			data, _ := json.Marshal(map[string]string{"error": "unauthorized"})
			_, _ = w.Write(data)
			return
		}

		next.ServeHTTP(w, r)
	})
}
