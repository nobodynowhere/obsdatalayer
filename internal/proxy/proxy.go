package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"obsdatalayer/internal/config"
)

// Proxy forwards requests to upstream backends.
type Proxy struct {
	queryClient *http.Client
	pushClient  *http.Client
}

// New creates a Proxy with separate query and push clients.
func New(queryClient, pushClient *http.Client) *Proxy {
	return &Proxy{queryClient: queryClient, pushClient: pushClient}
}

// ForwardQuery forwards a read request using the query client.
func (p *Proxy) ForwardQuery(w http.ResponseWriter, r *http.Request, inst *config.InstanceConfig, upstreamPath string) {
	p.forward(w, r, inst, upstreamPath, p.queryClient)
}

// ForwardPush forwards a push request using the push client (Tempo single-target).
func (p *Proxy) ForwardPush(w http.ResponseWriter, r *http.Request, inst *config.InstanceConfig, upstreamPath string) {
	p.forward(w, r, inst, upstreamPath, p.pushClient)
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

	CopyHeadersForUpstream(req, r.Header, inst, target)

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

// CopyHeadersForUpstream copies inbound headers to the upstream request, applying auth rules.
func CopyHeadersForUpstream(req *http.Request, inbound http.Header, inst *config.InstanceConfig, target config.ResolvedTarget) {
	if inst.AuthPassthrough {
		for key, vals := range inbound {
			if !strings.EqualFold(key, "Authorization") {
				req.Header[key] = vals
			}
		}
	} else {
		for key, vals := range inbound {
			if !strings.EqualFold(key, "Authorization") && !strings.EqualFold(key, "X-Scope-OrgID") {
				req.Header[key] = vals
			}
		}
		injectAuth(req, target)
	}
}

// injectAuth sets BasicAuth and X-Scope-OrgID from target config.
func injectAuth(req *http.Request, target config.ResolvedTarget) {
	if target.BasicAuth != "" {
		if parts := strings.SplitN(target.BasicAuth, ":", 2); len(parts) == 2 {
			req.SetBasicAuth(parts[0], parts[1])
		}
	}
	if target.TenantID != "" {
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
