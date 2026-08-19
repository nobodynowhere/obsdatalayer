// Package metrics owns the gateway's Prometheus counters.
//
// The counters are backed by atomic integers this package owns and are exposed
// through a custom prometheus.Collector rather than a CounterVec. Two properties
// of this gateway motivate that:
//
// Series lifecycle. Instances are created and deleted at runtime through the
// admin API, and every counter here is labeled by instance. A CounterVec never
// evicts a series, so a long-lived gateway would keep exporting counters for
// instances that no longer exist. Owning the map lets RetainInstances drop them
// when a reload observes that an instance is gone.
//
// Reload safety. The config is rebuilt and atomically swapped on every reload,
// so counters must not live on config structs: they would reset to zero on each
// reload and Prometheus would read every reload as a counter reset. Keeping them
// here, keyed by label tuple, makes them independent of the config lifecycle.
package metrics

import (
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

// Label tuples. Each is a comparable struct of strings so it can key a map
// directly. That also makes it impossible for Collect to emit the same series
// twice, which would fail the whole scrape.
type fanoutKey struct{ instance, target, status string }
type suppressedKey struct{ instance, target, pattern string }
type partialKey struct{ instance string }
type writeItemsKey struct{ backend, instance, kind, result string }
type rewriteLabelsKey struct{ backend, instance, operation string }

// labelKey is the contract every key type satisfies so counterSet can stay
// generic: values() returns the label values in the order its Desc declares
// them, and instanceName() is what RetainInstances prunes on.
type labelKey interface {
	comparable
	values() []string
	instanceName() string
}

func (k fanoutKey) values() []string     { return []string{k.instance, k.target, k.status} }
func (k fanoutKey) instanceName() string { return k.instance }

func (k suppressedKey) values() []string     { return []string{k.instance, k.target, k.pattern} }
func (k suppressedKey) instanceName() string { return k.instance }

func (k partialKey) values() []string     { return []string{k.instance} }
func (k partialKey) instanceName() string { return k.instance }

func (k writeItemsKey) values() []string {
	return []string{k.backend, k.instance, k.kind, k.result}
}
func (k writeItemsKey) instanceName() string { return k.instance }

func (k rewriteLabelsKey) values() []string {
	return []string{k.backend, k.instance, k.operation}
}
func (k rewriteLabelsKey) instanceName() string { return k.instance }

// counterSet is a set of monotonic counters keyed by label tuple. The map is
// guarded by a mutex; the values are atomics, so the common case (a series that
// already exists) takes only a read lock plus an atomic add.
type counterSet[K labelKey] struct {
	mu   sync.RWMutex
	vals map[K]*atomic.Uint64
}

func newCounterSet[K labelKey]() *counterSet[K] {
	return &counterSet[K]{vals: make(map[K]*atomic.Uint64)}
}

func (c *counterSet[K]) add(key K, n uint64) {
	c.mu.RLock()
	v, ok := c.vals[key]
	c.mu.RUnlock()
	if ok {
		v.Add(n)
		return
	}

	c.mu.Lock()
	if v, ok = c.vals[key]; !ok {
		v = new(atomic.Uint64)
		c.vals[key] = v
	}
	c.mu.Unlock()
	v.Add(n)
}

func (c *counterSet[K]) value(key K) uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if v, ok := c.vals[key]; ok {
		return v.Load()
	}
	return 0
}

// snapshot copies the set under a read lock. Callers emit from the copy rather
// than holding the lock across channel sends, so a slow scrape cannot stall
// recorders on the request path.
func (c *counterSet[K]) snapshot() map[K]uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[K]uint64, len(c.vals))
	for k, v := range c.vals {
		out[k] = v.Load()
	}
	return out
}

// retain drops every series whose instance is absent from live, returning how
// many were dropped. Dropping a series is safe: the metric simply stops being
// exported and Prometheus marks it stale. Resetting one in place would not be,
// which is why nothing here ever writes a smaller value.
func (c *counterSet[K]) retain(live map[string]struct{}) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	dropped := 0
	for k := range c.vals {
		if _, ok := live[k.instanceName()]; !ok {
			delete(c.vals, k)
			dropped++
		}
	}
	return dropped
}

func (c *counterSet[K]) collect(ch chan<- prometheus.Metric, desc *prometheus.Desc) {
	for k, v := range c.snapshot() {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, float64(v), k.values()...)
	}
}

