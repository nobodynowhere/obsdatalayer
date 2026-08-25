package proxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/config"
	"obsdatalayer/internal/metrics"
)

const upstreamErrorBodyPreviewBytes = 4096

type skipTLSVerifyKey struct{}

// WithSkipTLSVerify marks a single upstream request as allowed to skip TLS
// certificate verification. It only has an effect when the client uses
// NewTransport.
func WithSkipTLSVerify(ctx context.Context) context.Context {
	return context.WithValue(ctx, skipTLSVerifyKey{}, true)
}

type clients struct {
	query *http.Client
	push  *http.Client

	// read shares the query client's transport but carries no overall timeout
	// of its own. Read attempts are bounded per target instead, and a client
	// timeout would silently cap a target configured to allow longer.
	read *http.Client
}

// readClientFrom derives the per-attempt read client from the query client,
// keeping its transport -- and therefore its connection pool -- while dropping
// the whole-call timeout.
func readClientFrom(query *http.Client) *http.Client {
	if query == nil {
		return nil
	}
	return &http.Client{Transport: query.Transport, CheckRedirect: query.CheckRedirect, Jar: query.Jar}
}

// Proxy forwards requests to upstream backends.
// The underlying HTTP clients are stored atomically so timeouts can be reloaded.
type Proxy struct {
	clients atomic.Pointer[clients]

	// health tracks which read targets are failing. It is owned by the Proxy
	// rather than the config so that it survives a reload; see targetHealth.
	health *targetHealth

	// metrics is optional so that tests and the upgrade path can construct a
	// Proxy without one. Every use is nil-guarded.
	metrics atomic.Pointer[metrics.Metrics]

	// defaultTargetTimeout is the fallback for targets with no timeout of their
	// own, held as nanoseconds so it can be swapped on reload.
	defaultTargetTimeout atomic.Int64
}

// SetMetrics attaches the counter sink. It is a setter rather than a
// constructor argument because the Proxy is built before the registry in
// main.go, and because most tests have no interest in counters.
func (p *Proxy) SetMetrics(m *metrics.Metrics) {
	p.metrics.Store(m)
}

// recordRead counts one read attempt, if a sink is attached.
func (p *Proxy) recordRead(instance, target string, ok bool) {
	if m := p.metrics.Load(); m != nil {
		m.RecordRead(instance, target, ok)
	}
}

// recordOperational counts one operational endpoint request, if a sink is
// attached. Kept beside the read recorders and separate from them: see
// Metrics.RecordOperational for why these must not share a series with reads.
func (p *Proxy) recordOperational(instance, target, endpoint, result string) {
	if m := p.metrics.Load(); m != nil {
		m.RecordOperational(instance, target, endpoint, result)
	}
}

// recordReadFailover counts a read that had to try more than one target.
func (p *Proxy) recordReadFailover(instance string) {
	if m := p.metrics.Load(); m != nil {
		m.RecordReadFailover(instance)
	}
}

// recordReadTruncated counts a read whose body failed part-way to the client.
func (p *Proxy) recordReadTruncated(instance, target string) {
	if m := p.metrics.Load(); m != nil {
		m.RecordReadTruncated(instance, target)
	}
}

// recordReadClientDisconnect counts a read whose body copy stopped because the
// caller hung up.
func (p *Proxy) recordReadClientDisconnect(instance, target string) {
	if m := p.metrics.Load(); m != nil {
		m.RecordReadClientDisconnect(instance, target)
	}
}

// New creates a Proxy with separate query and push clients.
func New(queryClient, pushClient *http.Client) *Proxy {
	p := &Proxy{health: newTargetHealth()}
	p.clients.Store(&clients{query: queryClient, push: pushClient, read: readClientFrom(queryClient)})
	p.defaultTargetTimeout.Store(int64(30 * time.Second))
	return p
}

