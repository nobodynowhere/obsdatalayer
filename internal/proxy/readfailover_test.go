package proxy_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/metrics"
	"obsdatalayer/internal/proxy"
)

// countingTarget is an upstream that records how many times it was called and
// answers with whatever the test tells it to.
type countingTarget struct {
	srv    *httptest.Server
	calls  atomic.Int64
	status atomic.Int64
	body   atomic.Value // string, written by tests while the handler reads it
}

func newTarget(t *testing.T, status int, body string) *countingTarget {
	t.Helper()
	tgt := &countingTarget{}
	tgt.status.Store(int64(status))
	tgt.body.Store(body)
	tgt.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tgt.calls.Add(1)
		code := int(tgt.status.Load())
		w.Header().Set("X-Served-By", tgt.srv.URL)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, tgt.body.Load().(string))
	}))
	t.Cleanup(tgt.srv.Close)
	return tgt
}

// answer switches what the target returns from here on.
func (c *countingTarget) answer(status int, body string) {
	c.status.Store(int64(status))
	c.body.Store(body)
}

// deadTarget is an address nothing is listening on, to produce a transport
// failure rather than an HTTP status.
func deadTarget(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // close immediately so connections are refused
	return url
}

func fanoutInstance(name string, urls ...string) *config.InstanceConfig {
	targets := make([]config.PushTarget, len(urls))
	for i, u := range urls {
		targets[i] = config.PushTarget{URL: u}
	}
	return &config.InstanceConfig{Name: name, Backend: "loki", PushURLs: targets, FanOutMode: "any"}
}

func doQuery(p *proxy.Proxy, inst *config.InstanceConfig) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/loki/api/v1/labels", nil)
	rec := httptest.NewRecorder()
	p.ForwardQuery(rec, req, inst, "/loki/api/v1/labels")
	return rec
}

// The defect: every read went to the first push target, so a target being down
// failed reads outright even though a healthy replica was configured.
func TestReadFailsOverWhenFirstTargetIsDown(t *testing.T) {
	dead := deadTarget(t)
	live := newTarget(t, http.StatusOK, "labels-from-live")
	p := newProxy(&http.Client{Timeout: 5 * time.Second})

	rec := doQuery(p, fanoutInstance("loki-ha", dead, live.srv.URL))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected the read to succeed against the second target, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "labels-from-live" {
		t.Errorf("body = %q, want the live target's answer", got)
	}
	if got := live.calls.Load(); got != 1 {
		t.Errorf("live target calls = %d, want 1", got)
	}
}

func TestReadFailsOverOnUpstream5xx(t *testing.T) {
	broken := newTarget(t, http.StatusBadGateway, "upstream is unwell")
	live := newTarget(t, http.StatusOK, "labels-from-live")
	p := newProxy(&http.Client{Timeout: 5 * time.Second})

	rec := doQuery(p, fanoutInstance("loki-ha", broken.srv.URL, live.srv.URL))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected failover past the 5xx, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != "labels-from-live" {
		t.Errorf("body = %q, want the live target's answer", got)
	}
	if got := broken.calls.Load(); got != 1 {
		t.Errorf("broken target calls = %d, want 1", got)
	}
}

// A 4xx is the upstream answering. Asking a replica the same malformed question
// returns the same answer while doubling the work, and a 404 from a query
// endpoint is a legitimate result rather than an outage.
func TestReadDoesNotFailOverOn4xx(t *testing.T) {
	first := newTarget(t, http.StatusNotFound, "no such series")
	second := newTarget(t, http.StatusOK, "should not be reached")
	p := newProxy(&http.Client{Timeout: 5 * time.Second})

	rec := doQuery(p, fanoutInstance("loki-ha", first.srv.URL, second.srv.URL))

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected the 404 to be returned as-is, got %d", rec.Code)
	}
	if got := second.calls.Load(); got != 0 {
		t.Errorf("second target was called %d times for a 4xx", got)
	}
}

