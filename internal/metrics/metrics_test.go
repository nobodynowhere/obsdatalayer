package metrics_test

import (
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"obsdatalayer/internal/metrics"
)

// instanceMetrics are the series keyed by instance, and so the only ones
// RetainInstances is concerned with. The authentication counters are
// process-wide and deliberately outside that lifecycle, so the tests below
// filter to these names rather than counting everything the collector exports.
var instanceMetrics = []string{
	"gateway_fanout_requests_total",
	"gateway_suppressed_errors_total",
	"gateway_partial_failures_total",
	"gateway_write_items_total",
	"gateway_rewrite_labels_total",
}

// TestNewWithFreshRegistry verifies New can be called multiple times without panicking,
// as long as each call uses a distinct registry.
func TestNewWithFreshRegistry(t *testing.T) {
	m1 := metrics.New(prometheus.NewRegistry())
	m2 := metrics.New(prometheus.NewRegistry())

	if m1 == nil || m2 == nil {
		t.Fatal("expected non-nil Metrics")
	}
	// A fresh collector exports nothing until something is recorded, but it must
	// still describe every metric it owns.
	if got := testutil.CollectAndCount(m1, instanceMetrics...); got != 0 {
		t.Fatalf("expected a fresh collector to export no instance series, got %d", got)
	}
	m1.RecordPartialFailure("inst")
	if got := testutil.CollectAndCount(m1, instanceMetrics...); got != 1 {
		t.Fatalf("expected 1 series after recording, got %d", got)
	}
	if got := testutil.CollectAndCount(m2, instanceMetrics...); got != 0 {
		t.Fatalf("expected the second collector to be independent, got %d", got)
	}
}

// TestNewPanicsOnDuplicateRegistration verifies that registering into the same
// registry twice panics, confirming the previous production behaviour was broken.
func TestNewPanicsOnDuplicateRegistration(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics.New(reg) // first registration is fine

	defer func() {
		if recover() == nil {
			t.Error("expected panic on duplicate registration, got none")
		}
	}()
	metrics.New(reg) // second registration must panic
}

func TestRecordFanout(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())
	// should not panic
	m.RecordFanout("inst", "http://target", 204)
	m.RecordFanout("inst", "http://target", 0)
}

func TestRecordSuppressed(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())
	m.RecordSuppressed("inst", "http://target", "out of order sample")
}

func TestRecordPartialFailure(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())
	m.RecordPartialFailure("inst")
}

func TestRecordWriteItems(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())
	m.RecordWriteItems("mimir", "inst", "series", "forwarded", 3)
	m.RecordWriteItems("mimir", "inst", "series", "forwarded", 0)

	count := m.WriteItemsValue("mimir", "inst", "series", "forwarded")
	if count != 3 {
		t.Fatalf("expected forwarded item count 3, got %v", count)
	}
}

func TestRecordRewriteLabels(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())
	m.RecordRewriteLabels("mimir", "inst", "dropped", 2)
	m.RecordRewriteLabels("mimir", "inst", "dropped", -1)

	count := m.RewriteLabelsValue("mimir", "inst", "dropped")
	if count != 2 {
		t.Fatalf("expected dropped label count 2, got %v", count)
	}
}