// SetDefaultTargetTimeout sets the fallback applied to a target that does not
// carry its own timeout. Called at startup and on every settings reload.
func (p *Proxy) SetDefaultTargetTimeout(d time.Duration) {
	if d <= 0 {
		return
	}
	p.defaultTargetTimeout.Store(int64(d))
}

// DefaultTargetTimeout returns the current fallback.
func (p *Proxy) DefaultTargetTimeout() time.Duration {
	return time.Duration(p.defaultTargetTimeout.Load())
}

// ReadClient returns the client used for read attempts.
func (p *Proxy) ReadClient() *http.Client { return p.clients.Load().read }

// SetClients replaces the active HTTP clients.
func (p *Proxy) SetClients(queryClient, pushClient *http.Client) {
	p.clients.Store(&clients{query: queryClient, push: pushClient, read: readClientFrom(queryClient)})
}

// QueryClient returns the current query HTTP client.
func (p *Proxy) QueryClient() *http.Client { return p.clients.Load().query }

// PushClient returns the current push HTTP client.
func (p *Proxy) PushClient() *http.Client { return p.clients.Load().push }

// ForwardPush forwards a push request using the push client (Tempo single-target).
// maxBodyBytes caps the request body; pass 0 to leave it uncapped. The body is
// streamed rather than buffered, so the cap is enforced by the upstream read
// and surfaces as a 413 only once the limit is actually crossed.
func (p *Proxy) ForwardPush(w http.ResponseWriter, r *http.Request, inst *config.InstanceConfig, upstreamPath string, maxBodyBytes int64) {
	if maxBodyBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	}
	target := inst.GetQueryTarget()
	if target.URL == "" {
		WriteJSONError(w, http.StatusInternalServerError, map[string]string{
			"error": "instance has no query target", "instance": inst.Name,
		})
		return
	}
	p.forward(w, r, inst, target, upstreamPath, p.PushClient())
}

// OperationalResponse is one answer collected by FetchOperational.
//
// Header is the upstream's full response header. ContentType is pulled out of
// it for convenience because the fan-out surfaces that one field in JSON, but a
// caller relaying the answer verbatim -- the endpoints still served at their
// upstream paths -- must copy Header, or it drops everything the upstream said
// besides the content type.
type OperationalResponse struct {
	StatusCode  int
	Header      http.Header
	ContentType string
	Body        []byte
	Truncated   bool
	Duration    time.Duration
}