// When nothing can answer, the client gets a real upstream status rather than a
// synthesised one.
func TestReadReportsLastFailureWhenAllTargetsFail(t *testing.T) {
	first := newTarget(t, http.StatusBadGateway, "first is unwell")
	second := newTarget(t, http.StatusServiceUnavailable, "second is unwell")
	p := newProxy(&http.Client{Timeout: 5 * time.Second})

	rec := doQuery(p, fanoutInstance("loki-ha", first.srv.URL, second.srv.URL))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected the last target's status, got %d", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, "second is unwell") {
		t.Errorf("expected the last target's body, got %q", got)
	}
	if first.calls.Load() != 1 || second.calls.Load() != 1 {
		t.Errorf("expected both targets to be tried, got %d and %d", first.calls.Load(), second.calls.Load())
	}
}

// A single-target instance keeps behaving exactly as before.
func TestSingleTargetReadIsUnchanged(t *testing.T) {
	only := newTarget(t, http.StatusOK, "the answer")
	p := newProxy(&http.Client{Timeout: 5 * time.Second})

	rec := doQuery(p, newInst("loki-solo", "loki", only.srv.URL))

	if rec.Code != http.StatusOK || rec.Body.String() != "the answer" {
		t.Errorf("got %d %q", rec.Code, rec.Body.String())
	}
	if got := only.calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1", got)
	}
}

// Health memory: after enough consecutive failures a dead target is skipped, so
// one dead replica does not add a connection timeout to every read.
func TestFailingTargetIsSkippedAfterRepeatedFailures(t *testing.T) {
	broken := newTarget(t, http.StatusBadGateway, "unwell")
	live := newTarget(t, http.StatusOK, "ok")
	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	inst := fanoutInstance("loki-ha", broken.srv.URL, live.srv.URL)

	// Drive the broken target past the failure threshold.
	for i := 0; i < 4; i++ {
		if rec := doQuery(p, inst); rec.Code != http.StatusOK {
			t.Fatalf("read %d: expected failover to succeed, got %d", i, rec.Code)
		}
	}
	callsBefore := broken.calls.Load()

	// Subsequent reads should go straight to the healthy target.
	for i := 0; i < 5; i++ {
		if rec := doQuery(p, inst); rec.Code != http.StatusOK {
			t.Fatalf("read %d: %d", i, rec.Code)
		}
	}

	if got := broken.calls.Load(); got != callsBefore {
		t.Errorf("the cooled-off target was dialled %d more times; expected it to be skipped",
			got-callsBefore)
	}
	if live.calls.Load() < 9 {
		t.Errorf("healthy target served %d reads, expected all of them", live.calls.Load())
	}
}

// A target that recovers must be used again rather than being written off.
func TestRecoveredTargetIsUsedAgain(t *testing.T) {
	flaky := newTarget(t, http.StatusBadGateway, "unwell")
	live := newTarget(t, http.StatusOK, "from-live")
	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	inst := fanoutInstance("loki-ha", flaky.srv.URL, live.srv.URL)

	now := time.Now()
	proxy.SetHealthClock(p, func() time.Time { return now })

	for i := 0; i < 3; i++ {
		doQuery(p, inst)
	}
	if rec := doQuery(p, inst); rec.Body.String() != "from-live" {
		t.Fatalf("expected the failing target to be skipped, got %q", rec.Body.String())
	}
	flaky.answer(http.StatusOK, "from-recovered")

	// Still inside the cool-off: the recovered target is not preferred yet.
	if rec := doQuery(p, inst); rec.Body.String() != "from-live" {
		t.Errorf("cooled-off target was preferred before its window elapsed: %q", rec.Body.String())
	}

	// Past the cool-off, the configured order is restored.
	now = now.Add(proxy.ReadCoolOff + time.Second)
	rec := doQuery(p, inst)
	if rec.Code != http.StatusOK {
		t.Fatalf("read failed after recovery: %d", rec.Code)
	}
	if got := rec.Body.String(); got != "from-recovered" {
		t.Errorf("body = %q, want the recovered target to be preferred again", got)
	}
}