// TestCollectExportsCorrectLabels pins the exposition output. The Value
// accessors read through the same key structs the recorders write, so they
// cannot catch a mismatch between a key's values() order and its Desc's label
// order -- that mistake would silently export the right numbers under swapped
// label names, and only a scrape reveals it.
func TestCollectExportsCorrectLabels(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())
	m.RecordFanout("inst-a", "http://target-1", 204)
	m.RecordSuppressed("inst-a", "http://target-1", "out of order sample")
	m.RecordPartialFailure("inst-a")
	m.RecordWriteItems("mimir", "inst-a", "series", "forwarded", 7)
	m.RecordRewriteLabels("mimir", "inst-a", "dropped", 3)

	expected := `
# HELP gateway_fanout_requests_total Fan-out push attempts, labeled by instance, target, status.
# TYPE gateway_fanout_requests_total counter
gateway_fanout_requests_total{instance="inst-a",status="204",target="http://target-1"} 1
# HELP gateway_suppressed_errors_total Mimir 400 responses suppressed in any mode, labeled by instance, target, pattern.
# TYPE gateway_suppressed_errors_total counter
gateway_suppressed_errors_total{instance="inst-a",pattern="out of order sample",target="http://target-1"} 1
# HELP gateway_partial_failures_total Pushes that returned success with X-Gateway-Partial-Failure header, labeled by instance.
# TYPE gateway_partial_failures_total counter
gateway_partial_failures_total{instance="inst-a"} 1
# HELP gateway_write_items_total Write payload items observed by the gateway, labeled by backend, instance, item kind, and result.
# TYPE gateway_write_items_total counter
gateway_write_items_total{backend="mimir",instance="inst-a",kind="series",result="forwarded"} 7
# HELP gateway_rewrite_labels_total Labels changed by gateway write-payload rewriting, labeled by backend, instance, and operation.
# TYPE gateway_rewrite_labels_total counter
gateway_rewrite_labels_total{backend="mimir",instance="inst-a",operation="dropped"} 3
# HELP gateway_auth_rejected_total Requests rejected before their credentials were checked, labeled by reason: throttled (the source had failed too often) or saturated (no password-hashing slot was free).
# TYPE gateway_auth_rejected_total counter
gateway_auth_rejected_total{reason="saturated"} 0
gateway_auth_rejected_total{reason="throttled"} 0
# HELP gateway_auth_failures_total Credential checks that ran and were rejected.
# TYPE gateway_auth_failures_total counter
gateway_auth_failures_total 0
# HELP gateway_config_reload_failures_total Config reloads that failed, from any trigger: the ticker, SIGHUP, the admin API, or a mutation applying itself. The live config is retained on failure.
# TYPE gateway_config_reload_failures_total counter
gateway_config_reload_failures_total 0
`
	// gateway_config_last_successful_reload_seconds is deliberately not here:
	// no reload has succeeded in this test, and the gauge is absent until one
	// has. See TestConfigReloadGaugeAbsentUntilFirstSuccess.
	if err := testutil.CollectAndCompare(m, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}
}

// The authentication counters carry no instance label and are exported from
// process start, zeros included, so an alert on their rate has a series to
// attach to before the gateway has ever been attacked.
func TestAuthCountersAlwaysExported(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())

	expected := `
# HELP gateway_auth_rejected_total Requests rejected before their credentials were checked, labeled by reason: throttled (the source had failed too often) or saturated (no password-hashing slot was free).
# TYPE gateway_auth_rejected_total counter
gateway_auth_rejected_total{reason="saturated"} 2
gateway_auth_rejected_total{reason="throttled"} 1
# HELP gateway_auth_failures_total Credential checks that ran and were rejected.
# TYPE gateway_auth_failures_total counter
gateway_auth_failures_total 3
`
	m.RecordAuthRejected("throttled")
	m.RecordAuthRejected("saturated")
	m.RecordAuthRejected("saturated")
	for i := 0; i < 3; i++ {
		m.RecordAuthFailure()
	}

	if err := testutil.CollectAndCompare(m, strings.NewReader(expected),
		"gateway_auth_rejected_total", "gateway_auth_failures_total"); err != nil {
		t.Fatal(err)
	}
}

// TestRetainInstancesDropsRemovedSeries is the reason this package owns its
// counters: a CounterVec would keep exporting inst-b forever.
func TestRetainInstancesDropsRemovedSeries(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())
	for _, inst := range []string{"inst-a", "inst-b"} {
		m.RecordFanout(inst, "http://target-1", 204)
		m.RecordSuppressed(inst, "http://target-1", "out of order sample")
		m.RecordPartialFailure(inst)
		m.RecordWriteItems("mimir", inst, "series", "forwarded", 2)
		m.RecordRewriteLabels("mimir", inst, "dropped", 1)
	}
	if got := testutil.CollectAndCount(m, instanceMetrics...); got != 10 {
		t.Fatalf("expected 10 series before pruning, got %d", got)
	}

	m.RetainInstances([]string{"inst-a"})

	if got := testutil.CollectAndCount(m, instanceMetrics...); got != 5 {
		t.Fatalf("expected 5 series after pruning, got %d", got)
	}
	if got := m.PartialFailureValue("inst-b"); got != 0 {
		t.Errorf("expected removed instance to read 0, got %d", got)
	}
	// The survivor must be untouched: pruning is not a reset.
	if got := m.PartialFailureValue("inst-a"); got != 1 {
		t.Errorf("expected surviving instance to keep its value, got %d", got)
	}
	if got := m.WriteItemsValue("mimir", "inst-a", "series", "forwarded"); got != 2 {
		t.Errorf("expected surviving write items 2, got %d", got)
	}
}