// Metrics holds all gateway Prometheus metrics and is itself the Collector that
// exports them.
type Metrics struct {
	fanoutDesc        *prometheus.Desc
	suppressedDesc    *prometheus.Desc
	partialDesc       *prometheus.Desc
	writeItemsDesc    *prometheus.Desc
	rewriteLabelsDesc *prometheus.Desc

	fanout        *counterSet[fanoutKey]
	suppressed    *counterSet[suppressedKey]
	partial       *counterSet[partialKey]
	writeItems    *counterSet[writeItemsKey]
	rewriteLabels *counterSet[rewriteLabelsKey]
}

var _ prometheus.Collector = (*Metrics)(nil)

// New creates and registers all gateway metrics with reg.
// Pass prometheus.DefaultRegisterer in production; prometheus.NewRegistry() in tests.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		fanoutDesc: prometheus.NewDesc(
			"gateway_fanout_requests_total",
			"Fan-out push attempts, labeled by instance, target, status.",
			[]string{"instance", "target", "status"}, nil,
		),
		suppressedDesc: prometheus.NewDesc(
			"gateway_suppressed_errors_total",
			"Mimir 400 responses suppressed in any mode, labeled by instance, target, pattern.",
			[]string{"instance", "target", "pattern"}, nil,
		),
		partialDesc: prometheus.NewDesc(
			"gateway_partial_failures_total",
			"Pushes that returned success with X-Gateway-Partial-Failure header, labeled by instance.",
			[]string{"instance"}, nil,
		),
		writeItemsDesc: prometheus.NewDesc(
			"gateway_write_items_total",
			"Write payload items observed by the gateway, labeled by backend, instance, item kind, and result.",
			[]string{"backend", "instance", "kind", "result"}, nil,
		),
		rewriteLabelsDesc: prometheus.NewDesc(
			"gateway_rewrite_labels_total",
			"Labels changed by gateway write-payload rewriting, labeled by backend, instance, and operation.",
			[]string{"backend", "instance", "operation"}, nil,
		),

		fanout:        newCounterSet[fanoutKey](),
		suppressed:    newCounterSet[suppressedKey](),
		partial:       newCounterSet[partialKey](),
		writeItems:    newCounterSet[writeItemsKey](),
		rewriteLabels: newCounterSet[rewriteLabelsKey](),
	}
	reg.MustRegister(m)
	return m
}

// Describe sends every Desc, so the registry can still detect a duplicate
// registration rather than silently accepting two collectors for one metric.
func (m *Metrics) Describe(ch chan<- *prometheus.Desc) {
	ch <- m.fanoutDesc
	ch <- m.suppressedDesc
	ch <- m.partialDesc
	ch <- m.writeItemsDesc
	ch <- m.rewriteLabelsDesc
}

func (m *Metrics) Collect(ch chan<- prometheus.Metric) {
	m.fanout.collect(ch, m.fanoutDesc)
	m.suppressed.collect(ch, m.suppressedDesc)
	m.partial.collect(ch, m.partialDesc)
	m.writeItems.collect(ch, m.writeItemsDesc)
	m.rewriteLabels.collect(ch, m.rewriteLabelsDesc)
}

// ---- recording --------------------------------------------------------------

func (m *Metrics) RecordFanout(instance, target string, status int) {
	m.fanout.add(fanoutKey{instance, target, strconv.Itoa(status)}, 1)
}

func (m *Metrics) RecordSuppressed(instance, target, pattern string) {
	m.suppressed.add(suppressedKey{instance, target, pattern}, 1)
}

func (m *Metrics) RecordPartialFailure(instance string) {
	m.partial.add(partialKey{instance}, 1)
}

func (m *Metrics) RecordWriteItems(backend, instance, kind, result string, count int) {
	if count <= 0 {
		return
	}
	m.writeItems.add(writeItemsKey{backend, instance, kind, result}, uint64(count))
}

func (m *Metrics) RecordRewriteLabels(backend, instance, operation string, count int) {
	if count <= 0 {
		return
	}
	m.rewriteLabels.add(rewriteLabelsKey{backend, instance, operation}, uint64(count))
}

// ---- reading ----------------------------------------------------------------

// The Value accessors mirror the Record methods and read a single series. They
// exist so tests and the admin API can read a counter without going through the
// Prometheus exposition format.

func (m *Metrics) FanoutValue(instance, target string, status int) uint64 {
	return m.fanout.value(fanoutKey{instance, target, strconv.Itoa(status)})
}

func (m *Metrics) SuppressedValue(instance, target, pattern string) uint64 {
	return m.suppressed.value(suppressedKey{instance, target, pattern})
}

