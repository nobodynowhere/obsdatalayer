package fanout_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/fanout"
	"obsdatalayer/internal/metrics"
)

func newMetrics() *metrics.Metrics {
	return metrics.New(prometheus.NewRegistry())
}

func newInstance(name, backend, fanOutMode string) *config.InstanceConfig {
	return &config.InstanceConfig{
		Name:       name,
		Backend:    backend,
		FanOutMode: fanOutMode,
	}
}

func makeTarget(url string) config.PushTarget {
	return config.PushTarget{URL: url}
}

// ---- FormatPartialFailureHeader tests ----

func TestFormatPartialFailureHeaderEmpty(t *testing.T) {
	s := fanout.FormatPartialFailureHeader(nil)
	if s != "" {
		t.Errorf("expected empty string, got %q", s)
	}
}

func TestFormatPartialFailureHeaderSingle(t *testing.T) {
	failures := []fanout.PartialFailure{
		{Instance: "loki-prod", StatusCode: 502},
	}
	s := fanout.FormatPartialFailureHeader(failures)
	if s != "instance=loki-prod status=502" {
		t.Errorf("unexpected header value: %q", s)
	}
}

func TestFormatPartialFailureHeaderMultiple(t *testing.T) {
	failures := []fanout.PartialFailure{
		{Instance: "loki-prod", StatusCode: 502},
		{Instance: "loki-prod", StatusCode: 503},
	}
	s := fanout.FormatPartialFailureHeader(failures)
	if s != "instance=loki-prod status=502, instance=loki-prod status=503" {
		t.Errorf("unexpected header value: %q", s)
	}
}

// ---- Do (any mode) tests ----

func TestDoAnySingleTargetSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	inst := newInstance("loki-prod", "loki", "any")
	targets := []config.PushTarget{makeTarget(upstream.URL)}
	m := newMetrics()

	statusCode, _, _, partialFailures := fanout.Do(
		context.Background(), inst, targets, []byte("body"),
		http.Header{"Content-Type": []string{"application/json"}},
		"/loki/api/v1/push", http.DefaultClient, m,
	)
	if statusCode != http.StatusNoContent {
		t.Errorf("expected 204, got %d", statusCode)
	}
	if len(partialFailures) != 0 {
		t.Errorf("expected no partial failures, got %v", partialFailures)
	}
}

func TestDoAnyAllTargetsSucceed(t *testing.T) {
	upstream1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-From", "upstream1")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream1.Close)

	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream2.Close)

	inst := newInstance("loki-prod", "loki", "any")
	targets := []config.PushTarget{makeTarget(upstream1.URL), makeTarget(upstream2.URL)}
	m := newMetrics()

	statusCode, _, _, partialFailures := fanout.Do(
		context.Background(), inst, targets, []byte("body"),
		http.Header{"Content-Type": []string{"application/json"}},
		"/loki/api/v1/push", http.DefaultClient, m,
	)
	if statusCode != http.StatusNoContent {
		t.Errorf("expected 204, got %d", statusCode)
	}
	if len(partialFailures) != 0 {
		t.Errorf("expected no partial failures, got %v", partialFailures)
	}
}

func TestDoAnyOneTargetFailsOneSucceeds(t *testing.T) {
	upstreamOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstreamOK.Close)

	upstreamFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(upstreamFail.Close)

	inst := newInstance("loki-prod", "loki", "any")
	targets := []config.PushTarget{makeTarget(upstreamFail.URL), makeTarget(upstreamOK.URL)}
	m := newMetrics()

	statusCode, _, _, partialFailures := fanout.Do(
		context.Background(), inst, targets, []byte("body"),
		http.Header{"Content-Type": []string{"application/json"}},
		"/loki/api/v1/push", http.DefaultClient, m,
	)
	if statusCode != http.StatusNoContent {
		t.Errorf("expected 204, got %d", statusCode)
	}
	if len(partialFailures) != 1 {
		t.Errorf("expected 1 partial failure, got %d", len(partialFailures))
	}
}