// FetchOperational performs one GET against one exact target and returns the
// answer instead of streaming it to a client. It is the only way the gateway
// reaches an upstream operational endpoint, and it differs from both
// ForwardQuery and ForwardPush in three ways that are the whole point of it
// existing separately:
//
//   - It never sends X-Scope-OrgID. These endpoints are not registered behind
//     their backend's tenant middleware, so a tenant assertion means nothing to
//     them; sending one anyway would be the gateway asserting a scope that the
//     answer does not respect. The omission is structural rather than
//     conditional: this path calls CopyHeadersUntenanted, which has no code to
//     set the header, so it cannot be reintroduced by a caller passing
//     different arguments.
//   - It does not fail over, and records no read health. Asking target 2
//     whether it is ready must not make the gateway report that target 1 failed
//     a read, and "target 2 is down" is the answer being asked for rather than
//     an error to route around.
//   - It returns the body rather than streaming it, because the caller fans out
//     to every target and answers with all of them at once. maxBody caps what
//     is held per target; a body that hits the cap is returned truncated and
//     says so, rather than being dropped or held whole. Pass 0 for no cap.
//
// A transport failure is returned as an error. An upstream that answers is not
// an error however it answered: a 503 from /ready is a successful observation.
func (p *Proxy) FetchOperational(ctx context.Context, inst *config.InstanceConfig, target config.PushTarget, endpoint, upstreamPath, rawQuery string, inbound http.Header, maxBody int64) (OperationalResponse, error) {
	upstreamURL := target.URL + upstreamPath
	if rawQuery != "" {
		upstreamURL += "?" + rawQuery
	}

	ctx, cancel := context.WithTimeout(ctx, target.Timeout(p.DefaultTargetTimeout()))
	defer cancel()
	if target.SkipTLSVerify {
		ctx = WithSkipTLSVerify(ctx)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL, nil)
	if err != nil {
		// A malformed target URL is a configuration fault, not an outage, but
		// the caller still could not see this target, so it counts as one.
		p.recordOperational(inst.Name, target.URL, endpoint, metrics.OperationalUnreachable)
		return OperationalResponse{}, err
	}
	CopyHeadersUntenanted(req, inbound, target)

	slog.Debug("fetching upstream operational endpoint",
		"instance", inst.Name, "url", upstreamURL)

	started := time.Now()
	resp, err := p.ReadClient().Do(req)
	if err != nil {
		slog.Debug("upstream operational request failed",
			"instance", inst.Name, "url", upstreamURL,
			"duration", time.Since(started), "error", err)
		p.recordOperational(inst.Name, target.URL, endpoint, metrics.OperationalUnreachable)
		return OperationalResponse{}, err
	}
	defer resp.Body.Close()

	reader := io.Reader(resp.Body)
	if maxBody > 0 {
		reader = io.LimitReader(resp.Body, maxBody+1)
	}
	body, readErr := io.ReadAll(reader)
	if readErr != nil {
		// The status line arrived but the body did not. Nothing usable reached
		// the caller, so this is the unreachable case rather than a partial
		// success -- there is no half-answer to report.
		p.recordOperational(inst.Name, target.URL, endpoint, metrics.OperationalUnreachable)
		return OperationalResponse{}, readErr
	}
	truncated := maxBody > 0 && int64(len(body)) > maxBody
	if truncated {
		body = body[:maxBody]
	}

	out := OperationalResponse{
		StatusCode:  resp.StatusCode,
		Header:      resp.Header.Clone(),
		ContentType: resp.Header.Get("Content-Type"),
		Body:        body,
		Truncated:   truncated,
		Duration:    time.Since(started),
	}
	if isNon2XX(resp.StatusCode) {
		p.recordOperational(inst.Name, target.URL, endpoint, metrics.OperationalError)
		LogUpstreamNon2XX(inst.Name, http.MethodGet, upstreamURL, resp.StatusCode, out.Duration, "", body, nil)
		return out, nil
	}
	p.recordOperational(inst.Name, target.URL, endpoint, metrics.OperationalSuccess)
	return out, nil
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, inst *config.InstanceConfig, target config.PushTarget, upstreamPath string, client *http.Client) {
	upstreamURL := target.URL + upstreamPath
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	ctx := r.Context()
	if target.SkipTLSVerify {
		ctx = WithSkipTLSVerify(ctx)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, upstreamURL, r.Body)
	if err != nil {
		WriteJSONError(w, http.StatusInternalServerError, map[string]string{
			"error": "failed to build upstream request", "instance": inst.Name,
		})
		return
	}

	CopyHeadersForUpstream(req, r.Header, target)

	slog.Debug("forwarding upstream",
		"instance", inst.Name, "method", r.Method, "url", upstreamURL,
		"org_id", req.Header.Get("X-Scope-OrgID"))

	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		slog.Debug("upstream request failed",
			"instance", inst.Name, "url", upstreamURL,
			"duration", time.Since(started), "error", err)
		var maxBytes *http.MaxBytesError
		switch {
		case errors.As(err, &maxBytes):
			// The caller's body exceeded the configured limit mid-stream.
			WriteJSONError(w, http.StatusRequestEntityTooLarge, map[string]string{
				"error": "request body too large", "instance": inst.Name,
			})
		case isTimeoutError(err):
			WriteJSONError(w, http.StatusGatewayTimeout, map[string]string{
				"error": "upstream timeout", "instance": inst.Name,
			})
		default:
			WriteJSONError(w, http.StatusBadGateway, map[string]string{
				"error": "upstream unavailable", "instance": inst.Name,
			})
		}
		return
	}
	defer resp.Body.Close()

	slog.Debug("upstream responded",
		"instance", inst.Name, "url", upstreamURL,
		"status", resp.StatusCode, "duration", time.Since(started))

	for key, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}
	if isNon2XX(resp.StatusCode) {
		body, readErr := io.ReadAll(resp.Body)
		LogUpstreamNon2XX(inst.Name, r.Method, upstreamURL, resp.StatusCode, time.Since(started), req.Header.Get("X-Scope-OrgID"), body, readErr)
		w.WriteHeader(resp.StatusCode)
		if len(body) > 0 {
			_, _ = w.Write(body)
		}
		return
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// ForwardUpgrade proxies a protocol-upgrade request, which is what Loki's live
// tail endpoint is. It cannot go through forward(): that reads a response body
// and copies it, whereas an upgrade hands back the raw connection after a 101
// and streams in both directions until either side closes.
//
// httputil.ReverseProxy handles the hijack and the bidirectional copy. Three
// things still have to be got right here:
//
//   - Headers. The outbound set is rebuilt from the forwarding allowlist plus
//     the handshake headers, so a tail request relays no more than any other
//     request does, apart from what the handshake itself requires.
//   - Tenancy. X-Scope-OrgID is injected exactly as on every other path. Loki
//     reads it at handshake time and the socket stays bound to that scope, so
//     there is no per-frame authorization to do.
//   - Timeouts. The query client's timeout covers a whole exchange and would
//     sever a tail stream when it expired, so the client's transport is used
//     directly. Connection pooling is preserved; the overall deadline is not.
//
// A hijacked connection is not tracked by http.Server.Shutdown, so a tail
// stream is cut when the process stops rather than drained.
func (p *Proxy) ForwardUpgrade(w http.ResponseWriter, r *http.Request, inst *config.InstanceConfig, upstreamPath string) {
	target := inst.GetQueryTarget()
	if target.URL == "" {
		WriteJSONError(w, http.StatusInternalServerError, map[string]string{
			"error": "instance has no query target", "instance": inst.Name,
		})
		return
	}
	upstreamURL, err := url.Parse(target.URL)
	if err != nil {
		WriteJSONError(w, http.StatusInternalServerError, map[string]string{
			"error": "invalid upstream URL", "instance": inst.Name,
		})
		return
	}

	ctx := r.Context()
	if target.SkipTLSVerify {
		ctx = WithSkipTLSVerify(ctx)
	}
	ra := auth.FromContext(ctx)
	var username string
	if ra != nil {
		username = ra.Username
	}

	// Written by the Director, read by ModifyResponse. Needs no
	// synchronisation: this ReverseProxy is built for one request and serves
	// that one request, so the write happens-before the read on the same
	// goroutine.
	var upstreamOrgID string

	rp := &httputil.ReverseProxy{
		Transport: p.QueryClient().Transport,
		Director: func(req *http.Request) {
			req.URL.Scheme = upstreamURL.Scheme
			req.URL.Host = upstreamURL.Host
			req.URL.Path = upstreamURL.Path + upstreamPath
			req.URL.RawQuery = r.URL.RawQuery
			req.Host = upstreamURL.Host

			out := make(http.Header, len(r.Header))
			for key, vals := range r.Header {
				if forwardableHeaders[key] || upgradeHeaders[key] {
					out[key] = vals
				}
			}
			// ReverseProxy appends the client IP to X-Forwarded-For after the
			// Director runs. A nil entry is its documented opt-out. Without it
			// the gateway would leak the caller's address upstream on this one
			// route while stripping it on every other.
			out["X-Forwarded-For"] = nil
			req.Header = out

			if target.BasicAuth != "" {
				if parts := strings.SplitN(target.BasicAuth, ":", 2); len(parts) == 2 {
					req.SetBasicAuth(parts[0], parts[1])
				}
			}
			switch {
			case target.TenantID != "":
				req.Header.Set("X-Scope-OrgID", target.TenantID)
			case ra != nil && len(ra.TenantIDs) > 0:
				// Loki answers 400 to a tail whose X-Scope-OrgID names more
				// than one tenant, so the caller is narrowed to one before
				// reaching here; see requireSingleTenant.
				req.Header.Set("X-Scope-OrgID", ra.TenantIDs[0])
			}
			upstreamOrgID = req.Header.Get("X-Scope-OrgID")
			slog.Debug("forwarding upgrade upstream",
				"instance", inst.Name, "url", req.URL.String(),
				"org_id", upstreamOrgID)
		},
		// ModifyResponse runs before ReverseProxy switches the connection over,
		// so a 101 seen here is the handshake the upstream actually accepted,
		// not merely the one the client asked for. Anything else falls through
		// to the ordinary response path and is not a tail.
		//
		// This is at info because the access log cannot serve the same purpose:
		// Logging wraps ServeHTTP, so its line for a tail is not written until
		// the stream ends, and its duration is the lifetime of the stream. A
		// tail that is still running has, until now, produced nothing at the
		// default level. The debug tracing on either side of this is left as it
		// was -- this is an addition to it, not a promotion of it.
		ModifyResponse: func(res *http.Response) error {
			if res.StatusCode != http.StatusSwitchingProtocols {
				return nil
			}
			slog.Info("loki tail upgraded to websocket",
				"instance", inst.Name,
				"user", username,
				"org_id", upstreamOrgID,
				"path", r.URL.Path)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Debug("upgrade request failed", "instance", inst.Name, "error", err)
			WriteJSONError(w, http.StatusBadGateway, map[string]string{
				"error": "upstream unavailable", "instance": inst.Name,
			})
		},
	}
	rp.ServeHTTP(w, r.WithContext(ctx))
}

// NewHTTPClient returns a client whose transport can skip upstream TLS
// verification per request when the resolved target asks for it.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: NewTransport()}
}