// A form-encoded query is a read with a body. Failing over must replay it, or
// the second target is asked a different question than the first.
func TestPostQueryBodyIsReplayedOnFailover(t *testing.T) {
	var got atomic.Value
	dead := deadTarget(t)
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		got.Store(string(payload))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer live.Close()

	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	const query = `query={job="api"}&start=1700000000&end=1700003600`
	req := httptest.NewRequest(http.MethodPost, "/loki/api/v1/query_range", strings.NewReader(query))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	p.ForwardQuery(rec, req, fanoutInstance("loki-ha", dead, live.URL), "/loki/api/v1/query_range")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected the replayed query to succeed, got %d: %s", rec.Code, rec.Body.String())
	}
	if got.Load() != query {
		t.Errorf("second target received %q, want the original query %q", got.Load(), query)
	}
}

// The configured query timeout bounds the whole read, not each attempt, so a
// caller's worst case does not scale with how many targets are configured.
func TestReadTimeoutIsSharedAcrossTargets(t *testing.T) {
	slow := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
	}
	a, b, c := slow(), slow(), slow()
	defer a.Close()
	defer b.Close()
	defer c.Close()

	const budget = 600 * time.Millisecond
	p := newProxy(&http.Client{Timeout: budget})
	inst := fanoutInstance("loki-slow", a.URL, b.URL, c.URL)

	start := time.Now()
	rec := doQuery(p, inst)
	elapsed := time.Since(start)

	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("expected 504, got %d", rec.Code)
	}
	// Three targets must not mean three full timeouts. Allow generous slack for
	// scheduling while still failing if the budget is applied per attempt.
	if elapsed > 2*budget {
		t.Errorf("read took %v with a %v budget across 3 targets; the timeout is per attempt, not shared", elapsed, budget)
	}
}

// Response headers from the target that answered must reach the client, and the
// discarded attempt's headers must not.
func TestFailoverCommitsOnlyTheAnsweringTargetsHeaders(t *testing.T) {
	broken := newTarget(t, http.StatusBadGateway, "unwell")
	live := newTarget(t, http.StatusOK, "ok")
	p := newProxy(&http.Client{Timeout: 5 * time.Second})

	rec := doQuery(p, fanoutInstance("loki-ha", broken.srv.URL, live.srv.URL))

	served := rec.Header().Get("X-Served-By")
	if served != live.srv.URL {
		t.Errorf("X-Served-By = %q, want the live target %q", served, live.srv.URL)
	}
	if n := len(rec.Header().Values("X-Served-By")); n != 1 {
		t.Errorf("expected exactly one X-Served-By header, got %d", n)
	}
}

// Every target being in cool-off must not mean reads stop: a target that might
// have recovered is a better answer than no answer at all.
func TestAllTargetsCoolingOffStillAttemptsRead(t *testing.T) {
	first := newTarget(t, http.StatusBadGateway, "unwell")
	second := newTarget(t, http.StatusBadGateway, "unwell")
	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	inst := fanoutInstance("loki-ha", first.srv.URL, second.srv.URL)

	for i := 0; i < 3; i++ {
		doQuery(p, inst)
	}
	first.answer(http.StatusOK, "recovered")

	rec := doQuery(p, inst)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected a read to still be attempted with every target cooling off, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != "recovered" {
		t.Errorf("body = %q, want %q", got, "recovered")
	}
}

func TestReadWithNoTargetsIsAnError(t *testing.T) {
	p := newProxy(&http.Client{Timeout: time.Second})
	inst := &config.InstanceConfig{Name: "empty", Backend: "loki"}

	rec := doQuery(p, inst)
	if rec.Code < 400 {
		t.Errorf("expected an error for an instance with no target, got %d", rec.Code)
	}
}

// ---- read counters ----------------------------------------------------------

