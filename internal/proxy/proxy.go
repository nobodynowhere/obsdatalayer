package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/config"
)

// hopByHopHeaders is the set of headers that must never be forwarded upstream.
// Includes standard hop-by-hop headers (RFC 7230 §6.1) plus gateway auth headers.
var hopByHopHeaders = map[string]bool{
	"Authorization":       true,
	"X-Scope-Orgid":       true, // canonical form of X-Scope-OrgID
	"Connection":          true,
	"Keep-Alive":          true,
	"Transfer-Encoding":   true,
	"Te":                  true,
	"Trailers":            true,
	"Upgrade":             true,
	"Proxy-Authorization": true,
	"Proxy-Authenticate":  true,
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
func (p *Proxy) ForwardPush(w http.ResponseWriter, r *http.Request, inst *config.InstanceConfig, upstreamPath string) {
	p.forward(w, r, inst, upstreamPath, p.PushClient())
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, inst *config.InstanceConfig, upstreamPath string, client *http.Client) {
	target := inst.GetQueryTarget()

	upstreamURL := target.URL + upstreamPath
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, r.Body)
	if err != nil {
		WriteJSONError(w, http.StatusInternalServerError, map[string]string{
			"error": "failed to build upstream request", "instance": inst.Name,
		})
		return
	}

	CopyHeadersForUpstream(req, r.Header, target)

	resp, err := client.Do(req)
	if err != nil {
		if isTimeoutError(err) {
			WriteJSONError(w, http.StatusGatewayTimeout, map[string]string{
				"error": "upstream timeout", "instance": inst.Name,
			})
		} else {
			WriteJSONError(w, http.StatusBadGateway, map[string]string{
				"error": "upstream unavailable", "instance": inst.Name,
			})
		}
		return
	}
	defer resp.Body.Close()

	for key, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// CopyHeadersForUpstream copies safe inbound headers to the upstream request.
// Hop-by-hop headers (Authorization, X-Scope-OrgID, Connection, etc.) are
// stripped. X-Scope-OrgID is then re-injected from the request's auth context
// (set by BasicAuth middleware): for reads all tenant IDs are pipe-joined, for
// writes only the first is used. Falls back to target.TenantID when no auth
// context is present. BasicAuth credentials are injected from target config.
func CopyHeadersForUpstream(req *http.Request, inbound http.Header, target config.ResolvedTarget) {
	for key, vals := range inbound {
		if !hopByHopHeaders[key] {
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
		if ra.IsRead {
			req.Header.Set("X-Scope-OrgID", strings.Join(ra.TenantIDs, "|"))
		} else {
			req.Header.Set("X-Scope-OrgID", ra.TenantIDs[0])
		}
	} else if target.TenantID != "" {
		// Fallback: inject from static target config (no auth context present).
		req.Header.Set("X-Scope-OrgID", target.TenantID)
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