// TestRetainInstancesWithNoLiveInstancesDropsAll covers the empty-config case,
// which must not be mistaken for "retain everything".
func TestRetainInstancesWithNoLiveInstancesDropsAll(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())
	m.RecordFanout("inst-a", "http://target-1", 204)
	m.RecordAuthFailure()
	m.RetainInstances(nil)

	if got := testutil.CollectAndCount(m, instanceMetrics...); got != 0 {
		t.Fatalf("expected all instance series dropped, got %d", got)
	}
	// Pruning is scoped to instances. The authentication counters have no
	// instance to be absent from, and wiping them on every reload would read to
	// Prometheus as a counter reset.
	if got := m.AuthFailureValue(); got != 1 {
		t.Errorf("expected the auth counter to survive pruning, got %d", got)
	}
}

func TestSummaryAggregates(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())
	// inst-a: one success, one 500, one transport error (status 0).
	m.RecordFanout("inst-a", "http://target-1", 204)
	m.RecordFanout("inst-a", "http://target-1", 500)
	m.RecordFanout("inst-a", "http://target-2", 0)
	m.RecordSuppressed("inst-a", "http://target-1", "out of order sample")
	m.RecordPartialFailure("inst-a")
	m.RecordWriteItems("mimir", "inst-a", "series", "received", 10)
	m.RecordWriteItems("mimir", "inst-a", "series", "forwarded", 9)
	m.RecordRewriteLabels("mimir", "inst-a", "dropped", 2)
	m.RecordRewriteLabels("mimir", "inst-a", "injected", 3)
	// inst-b: one success only.
	m.RecordFanout("inst-b", "http://target-3", 200)

	s := m.Summary()
	if s.FanoutRequests != 4 {
		t.Errorf("expected 4 fan-out requests, got %d", s.FanoutRequests)
	}
	// 500 and the transport error count; 204 and 200 do not.
	if s.FanoutFailures != 2 {
		t.Errorf("expected 2 fan-out failures, got %d", s.FanoutFailures)
	}
	if s.SuppressedErrors != 1 {
		t.Errorf("expected 1 suppressed error, got %d", s.SuppressedErrors)
	}
	if s.PartialFailures != 1 {
		t.Errorf("expected 1 partial failure, got %d", s.PartialFailures)
	}
	// Only "forwarded" counts, not "received".
	if s.ItemsForwarded != 9 {
		t.Errorf("expected 9 items forwarded, got %d", s.ItemsForwarded)
	}
	if s.LabelsRewritten != 5 {
		t.Errorf("expected 5 labels rewritten, got %d", s.LabelsRewritten)
	}

	if len(s.Instances) != 2 {
		t.Fatalf("expected 2 instances, got %d", len(s.Instances))
	}
	if s.Instances[0].Instance != "inst-a" || s.Instances[1].Instance != "inst-b" {
		t.Fatalf("expected instances sorted by name, got %q then %q",
			s.Instances[0].Instance, s.Instances[1].Instance)
	}
	if s.Instances[0].FanoutFailures != 2 || s.Instances[1].FanoutFailures != 0 {
		t.Errorf("failures attributed to the wrong instance: %+v", s.Instances)
	}
}

// TestConcurrentRecordAndCollect exercises the lock/atomic split under -race:
// the first writer for a key takes the write lock while others take the read
// lock, and Collect snapshots concurrently with both.
func TestConcurrentRecordAndCollect(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())
	const writers, perWriter = 8, 200

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				m.RecordFanout("inst-a", "http://target-1", 204)
				m.RecordWriteItems("mimir", "inst-a", "series", "forwarded", 1)
			}
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			testutil.CollectAndCount(m)
			m.Summary()
		}
	}()
	wg.Wait()

	if got := m.FanoutValue("inst-a", "http://target-1", 204); got != writers*perWriter {
		t.Errorf("expected %d, got %d -- lost increments", writers*perWriter, got)
	}
	if got := m.WriteItemsValue("mimir", "inst-a", "series", "forwarded"); got != writers*perWriter {
		t.Errorf("expected %d, got %d -- lost increments", writers*perWriter, got)
	}
}

