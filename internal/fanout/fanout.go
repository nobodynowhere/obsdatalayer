package fanout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

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
	return inst
}

// Do fans out a push request to all targets in parallel and returns the aggregated result.
func Do(
	ctx context.Context,
	inst *config.InstanceConfig,
	targets []config.ResolvedTarget,
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
		go func(idx int, tgt config.ResolvedTarget) {
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
					break
				}
			}
		}
	}

	mode := inst.FanOutMode
	if mode == "" {
		mode = "any"
	}
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
func handlePush(w http.ResponseWriter, r *http.Request, inst *config.InstanceConfig, upstreamPath string, rewriteFn func([]byte) ([]byte, error), maxBodyBytes int64, client *http.Client, m *metrics.Metrics) {
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
		}
		proxy.WriteJSONError(w, status, map[string]string{"error": msg})
		return
	}
	if inst.Labels != nil {
		body, err = rewriteFn(body)
		if err != nil {
			proxy.WriteJSONError(w, http.StatusBadRequest, map[string]string{
				"error": "label rewrite failed", "detail": err.Error(),
			})
			return
		}
	}
	statusCode, respBody, respHeaders, partialFailures := Do(r.Context(), inst, inst.GetPushTargets(), body, r.Header, upstreamPath, client, m)
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
	target config.ResolvedTarget,
	body []byte,
	originalHeaders http.Header,
	upstreamPath string,
	client *http.Client,
) TargetResult {
	url := target.URL + upstreamPath

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return TargetResult{URL: url, Err: err}
	}

	proxy.CopyHeadersForUpstream(req, originalHeaders, target)

	resp, err := client.Do(req)
	if err != nil {
		return TargetResult{URL: url, Err: err}
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	headers := make(http.Header, len(resp.Header))
	for k, v := range resp.Header {
		headers[k] = v
	}
	return TargetResult{URL: url, StatusCode: resp.StatusCode, Body: respBody, Headers: headers}
}