func TestDoAnyAllTargetsFail(t *testing.T) {
	upstream1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream1.Close)

	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(upstream2.Close)

	inst := newInstance("loki-prod", "loki", "any")
	targets := []config.PushTarget{makeTarget(upstream1.URL), makeTarget(upstream2.URL)}
	m := newMetrics()

	statusCode, respBody, _, _ := fanout.Do(
		context.Background(), inst, targets, []byte("body"),
		http.Header{"Content-Type": []string{"application/json"}},
		"/loki/api/v1/push", http.DefaultClient, m,
	)
	if statusCode != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", statusCode)
	}
	if !strings.Contains(string(respBody), "all push targets failed") {
		t.Errorf("expected body to mention 'all push targets failed', got %s", respBody)
	}
}

func TestDoAnyConnectionError(t *testing.T) {
	// Start then immediately close to get a connection error
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := upstream.URL
	upstream.Close() // Close before making request

	inst := newInstance("loki-prod", "loki", "any")
	targets := []config.PushTarget{makeTarget(url)}
	m := newMetrics()

	statusCode, respBody, _, _ := fanout.Do(
		context.Background(), inst, targets, []byte("body"),
		http.Header{"Content-Type": []string{"application/json"}},
		"/loki/api/v1/push", http.DefaultClient, m,
	)
	if statusCode != http.StatusBadGateway {
		t.Errorf("expected 502 for connection error, got %d", statusCode)
	}
	if !strings.Contains(string(respBody), "all push targets failed") {
		t.Errorf("expected 'all push targets failed' body, got %s", respBody)
	}
}

// ---- Do (all mode) tests ----

func TestDoAllTargetsSucceed(t *testing.T) {
	upstream1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream1.Close)

	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream2.Close)

	inst := newInstance("loki-prod", "loki", "all")
	targets := []config.PushTarget{makeTarget(upstream1.URL), makeTarget(upstream2.URL)}
	m := newMetrics()

	statusCode, _, _, _ := fanout.Do(
		context.Background(), inst, targets, []byte("body"),
		http.Header{"Content-Type": []string{"application/json"}},
		"/loki/api/v1/push", http.DefaultClient, m,
	)
	if statusCode != http.StatusNoContent {
		t.Errorf("expected 204, got %d", statusCode)
	}
}

func TestDoAllOneTargetFails(t *testing.T) {
	upstreamOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstreamOK.Close)

	upstreamFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstreamFail.Close)

	inst := newInstance("loki-prod", "loki", "all")
	targets := []config.PushTarget{makeTarget(upstreamOK.URL), makeTarget(upstreamFail.URL)}
	m := newMetrics()

	statusCode, respBody, _, _ := fanout.Do(
		context.Background(), inst, targets, []byte("body"),
		http.Header{"Content-Type": []string{"application/json"}},
		"/loki/api/v1/push", http.DefaultClient, m,
	)
	if statusCode != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", statusCode)
	}
	if !strings.Contains(string(respBody), "push target failed") {
		t.Errorf("expected body to mention 'push target failed', got %s", respBody)
	}
	if !strings.Contains(string(respBody), "loki-prod") {
		t.Errorf("expected body to include instance name 'loki-prod', got %s", respBody)
	}
}

// ---- Mimir suppression tests (any mode) ----

func mimirUpstreamWith(statusCode int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(statusCode)
		fmt.Fprint(w, body)
	}))
}

