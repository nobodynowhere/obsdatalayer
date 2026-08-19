package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/config"
	"obsdatalayer/internal/proxy"
	"obsdatalayer/internal/tenant"
)

type mimirObservabilityDoc struct {
	Tenant   tenant.Tenant `json:"tenant"`
	Instance string        `json:"instance"`
	Rules    any           `json:"rules"`
	Alerts   any           `json:"alerts"`
}

func (h *handler) getTenantMimirObservability(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, ok := h.tenants.Get(id)
	if !ok {
		writeErr(w, tenant.ErrNotFound)
		return
	}
	inst, err := h.selectTenantMimirInstance(id)
	if err != nil {
		writeErr(w, err)
		return
	}

	rules, err := h.fetchTenantMimirJSON(r.Context(), inst, id, "/prometheus/api/v1/rules")
	if err != nil {
		writeErr(w, err)
		return
	}
	alerts, err := h.fetchTenantMimirJSON(r.Context(), inst, id, "/prometheus/api/v1/alerts")
	if err != nil {
		writeErr(w, err)
		return
	}

	writeJSON(w, http.StatusOK, mimirObservabilityDoc{
		Tenant:   t,
		Instance: inst.Name,
		Rules:    rules,
		Alerts:   alerts,
	})
}

func (h *handler) selectTenantMimirInstance(tenantID string) (*config.InstanceConfig, error) {
	var dedicated []*config.InstanceConfig
	var shared []*config.InstanceConfig
	for _, inst := range h.cfg.Get().Instances {
		if inst.Backend != "mimir" {
			continue
		}
		target := inst.GetQueryTarget()
		switch target.TenantID {
		case tenantID:
			dedicated = append(dedicated, inst)
		case "":
			shared = append(shared, inst)
		}
	}
	if len(dedicated) == 1 {
		return dedicated[0], nil
	}
	if len(dedicated) > 1 {
		return nil, fmt.Errorf("%w: multiple Mimir instances are dedicated to tenant %q", errAmbiguousMimirInstance, tenantID)
	}
	if len(shared) == 1 {
		return shared[0], nil
	}
	if len(shared) > 1 {
		return nil, fmt.Errorf("%w: multiple shared Mimir instances are configured", errAmbiguousMimirInstance)
	}
	return nil, config.ErrNotFound
}

func (h *handler) fetchTenantMimirJSON(ctx context.Context, inst *config.InstanceConfig, tenantID, upstreamPath string) (any, error) {
	target := inst.GetQueryTarget()
	upstreamURL := target.URL + upstreamPath
	if target.SkipTLSVerify {
		ctx = proxy.WithSkipTLSVerify(ctx)
	}
	ctx = auth.WithRequestAuth(ctx, &auth.RequestAuth{
		Username:  "admin",
		TenantIDs: []string{tenantID},
		IsRead:    true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	proxy.CopyHeadersForUpstream(req, nil, target)

	started := time.Now()
	resp, err := h.mimirHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("query Mimir instance %q: %w", inst.Name, err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		proxy.LogUpstreamNon2XX(inst.Name, http.MethodGet, upstreamURL, resp.StatusCode, time.Since(started), req.Header.Get("X-Scope-OrgID"), body, readErr)
		return nil, fmt.Errorf("Mimir instance %q returned %d: %s", inst.Name, resp.StatusCode, string(body))
	}
	if readErr != nil {
		return nil, fmt.Errorf("read Mimir response: %w", readErr)
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode Mimir response: %w", err)
	}
	return payload, nil
}

func (h *handler) mimirHTTPClient() *http.Client {
	if h.mimirClient != nil {
		return h.mimirClient
	}
	return proxy.NewHTTPClient(h.cfg.Get().Gateway.Timeouts.Query.Duration())
}

var errAmbiguousMimirInstance = errors.New("ambiguous Mimir instance")
