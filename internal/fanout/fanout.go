package fanout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/config"
	"obsdatalayer/internal/metrics"
	"obsdatalayer/internal/proxy"
)

// TargetResult holds the result of a single fan-out target request.
type TargetResult struct {
	URL        string
	StatusCode int
	Body       []byte
	Headers    http.Header
	Err        error
	Suppressed bool
}

// PartialFailure represents a non-suppressed failed target in a fan-out request.
type PartialFailure struct {
	Instance   string
	StatusCode int
}

var mimirSuppressPatterns = []string{
	"out of order sample",
	"duplicate sample",
	"timestamp too old",
}

// FormatPartialFailureHeader formats failures as the X-Gateway-Partial-Failure header value.
func FormatPartialFailureHeader(failures []PartialFailure) string {
	parts := make([]string, len(failures))
	for i, f := range failures {
		parts[i] = fmt.Sprintf("instance=%s status=%d", f.Instance, f.StatusCode)
	}
	return strings.Join(parts, ", ")
}

// getInstance looks up an instance by URL path value and validates its backend type.
// Writes a 404 and returns nil if the instance is unknown or has the wrong backend.
func getInstance(h *config.ConfigHolder, r *http.Request, w http.ResponseWriter, backend string) *config.InstanceConfig {
	cfg := h.Get()
	inst, ok := cfg.ByName[r.PathValue("instance")]
	if !ok || inst.Backend != backend {
		proxy.WriteJSONError(w, http.StatusNotFound, map[string]string{"error": "unknown instance"})
		return nil
	}
	if !scopeRequestToInstance(w, r, inst) {
		return nil
	}
	return inst
}

func scopeRequestToInstance(w http.ResponseWriter, r *http.Request, inst *config.InstanceConfig) bool {
	ra := auth.FromContext(r.Context())
	if ra == nil || len(ra.TenantIDs) == 0 {
		return true
	}

	wanted := targetTenantIDs(inst, !ra.IsRead)
	if len(wanted) == 0 {
		if !ra.IsRead && len(ra.TenantIDs) > 1 {
			proxy.WriteJSONError(w, http.StatusForbidden, map[string]string{
				"error": "write tenant is ambiguous for this instance",
			})
			return false
		}
		return true
	}
	if !ra.IsRead && hasUnscopedPushTarget(inst) && len(ra.TenantIDs) > 1 {
		proxy.WriteJSONError(w, http.StatusForbidden, map[string]string{
			"error": "write tenant is ambiguous for this instance",
		})
		return false
	}

	allowed := make(map[string]struct{}, len(ra.TenantIDs))
	for _, id := range ra.TenantIDs {
		allowed[id] = struct{}{}
	}
	for _, id := range wanted {
		if _, ok := allowed[id]; !ok {
			proxy.WriteJSONError(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return false
		}
	}
	ra.TenantIDs = wanted
	return true
}

func targetTenantIDs(inst *config.InstanceConfig, write bool) []string {
	var targets []config.PushTarget
	if write {
		targets = inst.GetPushTargets()
	} else {
		targets = []config.PushTarget{inst.GetQueryTarget()}
	}

	seen := make(map[string]struct{})
	ids := make([]string, 0, len(targets))
	for _, target := range targets {
		if target.TenantID == "" {
			continue
		}
		if _, ok := seen[target.TenantID]; ok {
			continue
		}
		seen[target.TenantID] = struct{}{}
		ids = append(ids, target.TenantID)
	}
	return ids
}

func hasUnscopedPushTarget(inst *config.InstanceConfig) bool {
	for _, target := range inst.GetPushTargets() {
		if target.TenantID == "" {
			return true
		}
	}
	return false
}

// Do fans out a push request to all targets in parallel and returns the aggregated result.
func Do(
	ctx context.Context,
	inst *config.InstanceConfig,
	targets []config.PushTarget,
	body []byte,
	originalHeaders http.Header,
	upstreamPath string,
	client *http.Client,
	m *metrics.Metrics,
) (statusCode int, respBody []byte, respHeaders http.Header, partialFailures []PartialFailure) {

	results := make([]TargetResult, len(targets))
	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Add(1)
		go func(idx int, tgt config.PushTarget) {
			defer wg.Done()
			results[idx] = doSingleTarget(ctx, inst, tgt, body, originalHeaders, upstreamPath, client)
		}(i, target)
	}
	wg.Wait()

	// Record metrics and apply suppression in one pass.
	for i := range results {
		r := &results[i]
		sc := r.StatusCode
		if r.Err != nil {
			sc = 0
		}
		m.RecordFanout(inst.Name, r.URL, sc)

		if inst.Backend == "mimir" && r.StatusCode == 400 && r.Err == nil {
			for _, pattern := range mimirSuppressPatterns {
				if strings.Contains(strings.ToLower(string(r.Body)), pattern) {
					r.Suppressed = true
					m.RecordSuppressed(inst.Name, r.URL, pattern)
					// Suppressed errors are reported to the client as success,
					// so leave a trace of what was swallowed.
					slog.Debug("suppressed upstream error",
						"instance", inst.Name, "target", r.URL, "pattern", pattern)
					break
				}
			}
		}
	}

	mode := inst.FanOutMode
	if mode == "" {
		mode = "any"
	}
	slog.Debug("fan-out complete",
		"instance", inst.Name, "mode", mode, "targets", len(targets),
		"body_bytes", len(body))
	if mode == "all" {
		return doAllMode(inst, results)
	}
	return doAnyMode(inst, results)
}