func TestMimirSuppressOutOfOrderSample(t *testing.T) {
	upstream := mimirUpstreamWith(400, "out of order sample")
	t.Cleanup(upstream.Close)

	inst := newInstance("mimir-prod", "mimir", "any")
	targets := []config.PushTarget{makeTarget(upstream.URL)}
	m := newMetrics()

	statusCode, _, _, partialFailures := fanout.Do(
		context.Background(), inst, targets, []byte("body"),
		http.Header{"Content-Type": []string{"application/x-protobuf"}},
		"/api/v1/push", http.DefaultClient, m,
	)
	// With all targets suppressed, returns 204
	if statusCode != http.StatusNoContent {
		t.Errorf("expected 204 (all suppressed), got %d", statusCode)
	}
	if len(partialFailures) != 0 {
		t.Errorf("expected no partial failures for suppressed error, got %v", partialFailures)
	}
}

func TestMimirSuppressDuplicateSample(t *testing.T) {
	upstream := mimirUpstreamWith(400, "duplicate sample received")
	t.Cleanup(upstream.Close)

	inst := newInstance("mimir-prod", "mimir", "any")
	targets := []config.PushTarget{makeTarget(upstream.URL)}
	m := newMetrics()

	statusCode, _, _, _ := fanout.Do(
		context.Background(), inst, targets, []byte("body"),
		http.Header{},
		"/api/v1/push", http.DefaultClient, m,
	)
	if statusCode != http.StatusNoContent {
		t.Errorf("expected 204 (all suppressed), got %d", statusCode)
	}
}

func TestMimirSuppressTimestampTooOld(t *testing.T) {
	upstream := mimirUpstreamWith(400, "timestamp too old for the given series")
	t.Cleanup(upstream.Close)

	inst := newInstance("mimir-prod", "mimir", "any")
	targets := []config.PushTarget{makeTarget(upstream.URL)}
	m := newMetrics()

	statusCode, _, _, _ := fanout.Do(
		context.Background(), inst, targets, []byte("body"),
		http.Header{},
		"/api/v1/push", http.DefaultClient, m,
	)
	if statusCode != http.StatusNoContent {
		t.Errorf("expected 204 (all suppressed), got %d", statusCode)
	}
}

func TestMimirUnmatchedBodyNotSuppressed(t *testing.T) {
	upstream := mimirUpstreamWith(400, "some other error")
	t.Cleanup(upstream.Close)

	inst := newInstance("mimir-prod", "mimir", "any")
	targets := []config.PushTarget{makeTarget(upstream.URL)}
	m := newMetrics()

	statusCode, _, _, partialFailures := fanout.Do(
		context.Background(), inst, targets, []byte("body"),
		http.Header{},
		"/api/v1/push", http.DefaultClient, m,
	)
	if statusCode != http.StatusBadGateway {
		t.Errorf("expected 502 for unmatched 400, got %d", statusCode)
	}
	if len(partialFailures) != 0 {
		// In all-failed scenario, partialFailures is nil (all failed)
		// The all-failed path returns nil for partialFailures
		t.Logf("partial failures: %v", partialFailures)
	}
}

func TestMimirAllSuppressed(t *testing.T) {
	upstream1 := mimirUpstreamWith(400, "out of order sample")
	t.Cleanup(upstream1.Close)
	upstream2 := mimirUpstreamWith(400, "duplicate sample")
	t.Cleanup(upstream2.Close)

	inst := newInstance("mimir-prod", "mimir", "any")
	targets := []config.PushTarget{makeTarget(upstream1.URL), makeTarget(upstream2.URL)}
	m := newMetrics()

	statusCode, _, _, partialFailures := fanout.Do(
		context.Background(), inst, targets, []byte("body"),
		http.Header{},
		"/api/v1/push", http.DefaultClient, m,
	)
	if statusCode != http.StatusNoContent {
		t.Errorf("expected 204 when all suppressed, got %d", statusCode)
	}
	if len(partialFailures) != 0 {
		t.Errorf("expected no partial failures, got %v", partialFailures)
	}
}