// ---- read counters ----------------------------------------------------------

func TestReadCountersRecordPerTargetOutcome(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())

	m.RecordRead("loki-ha", "http://a.local", false)
	m.RecordRead("loki-ha", "http://b.local", true)
	m.RecordRead("loki-ha", "http://b.local", true)
	m.RecordReadFailover("loki-ha")

	if got := m.ReadValue("loki-ha", "http://a.local", "failure"); got != 1 {
		t.Errorf("failures for a = %d, want 1", got)
	}
	if got := m.ReadValue("loki-ha", "http://b.local", "success"); got != 2 {
		t.Errorf("successes for b = %d, want 2", got)
	}
	if got := m.ReadFailoverValue("loki-ha"); got != 1 {
		t.Errorf("failovers = %d, want 1", got)
	}
}

func TestReadCountersExportedWithLabels(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())
	m.RecordRead("loki-ha", "http://a.local", false)
	m.RecordRead("loki-ha", "http://b.local", true)
	m.RecordReadFailover("loki-ha")

	expected := `
# HELP gateway_read_requests_total Proxied read attempts, labeled by instance, the upstream target asked, and result (success or failure).
# TYPE gateway_read_requests_total counter
gateway_read_requests_total{instance="loki-ha",result="failure",target="http://a.local"} 1
gateway_read_requests_total{instance="loki-ha",result="success",target="http://b.local"} 1
# HELP gateway_read_failovers_total Reads that moved on to another target after one failed, labeled by instance.
# TYPE gateway_read_failovers_total counter
gateway_read_failovers_total{instance="loki-ha"} 1
`
	if err := testutil.CollectAndCompare(m, strings.NewReader(expected),
		"gateway_read_requests_total", "gateway_read_failovers_total"); err != nil {
		t.Fatal(err)
	}
}

// The summary is what the Overview page renders, so it must both total the
// reads and break them down by target -- an operator needs to see which replica
// is unwell, not only that something is.
func TestSummaryIncludesReadBreakdown(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())
	m.RecordRead("loki-ha", "http://a.local", false)
	m.RecordRead("loki-ha", "http://a.local", false)
	m.RecordRead("loki-ha", "http://b.local", true)
	m.RecordReadFailover("loki-ha")

	s := m.Summary()
	if s.ReadSuccesses != 1 || s.ReadFailures != 2 || s.ReadFailovers != 1 {
		t.Fatalf("totals = %d ok / %d failed / %d failovers, want 1/2/1",
			s.ReadSuccesses, s.ReadFailures, s.ReadFailovers)
	}
	if len(s.Instances) != 1 {
		t.Fatalf("expected 1 instance summary, got %d", len(s.Instances))
	}

	inst := s.Instances[0]
	if len(inst.ReadTargets) != 2 {
		t.Fatalf("expected 2 target rows, got %d", len(inst.ReadTargets))
	}
	// Sorted for a stable dashboard that does not reshuffle between polls.
	if inst.ReadTargets[0].Target != "http://a.local" || inst.ReadTargets[1].Target != "http://b.local" {
		t.Errorf("target rows are not sorted: %+v", inst.ReadTargets)
	}
	if inst.ReadTargets[0].Failures != 2 || inst.ReadTargets[0].Successes != 0 {
		t.Errorf("row a = %+v, want 2 failures", inst.ReadTargets[0])
	}
	if inst.ReadTargets[1].Successes != 1 {
		t.Errorf("row b = %+v, want 1 success", inst.ReadTargets[1])
	}
	if inst.ReadTargets[0].LastResult != "failure" || inst.ReadTargets[1].LastResult != "success" {
		t.Errorf("latest target results = %+v, want failure/success", inst.ReadTargets)
	}
	if !reflect.DeepEqual(inst.ReadTargets[0].RecentResults, []string{"failure", "failure"}) {
		t.Errorf("row a recent results = %+v, want two failures", inst.ReadTargets[0].RecentResults)
	}
	if !reflect.DeepEqual(inst.ReadTargets[1].RecentResults, []string{"success"}) {
		t.Errorf("row b recent results = %+v, want one success", inst.ReadTargets[1].RecentResults)
	}
}