// copyResponseHeaders copies headers from a fan-out response to the gateway response writer.
func copyResponseHeaders(w http.ResponseWriter, headers http.Header) {
	for k, vals := range headers {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
}

// handlePush reads the body (limited to maxBodyBytes), optionally rewrites labels,
// fans out, and writes the response. rewriteFn is called only when inst.Labels != nil.
func handlePush(w http.ResponseWriter, r *http.Request, inst *config.InstanceConfig, upstreamPath string, rewriteFn func([]byte) ([]byte, error), maxBodyBytes int64, p *proxy.Proxy, m *metrics.Metrics) {
	if maxBodyBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		status := http.StatusBadRequest
		msg := "failed to read request body"
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			status = http.StatusRequestEntityTooLarge
			msg = "request body too large"
			slog.Debug("push rejected: body over limit",
				"instance", inst.Name, "limit_bytes", maxBodyBytes)
		}
		proxy.WriteJSONError(w, status, map[string]string{"error": msg})
		return
	}
	if inst.Labels != nil {
		before := len(body)
		body, err = rewriteFn(body)
		if err == nil {
			slog.Debug("rewrote push labels",
				"instance", inst.Name, "bytes_before", before, "bytes_after", len(body))
		}
		if err != nil {
			proxy.WriteJSONError(w, http.StatusBadRequest, map[string]string{
				"error": "label rewrite failed", "detail": err.Error(),
			})
			return
		}
	}
	statusCode, respBody, respHeaders, partialFailures := Do(r.Context(), inst, inst.GetPushTargets(), body, r.Header, upstreamPath, p.PushClient(), m)
	if len(partialFailures) > 0 {
		w.Header().Set("X-Gateway-Partial-Failure", FormatPartialFailureHeader(partialFailures))
		m.RecordPartialFailure(inst.Name)
	}
	copyResponseHeaders(w, respHeaders)
	w.WriteHeader(statusCode)
	if respBody != nil {
		_, _ = w.Write(respBody)
	}
}

func doAnyMode(inst *config.InstanceConfig, results []TargetResult) (int, []byte, http.Header, []PartialFailure) {
	var firstSuccess *TargetResult
	var partialFailures []PartialFailure
	suppressedCount := 0

	for i := range results {
		r := &results[i]
		switch {
		case r.Suppressed:
			suppressedCount++
		case r.Err != nil || r.StatusCode < 200 || r.StatusCode >= 300:
			sc := r.StatusCode
			if r.Err != nil {
				sc = 502
			}
			partialFailures = append(partialFailures, PartialFailure{Instance: inst.Name, StatusCode: sc})
		default:
			if firstSuccess == nil {
				firstSuccess = r
			}
		}
	}

	if firstSuccess != nil {
		return firstSuccess.StatusCode, firstSuccess.Body, firstSuccess.Headers, partialFailures
	}
	// No real success: all-suppressed is still OK; otherwise all failed.
	if suppressedCount > 0 && len(partialFailures) == 0 {
		return http.StatusNoContent, nil, nil, nil
	}
	body, _ := json.Marshal(map[string]string{"error": "all push targets failed", "instance": inst.Name})
	return http.StatusBadGateway, body, http.Header{"Content-Type": []string{"application/json"}}, nil
}

func doAllMode(inst *config.InstanceConfig, results []TargetResult) (int, []byte, http.Header, []PartialFailure) {
	for i := range results {
		r := &results[i]
		if r.Err != nil || r.StatusCode < 200 || r.StatusCode >= 300 {
			body, _ := json.Marshal(map[string]string{
				"error": "push target failed", "instance": inst.Name,
			})
			return http.StatusBadGateway, body, http.Header{"Content-Type": []string{"application/json"}}, nil
		}
	}
	if len(results) > 0 {
		r := &results[0]
		return r.StatusCode, r.Body, r.Headers, nil
	}
	return http.StatusNoContent, nil, nil, nil
}

func doSingleTarget(
	ctx context.Context,
	inst *config.InstanceConfig,
	target config.PushTarget,
	body []byte,
	originalHeaders http.Header,
	upstreamPath string,
	client *http.Client,
) TargetResult {
	url := target.URL + upstreamPath

	if target.SkipTLSVerify {
		ctx = proxy.WithSkipTLSVerify(ctx)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return TargetResult{URL: url, Err: err}
	}

	proxy.CopyHeadersForUpstream(req, originalHeaders, target)

	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		slog.Debug("fan-out target failed",
			"instance", inst.Name, "target", url,
			"duration", time.Since(started), "error", err)
		return TargetResult{URL: url, Err: err}
	}
	defer resp.Body.Close()

	slog.Debug("fan-out target responded",
		"instance", inst.Name, "target", url,
		"status", resp.StatusCode, "duration", time.Since(started))

	respBody, _ := io.ReadAll(resp.Body)
	headers := make(http.Header, len(resp.Header))
	for k, v := range resp.Header {
		headers[k] = v
	}
	return TargetResult{URL: url, StatusCode: resp.StatusCode, Body: respBody, Headers: headers}
}
