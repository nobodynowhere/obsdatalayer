package metrics_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"obsdatalayer/internal/metrics"
)

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
	if got := testutil.CollectAndCount(m1); got != 0 {
		t.Fatalf("expected a fresh collector to export no series, got %d", got)
	}
	m1.RecordPartialFailure("inst")
	if got := testutil.CollectAndCount(m1); got != 1 {
		t.Fatalf("expected 1 series after recording, got %d", got)
	}
	if got := testutil.CollectAndCount(m2); got != 0 {
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
`
	if err := testutil.CollectAndCompare(m, strings.NewReader(expected)); err != nil {
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
	if got := testutil.CollectAndCount(m); got != 10 {
		t.Fatalf("expected 10 series before pruning, got %d", got)
	}

	m.RetainInstances([]string{"inst-a"})

	if got := testutil.CollectAndCount(m); got != 5 {
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
	m.RetainInstances(nil)
	if got := testutil.CollectAndCount(m); got != 0 {
		t.Fatalf("expected all series dropped, got %d", got)
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