func NewTransport() http.RoundTripper {
	base := http.DefaultTransport.(*http.Transport).Clone()
	insecure := http.DefaultTransport.(*http.Transport).Clone()
	insecure.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	return &tlsSwitchTransport{base: base, insecure: insecure}
}

type tlsSwitchTransport struct {
	base     http.RoundTripper
	insecure http.RoundTripper
}

func (t *tlsSwitchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if skip, _ := req.Context().Value(skipTLSVerifyKey{}).(bool); skip {
		return t.insecure.RoundTrip(req)
	}
	return t.base.RoundTrip(req)
}

// CopyHeadersUntenanted copies relayable inbound headers to the upstream
// request and injects the target's own credentials, and does nothing else. It
// is the tenancy-free half of CopyHeadersForUpstream, which calls it and then
// adds the X-Scope-OrgID assertion.
//
// It exists as its own function so that a request which must carry no tenant
// assertion is served by code that contains no way to add one. The upstream
// operational endpoints are the case: they are not registered behind their
// backend's tenant middleware, so a tenant header is at best ignored and at
// worst a claim the answer does not honour. Expressing that as "call the
// untenanted copier" rather than "call the normal copier with empty tenants"
// means it cannot regress into sending one -- CopyHeadersForUpstream falls back
// to the target's configured tenant ID when the grant has none, which is
// exactly the silent reintroduction this avoids.
func CopyHeadersUntenanted(req *http.Request, inbound http.Header, target config.PushTarget) {
	for key, vals := range inbound {
		if forwardableHeaders[key] {
			req.Header[key] = vals
		}
	}

	// Inject upstream basic auth if configured on the target.
	if target.BasicAuth != "" {
		if parts := strings.SplitN(target.BasicAuth, ":", 2); len(parts) == 2 {
			req.SetBasicAuth(parts[0], parts[1])
		}
	}
}