// A failover must be visible in the counters as both a failure against the
// target that broke and a success against the one that covered, or the Overview
// page shows a healthy gateway while a replica is down.
func TestFailoverRecordsPerTargetCounters(t *testing.T) {
	broken := newTarget(t, http.StatusBadGateway, "unwell")
	live := newTarget(t, http.StatusOK, "ok")
	m := metrics.New(prometheus.NewRegistry())
	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	p.SetMetrics(m)

	rec := doQuery(p, fanoutInstance("loki-ha", broken.srv.URL, live.srv.URL))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected the read to be served, got %d", rec.Code)
	}

	if got := m.ReadValue("loki-ha", broken.srv.URL, "failure"); got != 1 {
		t.Errorf("failure count for the broken target = %d, want 1", got)
	}
	if got := m.ReadValue("loki-ha", live.srv.URL, "success"); got != 1 {
		t.Errorf("success count for the live target = %d, want 1", got)
	}
	if got := m.ReadFailoverValue("loki-ha"); got != 1 {
		t.Errorf("failover count = %d, want 1", got)
	}
}

// A read served by the preferred target is not a failover, or the metric would
// fire on every healthy request and mean nothing.
func TestHealthyReadRecordsNoFailover(t *testing.T) {
	live := newTarget(t, http.StatusOK, "ok")
	other := newTarget(t, http.StatusOK, "ok")
	m := metrics.New(prometheus.NewRegistry())
	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	p.SetMetrics(m)

	doQuery(p, fanoutInstance("loki-ha", live.srv.URL, other.srv.URL))

	if got := m.ReadValue("loki-ha", live.srv.URL, "success"); got != 1 {
		t.Errorf("success count = %d, want 1", got)
	}
	if got := m.ReadFailoverValue("loki-ha"); got != 0 {
		t.Errorf("failover recorded for a healthy read: %d", got)
	}
}

// Every target failing is still a failover: the gateway did try more than one.
func TestAllTargetsFailingRecordsFailures(t *testing.T) {
	first := newTarget(t, http.StatusBadGateway, "unwell")
	second := newTarget(t, http.StatusBadGateway, "unwell")
	m := metrics.New(prometheus.NewRegistry())
	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	p.SetMetrics(m)

	doQuery(p, fanoutInstance("loki-ha", first.srv.URL, second.srv.URL))

	if got := m.ReadValue("loki-ha", first.srv.URL, "failure"); got != 1 {
		t.Errorf("first failure count = %d, want 1", got)
	}
	if got := m.ReadValue("loki-ha", second.srv.URL, "failure"); got != 1 {
		t.Errorf("second failure count = %d, want 1", got)
	}
	if got := m.ReadFailoverValue("loki-ha"); got != 1 {
		t.Errorf("failover count = %d, want 1", got)
	}
}

// A 4xx is the upstream answering, so it counts as a served read rather than a
// target failure -- otherwise a dashboard would show replicas as unhealthy
// every time a user typed a bad query.
func TestClientErrorCountsAsServed(t *testing.T) {
	only := newTarget(t, http.StatusBadRequest, "bad query")
	m := metrics.New(prometheus.NewRegistry())
	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	p.SetMetrics(m)

	doQuery(p, fanoutInstance("loki-ha", only.srv.URL))

	if got := m.ReadValue("loki-ha", only.srv.URL, "success"); got != 1 {
		t.Errorf("a 4xx was not counted as served: %d", got)
	}
	if got := m.ReadValue("loki-ha", only.srv.URL, "failure"); got != 0 {
		t.Errorf("a 4xx was counted as a target failure: %d", got)
	}
}

// A Proxy with no metrics sink must not panic: that is how every other test and
// the pre-wiring path constructs one.
func TestReadWithoutMetricsSinkDoesNotPanic(t *testing.T) {
	live := newTarget(t, http.StatusOK, "ok")
	p := newProxy(&http.Client{Timeout: 5 * time.Second})

	if rec := doQuery(p, fanoutInstance("loki-ha", live.srv.URL)); rec.Code != http.StatusOK {
		t.Errorf("got %d", rec.Code)
	}
}

// ---- per-target upstream credentials ----------------------------------------
//
// Fan-out targets are independent systems. Two Mimir clusters behind one
// instance may each want their own credential, and failover must present the
// credential belonging to the target it is actually talking to.

