package adminapi

import (
	"fmt"
	"net/http"

	"obsdatalayer/internal/config"
)

// instanceDoc is the API view of a backend instance. It mirrors InstanceConfig
// with JSON tags, so the wire format stays stable even if the YAML projection
// used elsewhere changes.
type instanceDoc struct {
	Name          string      `json:"name"`
	Backend       string      `json:"backend"`
	URL           string      `json:"url,omitempty"`
	PushURLs      []targetDoc `json:"push_urls,omitempty"`
	FanOutMode    string      `json:"fan_out_mode,omitempty"`
	BasicAuth     string      `json:"basic_auth,omitempty"`
	TenantID      string      `json:"tenant_id,omitempty"`
	SkipTLSVerify bool        `json:"skip_tls_verify,omitempty"`
	Labels        *labelsDoc  `json:"labels,omitempty"`
}

type targetDoc struct {
	URL            string `json:"url"`
	BasicAuth      string `json:"basic_auth,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	// Group names the upstream surface this target serves. Which values are
	// accepted depends on the instance's backend; GET /api/operational-endpoints
	// is not the source for this -- see config.GroupsForBackend.
	Group         string `json:"group,omitempty"`
	SkipTLSVerify bool   `json:"skip_tls_verify,omitempty"`
}

type labelsDoc struct {
	Filter *filterDoc        `json:"filter,omitempty"`
	Inject map[string]string `json:"inject,omitempty"`
}

type filterDoc struct {
	Mode  string   `json:"mode"`
	Names []string `json:"names"`
}

const redacted = "<redacted>"

// toDoc converts an instance for output, redacting upstream credentials.
// hasBasicAuth flags let the UI show whether a credential is set without
// revealing it, and distinguish "unchanged" from "cleared" on the way back in.
func toDoc(inst *config.InstanceConfig) instanceDoc {
	d := instanceDoc{
		Name:          inst.Name,
		Backend:       inst.Backend,
		URL:           inst.URL,
		FanOutMode:    inst.FanOutMode,
		TenantID:      inst.TenantID,
		SkipTLSVerify: inst.SkipTLSVerify,
	}
	if inst.BasicAuth != "" {
		d.BasicAuth = redacted
	}
	for _, pt := range inst.PushURLs {
		t := targetDoc{URL: pt.URL, Group: pt.Group, SkipTLSVerify: pt.SkipTLSVerify}
		t.TimeoutSeconds = pt.TimeoutSeconds
		if pt.BasicAuth != "" {
			t.BasicAuth = redacted
		}
		d.PushURLs = append(d.PushURLs, t)
	}
	if inst.Labels != nil {
		l := &labelsDoc{Inject: inst.Labels.Inject}
		if inst.Labels.Filter != nil {
			l.Filter = &filterDoc{Mode: inst.Labels.Filter.Mode, Names: inst.Labels.Filter.Names}
		}
		d.Labels = l
	}
	return d
}

// fromDoc converts a submitted document into an InstanceConfig. prev is the
// existing instance on an update, used to carry forward credentials the client
// echoed back as "<redacted>" rather than overwriting them with the mask.
//
// A masked push-target credential is resolved by matching the target's group
// and URL, not its position. Positional matching silently misroutes credentials
// when targets are reordered -- target A would be sent target B's upstream
// password.
//
// Group *and* URL, rather than URL alone, because target groups made a URL
// non-unique within an instance: one origin fronting both the ingest and the
// query surface is two targets with the same URL and, quite reasonably, two
// different upstream credentials. Keyed by URL alone the second overwrote the
// first in this map, and an ordinary masked edit then handed both targets
// whichever credential happened to come last -- silently, with a 200.
//
// The URL-only match is kept as a fallback, because it is the case an operator
// hits far more often than duplicate URLs: changing a target's group in the UI
// re-sends that target's credential masked, and its group-and-URL key no longer
// matches anything stored. That fallback is used only when every stored target
// on the URL carries the same credential. When they differ there is no
// non-guess available and the request is refused, as it is when a mask resolves
// to nothing at all -- the failure mode of guessing is sending a credential to
// the wrong backend.
// credentialKey identifies a stored target for credential carry-forward. The
// separator is a NUL because it cannot occur in either half, so no group and
// URL pair can collide with a different one by concatenation. WriteTargets
// builds its de-duplication key the same way.
func credentialKey(group, url string) string { return group + "\x00" + url }

func fromDoc(d instanceDoc, prev *config.InstanceConfig) (*config.InstanceConfig, error) {
	inst := &config.InstanceConfig{
		Name:          d.Name,
		Backend:       d.Backend,
		URL:           d.URL,
		FanOutMode:    d.FanOutMode,
		BasicAuth:     d.BasicAuth,
		TenantID:      d.TenantID,
		SkipTLSVerify: d.SkipTLSVerify,
	}
	// The instance-level credential is unambiguous: the instance is identified
	// by the request path, so there is nothing to mismatch.
	if d.BasicAuth == redacted && prev != nil {
		inst.BasicAuth = prev.BasicAuth
	}

	prevByGroupURL := map[string]string{}
	prevByURL := map[string]string{}
	prevURLAmbiguous := map[string]bool{}
	if prev != nil {
		for _, pt := range prev.PushURLs {
			prevByGroupURL[credentialKey(pt.Group, pt.URL)] = pt.BasicAuth
			if stored, seen := prevByURL[pt.URL]; seen && stored != pt.BasicAuth {
				prevURLAmbiguous[pt.URL] = true
				continue
			}
			prevByURL[pt.URL] = pt.BasicAuth
		}
	}

	for _, t := range d.PushURLs {
		target := config.PushTarget{
			URL:            t.URL,
			BasicAuth:      t.BasicAuth,
			Group:          t.Group,
			SkipTLSVerify:  t.SkipTLSVerify,
			TimeoutSeconds: t.TimeoutSeconds,
		}
		if t.BasicAuth == redacted {
			stored, ok := prevByGroupURL[credentialKey(t.Group, t.URL)]
			if !ok {
				if prevURLAmbiguous[t.URL] {
					return nil, fmt.Errorf(
						"push target %q in group %q sent a redacted basic_auth and matches no stored "+
							"target in that group, and the stored targets on that URL carry different "+
							"credentials; send the credential explicitly", t.URL, t.Group)
				}
				stored, ok = prevByURL[t.URL]
			}
			if !ok {
				return nil, fmt.Errorf(
					"push target %q sent a redacted basic_auth but has no stored credential to keep; "+
						"send the credential explicitly or omit the field", t.URL)
			}
			target.BasicAuth = stored
		}
		inst.PushURLs = append(inst.PushURLs, target)
	}

	if d.Labels != nil {
		l := &config.LabelsConfig{Inject: d.Labels.Inject}
		if d.Labels.Filter != nil {
			l.Filter = &config.FilterConfig{Mode: d.Labels.Filter.Mode, Names: d.Labels.Filter.Names}
		}
		inst.Labels = l
	}
	return inst, nil
}

// ---- instances --------------------------------------------------------------

func (h *handler) listInstances(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfg.Get()
	docs := make([]instanceDoc, 0, len(cfg.Instances))
	for _, inst := range cfg.Instances {
		docs = append(docs, toDoc(inst))
	}
	writeJSON(w, http.StatusOK, map[string]any{"instances": docs})
}

func (h *handler) getInstance(w http.ResponseWriter, r *http.Request) {
	inst, ok := h.cfg.Get().ByName[r.PathValue("name")]
	if !ok {
		writeErr(w, config.ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, toDoc(inst))
}

func (h *handler) createInstance(w http.ResponseWriter, r *http.Request) {
	var doc instanceDoc
	if !decode(w, r, &doc) {
		return
	}
	inst, err := fromDoc(doc, nil)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := config.CreateInstance(h.db, inst, h.tenants, h.cipher); err != nil {
		writeErr(w, err)
		return
	}
	if err := h.afterChange(); err != nil {
		writeErr(w, err)
		return
	}
	created, ok := h.cfg.Get().ByName[doc.Name]
	if !ok {
		writeErr(w, config.ErrNotFound)
		return
	}
	writeJSON(w, http.StatusCreated, toDoc(created))
}

func (h *handler) updateInstance(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var doc instanceDoc
	if !decode(w, r, &doc) {
		return
	}
	prev, ok := h.cfg.Get().ByName[name]
	if !ok {
		writeErr(w, config.ErrNotFound)
		return
	}
	next, err := fromDoc(doc, prev)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := config.UpdateInstance(h.db, name, next, h.tenants, h.cipher); err != nil {
		writeErr(w, err)
		return
	}
	if err := h.afterChange(); err != nil {
		writeErr(w, err)
		return
	}
	updated, ok := h.cfg.Get().ByName[doc.Name]
	if !ok {
		writeErr(w, config.ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, toDoc(updated))
}

func (h *handler) deleteInstance(w http.ResponseWriter, r *http.Request) {
	if err := config.DeleteInstance(h.db, r.PathValue("name")); err != nil {
		writeErr(w, err)
		return
	}
	if err := h.afterChange(); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- gateway settings -------------------------------------------------------

type settingsDoc struct {
	MaxBodyBytes   int64  `json:"max_body_bytes"`
	QueryTimeout   string `json:"query_timeout"`
	PushTimeout    string `json:"push_timeout"`
	LogLevel       string `json:"log_level"`
	ReloadInterval string `json:"reload_interval"`
	// DefaultTargetTimeout applies to any fan-out target that does not set its
	// own timeout_seconds.
	DefaultTargetTimeout string `json:"default_target_timeout"`

	// MetricsUnauthenticated serves GET /metrics without credentials.
	MetricsUnauthenticated bool `json:"metrics_unauthenticated"`

	AuthLimitEnabled        *bool  `json:"auth_limit_enabled"`
	AuthFailureThreshold    int    `json:"auth_failure_threshold"`
	AuthFailureWindow       string `json:"auth_failure_window"`
	AuthBlockDuration       string `json:"auth_block_duration"`
	AuthMaxBlockDuration    string `json:"auth_max_block_duration"`
	AuthMaxConcurrentHashes int    `json:"auth_max_concurrent_hashes"`
	AuthHashWait            string `json:"auth_hash_wait"`

	// AuthHashConcurrencyEffective reports the hashing cap actually in force,
	// which the operator cannot otherwise derive: the configured value of 0
	// means "half the CPUs of whatever host this is". Read-only.
	AuthHashConcurrencyEffective int `json:"auth_hash_concurrency_effective,omitempty"`
}

func (h *handler) getSettings(w http.ResponseWriter, r *http.Request) {
	g := h.cfg.Get().Gateway
	enabled := g.AuthLimit.ThrottleEnabled()
	writeJSON(w, http.StatusOK, settingsDoc{
		MaxBodyBytes:         g.MaxBodyBytes,
		QueryTimeout:         g.Timeouts.Query.Duration().String(),
		PushTimeout:          g.Timeouts.Push.Duration().String(),
		LogLevel:             g.LogLevel,
		ReloadInterval:       g.ReloadInterval.Duration().String(),
		DefaultTargetTimeout: g.DefaultTargetTimeout.Duration().String(),

		MetricsUnauthenticated: g.MetricsUnauthenticated,

		AuthLimitEnabled:        &enabled,
		AuthFailureThreshold:    g.AuthLimit.FailureThreshold,
		AuthFailureWindow:       g.AuthLimit.FailureWindow.Duration().String(),
		AuthBlockDuration:       g.AuthLimit.BlockDuration.Duration().String(),
		AuthMaxBlockDuration:    g.AuthLimit.MaxBlockDuration.Duration().String(),
		AuthMaxConcurrentHashes: g.AuthLimit.MaxConcurrentHashes,
		AuthHashWait:            g.AuthLimit.HashWait.Duration().String(),

		AuthHashConcurrencyEffective: g.AuthLimit.HashConcurrency(),
	})
}

func (h *handler) updateSettings(w http.ResponseWriter, r *http.Request) {
	var doc settingsDoc
	if !decode(w, r, &doc) {
		return
	}

	g := config.GatewayConfig{
		MaxBodyBytes:           doc.MaxBodyBytes,
		LogLevel:               doc.LogLevel,
		MetricsUnauthenticated: doc.MetricsUnauthenticated,
	}
	g.AuthLimit = config.AuthLimitConfig{
		Enabled:             doc.AuthLimitEnabled,
		FailureThreshold:    doc.AuthFailureThreshold,
		MaxConcurrentHashes: doc.AuthMaxConcurrentHashes,
	}
	for _, f := range []struct {
		raw string
		dst *config.Duration
	}{
		{doc.QueryTimeout, &g.Timeouts.Query},
		{doc.PushTimeout, &g.Timeouts.Push},
		{doc.ReloadInterval, &g.ReloadInterval},
		{doc.DefaultTargetTimeout, &g.DefaultTargetTimeout},
		{doc.AuthFailureWindow, &g.AuthLimit.FailureWindow},
		{doc.AuthBlockDuration, &g.AuthLimit.BlockDuration},
		{doc.AuthMaxBlockDuration, &g.AuthLimit.MaxBlockDuration},
		{doc.AuthHashWait, &g.AuthLimit.HashWait},
	} {
		if f.raw == "" {
			continue
		}
		if err := f.dst.UnmarshalText([]byte(f.raw)); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}

	if err := config.SaveSettings(h.db, g); err != nil {
		writeErr(w, err)
		return
	}
	if err := h.afterChange(); err != nil {
		writeErr(w, err)
		return
	}
	h.getSettings(w, r)
}