// CopyHeadersForUpstream copies relayable inbound headers to the upstream
// request. Only headers on the forwardable allowlist are copied; everything
// else -- including Authorization, X-Scope-OrgID, and any header nobody
// anticipated -- is dropped. X-Scope-OrgID is then injected from the auth context
// (set by BasicAuth middleware): configured target tenant wins, reads otherwise
// pipe-join all resolved tenant IDs, and writes otherwise use the single
// resolved tenant chosen by the fan-out layer. Falls back to target.TenantID
// when no auth context is present. BasicAuth credentials are injected from
// target config.
func CopyHeadersForUpstream(req *http.Request, inbound http.Header, target config.PushTarget) {
	CopyHeadersUntenanted(req, inbound, target)

	// Inject X-Scope-OrgID from the resolved auth context.
	if ra := auth.FromContext(req.Context()); ra != nil && len(ra.TenantIDs) > 0 {
		if target.TenantID != "" {
			req.Header.Set("X-Scope-OrgID", target.TenantID)
		} else if ra.IsRead {
			req.Header.Set("X-Scope-OrgID", strings.Join(ra.TenantIDs, "|"))
		} else {
			req.Header.Set("X-Scope-OrgID", ra.TenantIDs[0])
		}
		slog.Debug("injected tenant scope from grant",
			"user", ra.Username, "read", ra.IsRead, "org_id", req.Header.Get("X-Scope-OrgID"))
	} else if target.TenantID != "" {
		// Fallback: inject from static target config (no auth context present).
		req.Header.Set("X-Scope-OrgID", target.TenantID)
		slog.Debug("injected tenant scope from instance config", "org_id", target.TenantID)
	} else {
		slog.Debug("no tenant scope injected; upstream will see no X-Scope-OrgID")
	}
}