func TestReadTargetHistoryKeepsNewestSamples(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())
	for i := 0; i < 105; i++ {
		m.RecordRead("loki-ha", "http://a.local", i >= 10)
	}

	s := m.Summary()
	if len(s.Instances) != 1 || len(s.Instances[0].ReadTargets) != 1 {
		t.Fatalf("expected one read target, got %+v", s.Instances)
	}
	history := s.Instances[0].ReadTargets[0].RecentResults
	if len(history) != 100 {
		t.Fatalf("history length = %d, want 100", len(history))
	}
	for i := 0; i < 5; i++ {
		if history[i] != "failure" {
			t.Fatalf("history[%d] = %q, want failure", i, history[i])
		}
	}
	for i := 5; i < len(history); i++ {
		if history[i] != "success" {
			t.Fatalf("history[%d] = %q, want success", i, history[i])
		}
	}
	if s.Instances[0].ReadTargets[0].LastResult != "success" {
		t.Errorf("latest status = %q, want success", s.Instances[0].ReadTargets[0].LastResult)
	}
}

// Read counters are instance-labeled, so deleting an instance must drop them
// the way every other instance counter is dropped.
func TestReadCountersArePrunedWithInstances(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())
	m.RecordRead("gone", "http://a.local", true)
	m.RecordReadFailover("gone")
	m.RecordRead("kept", "http://b.local", true)

	m.RetainInstances([]string{"kept"})

	if got := m.ReadValue("gone", "http://a.local", "success"); got != 0 {
		t.Errorf("removed instance still reads %d", got)
	}
	if got := m.ReadFailoverValue("gone"); got != 0 {
		t.Errorf("removed instance failover still reads %d", got)
	}
	if got := m.ReadValue("kept", "http://b.local", "success"); got != 1 {
		t.Errorf("surviving instance lost its counter: %d", got)
	}
	s := m.Summary()
	if len(s.Instances) != 1 || len(s.Instances[0].ReadTargets) != 1 {
		t.Fatalf("expected one surviving read target, got %+v", s.Instances)
	}
	if s.Instances[0].ReadTargets[0].LastResult != "success" {
		t.Errorf("surviving latest status = %q", s.Instances[0].ReadTargets[0].LastResult)
	}
	if !reflect.DeepEqual(s.Instances[0].ReadTargets[0].RecentResults, []string{"success"}) {
		t.Errorf("surviving recent results = %+v", s.Instances[0].ReadTargets[0].RecentResults)
	}
}

// ---- config reload freshness ------------------------------------------------

// Before any reload has succeeded the gauge is absent rather than zero. A zero
// age reads as "reloaded just now", which would make a gateway that has never
// loaded its config look like the freshest one on the dashboard.
func TestConfigReloadGaugeAbsentUntilFirstSuccess(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())

	if n := testutil.CollectAndCount(m, "gateway_config_last_successful_reload_seconds"); n != 0 {
		t.Errorf("expected no age series before the first reload, got %d", n)
	}
	if _, ok := m.ConfigReloadedAt(); ok {
		t.Error("ConfigReloadedAt reported a time before any reload had succeeded")
	}
	// The failure counter, in contrast, is exported from the start so an alert
	// on its rate has a series to attach to.
	if n := testutil.CollectAndCount(m, "gateway_config_reload_failures_total"); n != 1 {
		t.Errorf("expected the failure counter to be exported from process start, got %d series", n)
	}
}

func TestConfigReloadGaugeTracksLastSuccess(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())

	first := time.Unix(1_700_000_000, 0)
	m.RecordConfigReload(first)
	if got, ok := m.ConfigReloadedAt(); !ok || !got.Equal(first) {
		t.Fatalf("ConfigReloadedAt = %v (ok=%v), want %v", got, ok, first)
	}

	second := first.Add(30 * time.Second)
	m.RecordConfigReload(second)
	if got, _ := m.ConfigReloadedAt(); !got.Equal(second) {
		t.Errorf("ConfigReloadedAt = %v, want %v", got, second)
	}
}