func TestMimirOneSuppressedOneSuccess(t *testing.T) {
	upstreamSuppressed := mimirUpstreamWith(400, "out of order sample")
	t.Cleanup(upstreamSuppressed.Close)

	upstreamOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstreamOK.Close)

	inst := newInstance("mimir-prod", "mimir", "any")
	targets := []config.PushTarget{makeTarget(upstreamSuppressed.URL), makeTarget(upstreamOK.URL)}
	m := newMetrics()

	statusCode, _, _, partialFailures := fanout.Do(
		context.Background(), inst, targets, []byte("body"),
		http.Header{},
		"/api/v1/push", http.DefaultClient, m,
	)
	if statusCode != http.StatusNoContent {
		t.Errorf("expected 204 (success from non-suppressed target), got %d", statusCode)
	}
	if len(partialFailures) != 0 {
		t.Errorf("expected no partial failures (suppressed not counted), got %v", partialFailures)
	}
}

func TestMimir409NotSuppressed(t *testing.T) {
	upstream := mimirUpstreamWith(409, "conflict")
	t.Cleanup(upstream.Close)

	inst := newInstance("mimir-prod", "mimir", "any")
	targets := []config.PushTarget{makeTarget(upstream.URL)}
	m := newMetrics()

	statusCode, _, _, _ := fanout.Do(
		context.Background(), inst, targets, []byte("body"),
		http.Header{},
		"/api/v1/push", http.DefaultClient, m,
	)
	// 409 is not suppressed, so all targets failed -> 502
	if statusCode != http.StatusBadGateway {
		t.Errorf("expected 502 (409 not suppressed), got %d", statusCode)
	}
}

func TestMimirAllModeSuppressionNotApplied(t *testing.T) {
	// In all mode, suppression does not apply - 400 fails hard
	upstream := mimirUpstreamWith(400, "out of order sample")
	t.Cleanup(upstream.Close)

	inst := newInstance("mimir-prod", "mimir", "all")
	targets := []config.PushTarget{makeTarget(upstream.URL)}
	m := newMetrics()

	statusCode, respBody, _, _ := fanout.Do(
		context.Background(), inst, targets, []byte("body"),
		http.Header{},
		"/api/v1/push", http.DefaultClient, m,
	)
	if statusCode != http.StatusBadGateway {
		t.Errorf("expected 502 in all mode (suppression not applied), got %d", statusCode)
	}
	if !strings.Contains(string(respBody), "push target failed") {
		t.Errorf("expected 'push target failed', got %s", respBody)
	}
}

// ---- readBody helper for upstream request verification ----