// WriteJSONError writes a JSON error response.
func WriteJSONError(w http.ResponseWriter, status int, body map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	data, _ := json.Marshal(body)
	_, _ = w.Write(data)
}

// LogUpstreamNon2XX logs an upstream error response body preview. It never logs
// request bodies, and the response preview is capped so noisy upstream errors do
// not flood logs.
func LogUpstreamNon2XX(instance, method, upstreamURL string, status int, duration time.Duration, orgID string, body []byte, readErr error) {
	preview, truncated := upstreamErrorPreview(body)
	attrs := []any{
		"instance", instance,
		"method", method,
		"url", upstreamURL,
		"status", status,
		"duration", duration,
		"org_id", orgID,
		"body_bytes", len(body),
		"body_preview", preview,
		"body_truncated", truncated,
	}
	if readErr != nil {
		attrs = append(attrs, "body_read_error", readErr)
	}
	slog.Warn("upstream returned non-2xx", attrs...)

	if status == http.StatusUnprocessableEntity && looksLikeTooManyTenants(body) {
		slog.Warn("invalid configuration detected: upstream requires a single tenant but gateway sent multiple tenant IDs", attrs...)
	}
}

func upstreamErrorPreview(body []byte) (string, bool) {
	truncated := len(body) > upstreamErrorBodyPreviewBytes
	if truncated {
		body = body[:upstreamErrorBodyPreviewBytes]
	}
	preview := strings.ToValidUTF8(string(body), "\uFFFD")
	preview = strings.TrimSpace(preview)
	return preview, truncated
}

func looksLikeTooManyTenants(body []byte) bool {
	text := strings.ToLower(string(body))
	return strings.Contains(text, "too many tenant") &&
		strings.Contains(text, "max: 1") &&
		strings.Contains(text, "actual")
}

func isNon2XX(status int) bool {
	return status < 200 || status >= 300
}

// IsTimeout reports whether a failure to reach an upstream was a timeout. It is
// exported so a caller that collects an error rather than writing a status --
// FetchOperational's -- can describe it the same way writeTransportError would.
func IsTimeout(err error) bool { return isTimeoutError(err) }

func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	type timeoutErr interface{ Timeout() bool }
	var te timeoutErr
	return errors.As(err, &te) && te.Timeout()
}
