package metrics_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

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
	if m1.FanoutRequests == nil || m2.FanoutRequests == nil {
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