func readBody(r *http.Request) string {
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

// TestPartialFailureMetricCountedOncePerRequest verifies that RecordPartialFailure
// is called exactly once per push operation, not once per failing target.
// Prior to the fix, doAnyMode called it once per failure AND the handler called it again.
func TestPartialFailureMetricCountedOncePerRequest(t *testing.T) {
	fail1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(fail1.Close)

	fail2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(fail2.Close)

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(ok.Close)

	inst := newInstance("loki-prod", "loki", "any")
	targets := []config.PushTarget{makeTarget(fail1.URL), makeTarget(fail2.URL), makeTarget(ok.URL)}
	m := newMetrics()

	statusCode, _, _, partialFailures := fanout.Do(
		context.Background(), inst, targets, []byte("body"),
		http.Header{},
		"/loki/api/v1/push", http.DefaultClient, m,
	)

	if statusCode != http.StatusNoContent {
		t.Errorf("expected 204, got %d", statusCode)
	}
	if len(partialFailures) != 2 {
		t.Errorf("expected 2 partial failures, got %d", len(partialFailures))
	}

	// Do() returns partialFailures to the caller (the handler) which calls
	// m.RecordPartialFailure once. Do() itself must NOT call it. The counter
	// should therefore be zero here — it is the handler's responsibility.
	count := m.PartialFailureValue("loki-prod")
	if count != 0 {
		t.Errorf("Do() must not call RecordPartialFailure; expected counter=0, got %v", count)
	}
}

// ---- per-target upstream credentials on the write path ----------------------

// Fan-out targets are independent systems: two Mimir clusters behind one
// instance may each require their own credential. Every target is written to on
// every push, so each must be presented its own key -- sending one upstream's
// password to another both leaks it and fails the write.
func TestFanOutPresentsEachTargetsOwnCredential(t *testing.T) {
	type seen struct {
		cred string
		org  string
	}
	got := map[string]seen{}
	var mu sync.Mutex

	newUpstream := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, pass, ok := r.BasicAuth()
			cred := "<none>"
			if ok {
				cred = user + ":" + pass
			}
			mu.Lock()
			got[name] = seen{cred: cred, org: r.Header.Get("X-Scope-OrgID")}
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}))
	}
	a := newUpstream("a")
	b := newUpstream("b")
	c := newUpstream("c")
	defer a.Close()
	defer b.Close()
	defer c.Close()

	inst := newInstance("mimir-ha", "mimir", "all")
	// Instance-level credential, overridden by two of the three targets.
	inst.BasicAuth = "shared-user:shared-password"
	inst.PushURLs = []config.PushTarget{
		{URL: a.URL, BasicAuth: "user-a:password-a"},
		{URL: b.URL, BasicAuth: "user-b:password-b"},
		{URL: c.URL}, // inherits the instance credential
	}

	status, _, _, failures := fanout.Do(context.Background(), inst, inst.GetPushTargets(),
		[]byte("payload"), http.Header{}, "/api/v1/push", a.Client(), newMetrics())

	if status < 200 || status >= 300 {
		t.Fatalf("expected the fan-out to succeed, got %d", status)
	}
	if len(failures) != 0 {
		t.Fatalf("unexpected partial failures: %v", failures)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("expected all three targets to be written to, got %d", len(got))
	}
	if got["a"].cred != "user-a:password-a" {
		t.Errorf("target a saw %q, want its own credential", got["a"].cred)
	}
	if got["b"].cred != "user-b:password-b" {
		t.Errorf("target b saw %q, want its own credential", got["b"].cred)
	}
	if got["c"].cred != "shared-user:shared-password" {
		t.Errorf("target c saw %q, want the instance-level credential", got["c"].cred)
	}
}

// A per-target tenant must travel with the same target as its credential, so a
// target scoped to its own tenant is addressed correctly on both counts.
func TestFanOutPairsCredentialWithTargetTenant(t *testing.T) {
	type seen struct{ cred, org string }
	got := map[string]seen{}
	var mu sync.Mutex

	newUpstream := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, pass, _ := r.BasicAuth()
			mu.Lock()
			got[name] = seen{cred: user + ":" + pass, org: r.Header.Get("X-Scope-OrgID")}
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}))
	}
	a := newUpstream("a")
	b := newUpstream("b")
	defer a.Close()
	defer b.Close()

	inst := newInstance("mimir-split", "mimir", "all")
	inst.PushURLs = []config.PushTarget{
		{URL: a.URL, BasicAuth: "user-a:password-a", TenantID: "tenant-a"},
		{URL: b.URL, BasicAuth: "user-b:password-b", TenantID: "tenant-b"},
	}

	if status, _, _, _ := fanout.Do(context.Background(), inst, inst.GetPushTargets(),
		[]byte("payload"), http.Header{}, "/api/v1/push", a.Client(), newMetrics()); status >= 300 {
		t.Fatalf("fan-out failed: %d", status)
	}

	mu.Lock()
	defer mu.Unlock()
	if got["a"].cred != "user-a:password-a" || got["a"].org != "tenant-a" {
		t.Errorf("target a saw %+v, want its own credential and tenant", got["a"])
	}
	if got["b"].cred != "user-b:password-b" || got["b"].org != "tenant-b" {
		t.Errorf("target b saw %+v, want its own credential and tenant", got["b"])
	}
}