func (m *Metrics) PartialFailureValue(instance string) uint64 {
	return m.partial.value(partialKey{instance})
}

func (m *Metrics) WriteItemsValue(backend, instance, kind, result string) uint64 {
	return m.writeItems.value(writeItemsKey{backend, instance, kind, result})
}

func (m *Metrics) RewriteLabelsValue(backend, instance, operation string) uint64 {
	return m.rewriteLabels.value(rewriteLabelsKey{backend, instance, operation})
}

// ---- lifecycle --------------------------------------------------------------

// RetainInstances drops the counters of every instance not named in live. The
// reload path calls it with the instance names it just published, so removing an
// instance through the admin API also stops its series being exported.
//
// It takes the surviving set rather than a single removed name on purpose: the
// reload path always knows what is live, and a diff-based API would silently
// leak whenever an instance disappeared without the caller noticing.
func (m *Metrics) RetainInstances(names []string) {
	live := make(map[string]struct{}, len(names))
	for _, n := range names {
		live[n] = struct{}{}
	}
	dropped := m.fanout.retain(live) +
		m.suppressed.retain(live) +
		m.partial.retain(live) +
		m.writeItems.retain(live) +
		m.rewriteLabels.retain(live)
	if dropped > 0 {
		slog.Debug("dropped metric series for removed instances",
			"series", dropped, "live_instances", len(live))
	}
}

// ---- summary ----------------------------------------------------------------

// Summary is an aggregated view of the counters for the admin UI, which wants
// headline totals rather than the full label-tuple cross product.
type Summary struct {
	FanoutRequests   uint64            `json:"fanout_requests"`
	FanoutFailures   uint64            `json:"fanout_failures"`
	SuppressedErrors uint64            `json:"suppressed_errors"`
	PartialFailures  uint64            `json:"partial_failures"`
	ItemsForwarded   uint64            `json:"items_forwarded"`
	LabelsRewritten  uint64            `json:"labels_rewritten"`
	Instances        []InstanceSummary `json:"instances"`
}

// InstanceSummary is the same figures for one instance.
type InstanceSummary struct {
	Instance         string `json:"instance"`
	FanoutRequests   uint64 `json:"fanout_requests"`
	FanoutFailures   uint64 `json:"fanout_failures"`
	SuppressedErrors uint64 `json:"suppressed_errors"`
	PartialFailures  uint64 `json:"partial_failures"`
	ItemsForwarded   uint64 `json:"items_forwarded"`
	LabelsRewritten  uint64 `json:"labels_rewritten"`
}

// Summary aggregates the current counters. Each set is snapshotted separately,
// so totals can be a scrape apart under load; they are counters for a dashboard,
// not a transactional read.
func (m *Metrics) Summary() Summary {
	byInstance := map[string]*InstanceSummary{}
	at := func(name string) *InstanceSummary {
		s, ok := byInstance[name]
		if !ok {
			s = &InstanceSummary{Instance: name}
			byInstance[name] = s
		}
		return s
	}

	var out Summary
	for k, v := range m.fanout.snapshot() {
		s := at(k.instance)
		s.FanoutRequests += v
		out.FanoutRequests += v
		if isFailureStatus(k.status) {
			s.FanoutFailures += v
			out.FanoutFailures += v
		}
	}
	for k, v := range m.suppressed.snapshot() {
		at(k.instance).SuppressedErrors += v
		out.SuppressedErrors += v
	}
	for k, v := range m.partial.snapshot() {
		at(k.instance).PartialFailures += v
		out.PartialFailures += v
	}
	for k, v := range m.writeItems.snapshot() {
		if k.result != "forwarded" {
			continue
		}
		at(k.instance).ItemsForwarded += v
		out.ItemsForwarded += v
	}
	for k, v := range m.rewriteLabels.snapshot() {
		at(k.instance).LabelsRewritten += v
		out.LabelsRewritten += v
	}

	out.Instances = make([]InstanceSummary, 0, len(byInstance))
	for _, s := range byInstance {
		out.Instances = append(out.Instances, *s)
	}
	sort.Slice(out.Instances, func(i, j int) bool {
		return out.Instances[i].Instance < out.Instances[j].Instance
	})
	return out
}

// isFailureStatus reports whether a recorded fan-out status counts as a failure.
// RecordFanout stores 0 when the request never produced a response at all.
func isFailureStatus(status string) bool {
	code, err := strconv.Atoi(status)
	if err != nil {
		return false
	}
	return code == 0 || code >= 400
}