// credentialTarget records the Authorization header it was presented.
type credentialTarget struct {
	srv  *httptest.Server
	seen atomic.Value // string
}

func newCredentialTarget(t *testing.T, status int) *credentialTarget {
	t.Helper()
	tgt := &credentialTarget{}
	tgt.seen.Store("")
	tgt.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if ok {
			tgt.seen.Store(user + ":" + pass)
		} else {
			tgt.seen.Store("<none>")
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(tgt.srv.Close)
	return tgt
}

func (c *credentialTarget) credential() string { return c.seen.Load().(string) }

// Failing over must not carry the first target's credential to the second.
// Sending one upstream's password to another would leak it and fail the call.
func TestFailoverPresentsEachTargetsOwnCredential(t *testing.T) {
	first := newCredentialTarget(t, http.StatusBadGateway)
	second := newCredentialTarget(t, http.StatusOK)

	inst := &config.InstanceConfig{
		Name:       "mimir-ha",
		Backend:    "mimir",
		FanOutMode: "any",
		PushURLs: []config.PushTarget{
			{URL: first.srv.URL, BasicAuth: "user-a:password-a"},
			{URL: second.srv.URL, BasicAuth: "user-b:password-b"},
		},
	}

	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	if rec := doQuery(p, inst); rec.Code != http.StatusOK {
		t.Fatalf("expected failover to succeed, got %d", rec.Code)
	}

	if got := first.credential(); got != "user-a:password-a" {
		t.Errorf("first target saw %q, want its own credential", got)
	}
	if got := second.credential(); got != "user-b:password-b" {
		t.Errorf("second target saw %q, want its own credential (not the first target's)", got)
	}
}

// A target without its own credential inherits the instance's; one that has its
// own overrides it. Both must hold on the read path, including after failover.
func TestReadCredentialFallsBackToInstanceLevel(t *testing.T) {
	first := newCredentialTarget(t, http.StatusBadGateway)
	second := newCredentialTarget(t, http.StatusOK)

	inst := &config.InstanceConfig{
		Name:       "mimir-ha",
		Backend:    "mimir",
		FanOutMode: "any",
		BasicAuth:  "shared-user:shared-password",
		PushURLs: []config.PushTarget{
			{URL: first.srv.URL, BasicAuth: "override-user:override-password"},
			{URL: second.srv.URL}, // inherits the instance credential
		},
	}

	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	if rec := doQuery(p, inst); rec.Code != http.StatusOK {
		t.Fatalf("expected failover to succeed, got %d", rec.Code)
	}

	if got := first.credential(); got != "override-user:override-password" {
		t.Errorf("first target saw %q, want its own override", got)
	}
	if got := second.credential(); got != "shared-user:shared-password" {
		t.Errorf("second target saw %q, want the instance-level credential", got)
	}
}

// A single-target instance presents the instance credential, unchanged.
func TestSingleTargetReadPresentsInstanceCredential(t *testing.T) {
	only := newCredentialTarget(t, http.StatusOK)
	inst := &config.InstanceConfig{
		Name:      "mimir-solo",
		Backend:   "mimir",
		URL:       only.srv.URL,
		BasicAuth: "solo-user:solo-password",
	}

	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	if rec := doQuery(p, inst); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if got := only.credential(); got != "solo-user:solo-password" {
		t.Errorf("target saw %q", got)
	}
}

// An instance with no credential configured must present none, rather than
// leaking a neighbouring target's.
func TestReadWithoutCredentialPresentsNone(t *testing.T) {
	first := newCredentialTarget(t, http.StatusBadGateway)
	second := newCredentialTarget(t, http.StatusOK)

	inst := &config.InstanceConfig{
		Name:       "mimir-open",
		Backend:    "mimir",
		FanOutMode: "any",
		PushURLs: []config.PushTarget{
			{URL: first.srv.URL, BasicAuth: "user-a:password-a"},
			{URL: second.srv.URL},
		},
	}

	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	doQuery(p, inst)

	if got := second.credential(); got != "<none>" {
		t.Errorf("target with no credential saw %q, want none presented", got)
	}
}

// The failure mode must not change which credential is presented. A timeout is
// the case where a stale credential is most plausible -- the request to the
// first target was already built and in flight when it expired.
func TestTimeoutFailoverPresentsSecondTargetsCredential(t *testing.T) {
	// MIMIR1 accepts the connection and then never answers.
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hang.Close()

	// MIMIR2 records what it was presented and answers.
	mimir2 := newCredentialTarget(t, http.StatusOK)

	inst := &config.InstanceConfig{
		Name:       "mimir-ha",
		Backend:    "mimir",
		FanOutMode: "any",
		PushURLs: []config.PushTarget{
			{URL: hang.URL, BasicAuth: "mimir1-user:password1"},
			{URL: mimir2.srv.URL, BasicAuth: "mimir2-user:password2"},
		},
	}

	// A budget large enough to cover both attempts once the first has expired.
	p := newProxy(&http.Client{Timeout: 3 * time.Second})
	rec := doQuery(p, inst)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected the second target to serve the read, got %d", rec.Code)
	}
	if got := mimir2.credential(); got != "mimir2-user:password2" {
		t.Errorf("MIMIR2 was presented %q, want its own password2", got)
	}
}

