package proxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/config"
)

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
}

// Proxy forwards requests to upstream backends.
// The underlying HTTP clients are stored atomically so timeouts can be reloaded.
type Proxy struct {
	clients atomic.Pointer[clients]
}

// New creates a Proxy with separate query and push clients.
func New(queryClient, pushClient *http.Client) *Proxy {
	p := &Proxy{}
	p.clients.Store(&clients{query: queryClient, push: pushClient})
	return p
}

// SetClients replaces the active HTTP clients.
func (p *Proxy) SetClients(queryClient, pushClient *http.Client) {
	p.clients.Store(&clients{query: queryClient, push: pushClient})
}

// QueryClient returns the current query HTTP client.
func (p *Proxy) QueryClient() *http.Client { return p.clients.Load().query }

// PushClient returns the current push HTTP client.
func (p *Proxy) PushClient() *http.Client { return p.clients.Load().push }

// ForwardQuery forwards a read request using the query client.
func (p *Proxy) ForwardQuery(w http.ResponseWriter, r *http.Request, inst *config.InstanceConfig, upstreamPath string) {
	p.forward(w, r, inst, upstreamPath, p.QueryClient())
}

// ForwardPush forwards a push request using the push client (Tempo single-target).
// maxBodyBytes caps the request body; pass 0 to leave it uncapped. The body is
// streamed rather than buffered, so the cap is enforced by the upstream read
// and surfaces as a 413 only once the limit is actually crossed.
func (p *Proxy) ForwardPush(w http.ResponseWriter, r *http.Request, inst *config.InstanceConfig, upstreamPath string, maxBodyBytes int64) {
	if maxBodyBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	}
	p.forward(w, r, inst, upstreamPath, p.PushClient())
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, inst *config.InstanceConfig, upstreamPath string, client *http.Client) {
	target := inst.GetQueryTarget()

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
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
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

func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	type timeoutErr interface{ Timeout() bool }
	var te timeoutErr
	return errors.As(err, &te) && te.Timeout()
}
