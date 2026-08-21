package middleware

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/authlimit"
	"obsdatalayer/internal/fanout"
	"obsdatalayer/internal/metrics"
	"obsdatalayer/internal/ui"
)

// AuthGuard bundles the per-source failure throttle with the counters that
// record what it did. A nil *AuthGuard disables throttling, which is what tests
// that are not exercising it pass.
type AuthGuard struct {
	Limiter *authlimit.Limiter
	Metrics *metrics.Metrics
}

// check runs the throttle for a request. It reports whether the request may
// proceed to the credential check, having already written the response if not.
func (g *AuthGuard) check(w http.ResponseWriter, r *http.Request, plane string) (source string, ok bool) {
	if g == nil || g.Limiter == nil {
		return "", true
	}
	source = authlimit.SourceKey(r.RemoteAddr)
	allowed, retryAfter := g.Limiter.Allow(source)
	if allowed {
		return source, true
	}

	slog.Warn("authentication throttled",
		"plane", plane, "source", source, "retry_after", retryAfter, "path", r.URL.Path)
	if g.Metrics != nil {
		g.Metrics.RecordAuthRejected("throttled")
	}
	writeRetryLater(w, http.StatusTooManyRequests, retryAfter,
		"too many failed authentication attempts")
	return source, false
}

// recordFailure notes a rejected credential against the source.
func (g *AuthGuard) recordFailure(source string) {
	if g == nil {
		return
	}
	if g.Metrics != nil {
		g.Metrics.RecordAuthFailure()
	}
	if g.Limiter != nil {
		g.Limiter.RecordFailure(source)
	}
}

// recordSuccess clears a source that has proved it holds a valid credential.
func (g *AuthGuard) recordSuccess(source string) {
	if g == nil || g.Limiter == nil {
		return
	}
	g.Limiter.RecordSuccess(source)
}

// saturated answers a request the gateway declined to hash for.
func (g *AuthGuard) saturated(w http.ResponseWriter, r *http.Request, plane string) {
	slog.Warn("authentication capacity reached; shedding request",
		"plane", plane, "path", r.URL.Path)
	if g != nil && g.Metrics != nil {
		g.Metrics.RecordAuthRejected("saturated")
	}
	writeRetryLater(w, http.StatusServiceUnavailable, time.Second,
		"authentication capacity reached, retry shortly")
}

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
func BasicAuth(a auth.Authorizer, guard *AuthGuard, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Container probes carry no credentials. Both are terminated by the
		// gateway and never forwarded, so nothing tenant-scoped is exposed.
		if r.URL.Path == "/healthz" || r.URL.Path == "/ready" {
			next.ServeHTTP(w, r)
			return
		}

		source, allowed := guard.check(w, r, "data")
		if !allowed {
			return
		}

		// A bearer token is an API key belonging to a user. Authenticating with
		// one is authenticating as that user: everything below, including the
		// grant lookup, is identical either way.
		//
		// Keys are accepted on the data plane only. They are long-lived
		// credentials issued for unattended shippers, and the admin plane
		// creates users and edits routing, so it keeps requiring a password.
		// This follows the same instinct as the wildcard grant that
		// deliberately excludes the admin object.
		var username string
		if token, isBearer := auth.BearerToken(r.Header.Get("Authorization")); isBearer {
			u, err := a.AuthenticateAPIKey(token)
			if err != nil {
				guard.recordFailure(source)
				slog.Debug("data plane api key rejected", "path", r.URL.Path)
				writeUnauthorized(w)
				return
			}
			username = u.Name
			guard.recordSuccess(source)
		} else {
			name, password, ok := r.BasicAuth()
			if !ok {
				// Not counted as a failure: no credential was offered, so
				// nothing was hashed and this costs the gateway nothing to
				// reject.
				slog.Debug("data plane request without credentials", "path", r.URL.Path, "method", r.Method)
				writeUnauthorized(w)
				return
			}
			if _, err := a.AuthenticateContext(r.Context(), name, password); err != nil {
				if errors.Is(err, auth.ErrHashLimitReached) {
					guard.saturated(w, r, "data")
					return
				}
				guard.recordFailure(source)
				slog.Debug("data plane authentication failed", "user", name, "path", r.URL.Path)
				writeUnauthorized(w)
				return
			}
			username = name
			guard.recordSuccess(source)
		}

		backend := extractBackend(r.URL.Path)
		action := actionForRequest(r)

		decision := a.AccessDecision(username, backend, action)
		access := decision.Access
		if !decision.Allowed {
			// The single most common support question is "why did this 403?".
			slog.Info("data plane request denied",
				"status", http.StatusForbidden,
				"phase", "authorization",
				"reason", decision.DenyReason,
				"user", username,
				"backend", backend,
				"action", action,
				"path", r.URL.Path,
				"tenant_count", decision.TenantCount)
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
func AdminAuth(a auth.Authorizer, guard *AuthGuard, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ui.IsUIPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		source, allowed := guard.check(w, r, "admin")
		if !allowed {
			return
		}

		username, password, ok := r.BasicAuth()
		if !ok {
			slog.Debug("admin request without credentials", "path", r.URL.Path, "method", r.Method)
			writeUnauthorized(w)
			return
		}
		if _, err := a.AuthenticateContext(r.Context(), username, password); err != nil {
			if errors.Is(err, auth.ErrHashLimitReached) {
				guard.saturated(w, r, "admin")
				return
			}
			guard.recordFailure(source)
			slog.Debug("admin authentication failed", "user", username, "path", r.URL.Path)
			writeUnauthorized(w)
			return
		}
		guard.recordSuccess(source)
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

// controlAction maps endpoints with special authorization semantics onto
// discrete actions. A plain read or write grant does not satisfy them: the
// Casbin matcher compares actions for equality, so only the specific action or
// the "*" wildcard matches.
func controlAction(method, path string) string {
	switch {
	case isLokiTailPath(path) && method == http.MethodGet:
		return auth.ActionTail
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
	case isDeletePath(path):
		// One action for the whole deletion API, whatever the method: listing,
		// requesting and cancelling a deletion are the same privilege.
		return auth.ActionDelete
	default:
		return ""
	}
}

func isMimirRulesPath(path string) bool {
	return path == "/prometheus/api/v1/rules" ||
		strings.HasPrefix(path, "/prometheus/config/v1/rules") ||
		// Grafana's other spelling of the ruler configuration API, matched
		// exactly and with a trailing slash so that a future
		// /prometheus/rulesomething route cannot inherit rule permissions.
		path == "/prometheus/rules" ||
		strings.HasPrefix(path, "/prometheus/rules/")
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
// isDeletePath matches the data deletion API, which carries its own action so
// that an ordinary write grant cannot destroy data. Loki's is request-based:
// GET lists pending deletions, POST and PUT create one, DELETE cancels one.
func isDeletePath(path string) bool {
	return path == "/loki/loki/api/v1/delete"
}

func isLokiTailPath(path string) bool {
	return path == "/loki/loki/api/v1/tail"
}

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

// writeRetryLater answers a shed request with a Retry-After the client can act
// on. Both statuses it serves mean "not now", never "not ever", so a retryable
// hint is the difference between a client backing off and a client hammering.
func writeRetryLater(w http.ResponseWriter, status int, retryAfter time.Duration, msg string) {
	seconds := int(retryAfter.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	w.WriteHeader(status)
	data, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(data)
}

func writeForbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	data, _ := json.Marshal(map[string]string{"error": "forbidden"})
	_, _ = w.Write(data)
}
