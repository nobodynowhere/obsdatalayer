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
	"obsdatalayer/internal/rewrite"
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

var errAmbiguousInstance = errors.New("ambiguous backend instances")

// getInstance selects an operator-configured instance for a public
// /api/{backend}/... request. Tenant-bound instances win over shared instances
// when the request resolves to exactly one tenant; multi-tenant reads require a
// shared instance because this gateway does not merge query results across
// separate upstream systems.
func getInstance(h *config.ConfigHolder, r *http.Request, w http.ResponseWriter, backend string) *config.InstanceConfig {
	inst, err := selectInstance(h.Get(), auth.FromContext(r.Context()), backend)
	if err != nil {
		status := http.StatusNotFound
		msg := "no matching instance"
		if errors.Is(err, errAmbiguousInstance) {
			status = http.StatusConflict
			msg = errAmbiguousInstance.Error()
		}
		proxy.WriteJSONError(w, status, map[string]string{"error": msg})
		return nil
	}
	if !scopeRequestToInstance(w, r, inst) {
		return nil
	}
	return inst
}

func selectInstance(cfg *config.Config, ra *auth.RequestAuth, backend string) (*config.InstanceConfig, error) {
	var shared []*config.InstanceConfig
	var dedicated []*config.InstanceConfig
	write := ra == nil || !ra.IsRead

	for _, inst := range cfg.Instances {
		if inst.Backend != backend {
			continue
		}
		wanted := targetTenantIDs(inst, write)
		if len(wanted) == 0 {
			shared = append(shared, inst)
			continue
		}
		if tenantSetAllowed(wanted, raTenantIDs(ra)) {
			dedicated = append(dedicated, inst)
		}
	}

	if ra != nil && ra.IsRead && len(ra.TenantIDs) > 1 {
		switch len(shared) {
		case 0:
			if len(dedicated) > 0 {
				return nil, errAmbiguousInstance
			}
			return nil, config.ErrNotFound
		case 1:
			return shared[0], nil
		default:
			return nil, errAmbiguousInstance
		}
	}

	switch len(dedicated) {
	case 0:
	case 1:
		return dedicated[0], nil
	default:
		return nil, errAmbiguousInstance
	}

	switch len(shared) {
	case 0:
		return nil, config.ErrNotFound
	case 1:
		return shared[0], nil
	default:
		return nil, errAmbiguousInstance
	}
}

func raTenantIDs(ra *auth.RequestAuth) []string {
	if ra == nil {
		return nil
	}
	return ra.TenantIDs
}

func tenantSetAllowed(wanted []string, allowed []string) bool {
	if len(wanted) == 0 {
		return true
	}
	if len(allowed) == 0 {
		return false
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, id := range allowed {
		allowedSet[id] = struct{}{}
	}
	for _, id := range wanted {
		if _, ok := allowedSet[id]; !ok {
			return false
		}
	}
	return true
}

func requireSingleTenant(w http.ResponseWriter, r *http.Request) bool {
	ra := auth.FromContext(r.Context())
	if ra == nil || len(ra.TenantIDs) == 1 {
		return true
	}
	proxy.WriteJSONError(w, http.StatusForbidden, map[string]string{
		"error": "tenant is ambiguous for this endpoint",
	})
	return false
}

func requireSingleTenantForWrite(w http.ResponseWriter, r *http.Request) bool {
	ra := auth.FromContext(r.Context())
	if ra != nil && ra.IsRead {
		return true
	}
	return requireSingleTenant(w, r)
}

func forwardByMethod(w http.ResponseWriter, r *http.Request, inst *config.InstanceConfig, upstreamPath string, maxBodyBytes int64, p *proxy.Proxy) {
	if r.Method == http.MethodGet {
		p.ForwardQuery(w, r, inst, upstreamPath)
		return
	}
	p.ForwardPush(w, r, inst, upstreamPath, maxBodyBytes)
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
// records payload counters, fans out, and writes the response. rewriteFn is
// called only when it is non-nil.
func handlePush(w http.ResponseWriter, r *http.Request, inst *config.InstanceConfig, upstreamPath string, rewriteFn func([]byte) ([]byte, rewrite.PayloadStats, error), maxBodyBytes int64, p *proxy.Proxy, m *metrics.Metrics) {
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
	var stats rewrite.PayloadStats
	if rewriteFn != nil {
		before := len(body)
		body, stats, err = rewriteFn(body)
		if err == nil {
			recordPayloadStats(m, inst, stats, false)
			slog.Debug("processed push payload",
				"instance", inst.Name, "bytes_before", before, "bytes_after", len(body),
				"kind", stats.ItemKind, "items_total", stats.ItemsTotal, "items_modified", stats.ItemsModified,
				"labels_dropped", stats.LabelsDropped, "labels_injected", stats.LabelsInjected,
				"labels_overwritten", stats.LabelsOverwritten)
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
	if statusCode >= 200 && statusCode < 400 {
		recordPayloadStats(m, inst, stats, true)
	}
	copyResponseHeaders(w, respHeaders)
	w.WriteHeader(statusCode)
	if respBody != nil {
		_, _ = w.Write(respBody)
	}
}

func recordPayloadStats(m *metrics.Metrics, inst *config.InstanceConfig, stats rewrite.PayloadStats, forwarded bool) {
	if stats.Empty() {
		return
	}
	if forwarded {
		m.RecordWriteItems(inst.Backend, inst.Name, stats.ItemKind, "forwarded", stats.ItemsTotal)
		return
	}
	m.RecordWriteItems(inst.Backend, inst.Name, stats.ItemKind, "received", stats.ItemsTotal)
	unchanged := stats.ItemsTotal - stats.ItemsModified
	m.RecordWriteItems(inst.Backend, inst.Name, stats.ItemKind, "modified", stats.ItemsModified)
	m.RecordWriteItems(inst.Backend, inst.Name, stats.ItemKind, "unchanged", unchanged)
	m.RecordRewriteLabels(inst.Backend, inst.Name, "dropped", stats.LabelsDropped)
	m.RecordRewriteLabels(inst.Backend, inst.Name, "injected", stats.LabelsInjected)
	m.RecordRewriteLabels(inst.Backend, inst.Name, "overwritten", stats.LabelsOverwritten)
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
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		proxy.LogUpstreamNon2XX(inst.Name, http.MethodPost, url, resp.StatusCode, time.Since(started), req.Header.Get("X-Scope-OrgID"), respBody, nil)
	}
	headers := make(http.Header, len(resp.Header))
	for k, v := range resp.Header {
		headers[k] = v
	}
	return TargetResult{URL: url, StatusCode: resp.StatusCode, Body: respBody, Headers: headers}
}
