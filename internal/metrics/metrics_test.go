package metrics_test

import (
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
	if m1.FanoutRequests == nil || m2.FanoutRequests == nil || m1.WriteItems == nil || m2.RewriteLabels == nil {
		t.Fatal("expected counters to be initialised")
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

	count := testutil.ToFloat64(m.WriteItems.WithLabelValues("mimir", "inst", "series", "forwarded"))
	if count != 3 {
		t.Fatalf("expected forwarded item count 3, got %v", count)
	}
}

func TestRecordRewriteLabels(t *testing.T) {
	m := metrics.New(prometheus.NewRegistry())
	m.RecordRewriteLabels("mimir", "inst", "dropped", 2)
	m.RecordRewriteLabels("mimir", "inst", "dropped", -1)

	count := testutil.ToFloat64(m.RewriteLabels.WithLabelValues("mimir", "inst", "dropped"))
	if count != 2 {
		t.Fatalf("expected dropped label count 2, got %v", count)
	}
}