// The whole point of the gauge: a failing reload must leave the last-success
// instant alone, so the age keeps climbing while the database is unreachable.
func TestConfigReloadFailureDoesNotAdvanceTimestamp(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())

	good := time.Unix(1_700_000_000, 0)
	m.RecordConfigReload(good)
	for i := 0; i < 3; i++ {
		m.RecordConfigReloadFailure()
	}

	if got, _ := m.ConfigReloadedAt(); !got.Equal(good) {
		t.Errorf("a failed reload moved the timestamp to %v, want it left at %v", got, good)
	}
	if n := m.ConfigReloadFailures(); n != 3 {
		t.Errorf("failure count = %d, want 3", n)
	}

	expected := `
# HELP gateway_config_reload_failures_total Config reloads that failed, from any trigger: the ticker, SIGHUP, the admin API, or a mutation applying itself. The live config is retained on failure.
# TYPE gateway_config_reload_failures_total counter
gateway_config_reload_failures_total 3
`
	if err := testutil.CollectAndCompare(m, strings.NewReader(expected),
		"gateway_config_reload_failures_total"); err != nil {
		t.Fatal(err)
	}
}

// The exported gauge is an age resolved at scrape time, not the stored instant,
// so it climbs on its own between scrapes while reloads keep failing.
func TestConfigReloadAgeIsMeasuredAtScrapeTime(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())
	m.RecordConfigReload(time.Now().Add(-2 * time.Minute))

	age := gaugeValue(t, m, "gateway_config_last_successful_reload_seconds")
	if age < 119 || age > 125 {
		t.Fatalf("age = %v, want about 120", age)
	}

	// A failed reload must not reset it.
	m.RecordConfigReloadFailure()
	if after := gaugeValue(t, m, "gateway_config_last_successful_reload_seconds"); after < age {
		t.Errorf("a failed reload pulled the age back from %v to %v", age, after)
	}

	// A successful one must.
	m.RecordConfigReload(time.Now())
	if after := gaugeValue(t, m, "gateway_config_last_successful_reload_seconds"); after > 2 {
		t.Errorf("age after a successful reload = %v, want about 0", after)
	}
}

// gaugeValue scrapes one unlabelled gauge out of the collector.
func gaugeValue(t *testing.T, c prometheus.Collector, name string) float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		if len(mf.GetMetric()) != 1 {
			t.Fatalf("%s: got %d series, want 1", name, len(mf.GetMetric()))
		}
		return mf.GetMetric()[0].GetGauge().GetValue()
	}
	t.Fatalf("%s not exported", name)
	return 0
}

// The reload counters carry no instance label and must survive the pruning that
// drops series for deleted instances.
func TestConfigReloadMetricsSurviveRetainInstances(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())
	m.RecordConfigReload(time.Unix(1_700_000_000, 0))
	m.RecordConfigReloadFailure()

	m.RetainInstances(nil)

	if _, ok := m.ConfigReloadedAt(); !ok {
		t.Error("RetainInstances dropped the reload timestamp")
	}
	if n := m.ConfigReloadFailures(); n != 1 {
		t.Errorf("RetainInstances dropped the failure count: got %d, want 1", n)
	}
}

// The Summary the admin UI reads carries the same figures, with the age
// resolved against the gateway's clock rather than the browser's.
func TestSummaryReportsConfigAge(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())

	if s := m.Summary(); s.ConfigReloadedAt != nil || s.ConfigAgeSeconds != nil {
		t.Error("summary reported a config age before any reload had succeeded")
	}

	at := time.Now().Add(-90 * time.Second)
	m.RecordConfigReload(at)
	m.RecordConfigReloadFailure()

	s := m.Summary()
	if s.ConfigReloadedAt == nil || *s.ConfigReloadedAt != at.Unix() {
		t.Fatalf("ConfigReloadedAt = %v, want %d", s.ConfigReloadedAt, at.Unix())
	}
	if s.ConfigAgeSeconds == nil || *s.ConfigAgeSeconds < 89 || *s.ConfigAgeSeconds > 95 {
		t.Errorf("ConfigAgeSeconds = %v, want about 90", s.ConfigAgeSeconds)
	}
	if s.ConfigReloadFailures != 1 {
		t.Errorf("ConfigReloadFailures = %d, want 1", s.ConfigReloadFailures)
	}
}