// An upstream that rejects the wrong credential proves the point end to end:
// if the gateway carried MIMIR1's password over, MIMIR2 would 401 and the read
// would fail rather than succeed.
func TestFailoverSucceedsAgainstCredentialCheckingUpstream(t *testing.T) {
	mimir1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer mimir1.Close()

	mimir2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "mimir2-user" || pass != "password2" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "wrong credential")
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "series-from-mimir2")
	}))
	defer mimir2.Close()

	inst := &config.InstanceConfig{
		Name:       "mimir-ha",
		Backend:    "mimir",
		FanOutMode: "any",
		PushURLs: []config.PushTarget{
			{URL: mimir1.URL, BasicAuth: "mimir1-user:password1"},
			{URL: mimir2.URL, BasicAuth: "mimir2-user:password2"},
		},
	}

	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	rec := doQuery(p, inst)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from MIMIR2, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "series-from-mimir2" {
		t.Errorf("body = %q, want MIMIR2's answer", got)
	}
}

// Transparency: the caller sees one ordinary response. Nothing in it reveals
// that a target failed, which target served, or any upstream credential.
func TestFailoverIsTransparentToTheCaller(t *testing.T) {
	mimir1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Mimir-Node", "mimir1")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "mimir1 is unwell")
	}))
	defer mimir1.Close()

	mimir2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"success","data":[]}`)
	}))
	defer mimir2.Close()

	inst := &config.InstanceConfig{
		Name:       "mimir-ha",
		Backend:    "mimir",
		FanOutMode: "any",
		PushURLs: []config.PushTarget{
			{URL: mimir1.URL, BasicAuth: "mimir1-user:password1"},
			{URL: mimir2.URL, BasicAuth: "mimir2-user:password2"},
		},
	}

	p := newProxy(&http.Client{Timeout: 5 * time.Second})
	rec := doQuery(p, inst)

	if rec.Code != http.StatusOK {
		t.Fatalf("caller saw %d; a served read must look like a served read", rec.Code)
	}
	if got := rec.Body.String(); got != `{"status":"success","data":[]}` {
		t.Errorf("caller body = %q, want only MIMIR2's answer", got)
	}

	// The failed attempt must leave no trace in what the caller receives.
	dump := fmt.Sprintf("%v %s", rec.Header(), rec.Body.String())
	for _, leak := range []string{"password1", "password2", "mimir1-user", "mimir2-user", "mimir1 is unwell", "X-Mimir-Node"} {
		if strings.Contains(dump, leak) {
			t.Errorf("response leaks %q to the caller: %s", leak, dump)
		}
	}
	// Nor should the gateway invent a header announcing the failover.
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want MIMIR2's own header passed through", got)
	}
}
