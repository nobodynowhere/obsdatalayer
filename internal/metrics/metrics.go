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
	"time"

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

// readKey labels a proxied read. target is the upstream actually asked, so a
// fan-out instance shows which replica served and which failed; result is
// "success" or "failure".
type readKey struct{ instance, target, result string }
type readTargetKey struct{ instance, target string }

// operationalKey labels one request to an upstream operational endpoint --
// /ready, /config, /metrics and the rest. endpoint is the gateway's short alias
// rather than the upstream path, because the same question has a different path
// on each backend and an operator comparing instances wants one series.
//
// result has three values rather than the reads' two. "unreachable" and "error"
// are the distinction the whole feature exists to make: a target that refuses
// the connection and a target that answers "not ready" look identical in a
// success/failure counter, and they call for different action.
type operationalKey struct{ instance, target, endpoint, result string }

const readHistoryLimit = 100

// truncatedKey labels a read whose response body was cut short on its way to
// the client. target is the upstream that was serving it. Shared by the
// truncation and client-disconnect counters, which carry the same labels and
// differ only in which side of the gateway gave up.
type truncatedKey struct{ instance, target string }

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

func (k readKey) values() []string     { return []string{k.instance, k.target, k.result} }
func (k readKey) instanceName() string { return k.instance }

func (k operationalKey) values() []string {
	return []string{k.instance, k.target, k.endpoint, k.result}
}
func (k operationalKey) instanceName() string { return k.instance }

func (k truncatedKey) values() []string     { return []string{k.instance, k.target} }
func (k truncatedKey) instanceName() string { return k.instance }

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
	readDesc          *prometheus.Desc
	operationalDesc   *prometheus.Desc
	readFailoverDesc  *prometheus.Desc
	readTruncatedDesc *prometheus.Desc
	readClientGone    *prometheus.Desc
	authRejectedDesc  *prometheus.Desc
	authFailuresDesc  *prometheus.Desc

	configReloadAgeDesc  *prometheus.Desc
	configReloadFailDesc *prometheus.Desc

	fanout        *counterSet[fanoutKey]
	suppressed    *counterSet[suppressedKey]
	partial       *counterSet[partialKey]
	writeItems    *counterSet[writeItemsKey]
	rewriteLabels *counterSet[rewriteLabelsKey]
	reads         *counterSet[readKey]
	operational   *counterSet[operationalKey]
	readFailovers *counterSet[partialKey]
	readTruncated *counterSet[truncatedKey]

	readHistoryMu sync.RWMutex
	readHistory   map[readTargetKey][]string

	// readDisconnects is deliberately not folded into readTruncated. A caller
	// hanging up mid-body is routine -- a dashboard panel closed, a query
	// cancelled -- while a truncation means the gateway lost data it had
	// promised. One series that mixed them would make the second unalertable.
	readDisconnects *counterSet[truncatedKey]

	// Authentication counters are plain atomics rather than a counterSet.
	// They carry no instance label, so they must not be reachable by
	// RetainInstances, which prunes every series whose instance is gone and
	// would otherwise delete these on the first reload. The label space is a
	// fixed, tiny set, so a map buys nothing here anyway.
	authThrottled atomic.Uint64
	authSaturated atomic.Uint64
	authFailures  atomic.Uint64

	// Config reload freshness. Plain atomics for the same reason as the auth
	// counters above: they carry no instance label and must survive
	// RetainInstances.
	//
	// configReloadedAt is Unix seconds of the last *successful* reload. It is
	// stored as an instant and exported as an age, so the age is always
	// current at scrape time rather than at reload time. A failed reload
	// deliberately does not touch it -- the gateway is still serving whatever
	// it loaded then, so the age keeps climbing for as long as the database is
	// unreachable, which is exactly what an alert needs to see.
	configReloadedAt   atomic.Int64
	configReloadFailed atomic.Uint64
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

		readDesc: prometheus.NewDesc(
			"gateway_read_requests_total",
			"Proxied read attempts, labeled by instance, the upstream target asked, and result (success or failure).",
			[]string{"instance", "target", "result"}, nil,
		),
		operationalDesc: prometheus.NewDesc(
			"gateway_operational_requests_total",
			"Requests to an upstream operational endpoint, labeled by instance, the target asked, the endpoint alias, and result (success, error or unreachable).",
			[]string{"instance", "target", "endpoint", "result"}, nil,
		),
		readFailoverDesc: prometheus.NewDesc(
			"gateway_read_failovers_total",
			"Reads that moved on to another target after one failed, labeled by instance.",
			[]string{"instance"}, nil,
		),

		readTruncatedDesc: prometheus.NewDesc(
			"gateway_read_truncated_total",
			"Reads whose response body failed part-way through being copied to the client, "+
				"labeled by instance and the upstream target that was serving it. The status "+
				"line was already sent, so the client received an incomplete body under a "+
				"success status; any non-zero rate here means clients are silently seeing "+
				"partial results.",
			[]string{"instance", "target"}, nil,
		),

		readClientGone: prometheus.NewDesc(
			"gateway_read_client_disconnects_total",
			"Reads whose body copy stopped because the client went away, labeled by "+
				"instance and the upstream target that was serving it. Routine: it means "+
				"the caller cancelled or closed the connection before the answer was "+
				"fully delivered, and neither the gateway nor the upstream lost data.",
			[]string{"instance", "target"}, nil,
		),

		authRejectedDesc: prometheus.NewDesc(
			"gateway_auth_rejected_total",
			"Requests rejected before their credentials were checked, labeled by reason: "+
				"throttled (the source had failed too often) or saturated (no password-hashing slot was free).",
			[]string{"reason"}, nil,
		),
		authFailuresDesc: prometheus.NewDesc(
			"gateway_auth_failures_total",
			"Credential checks that ran and were rejected.",
			nil, nil,
		),

		configReloadAgeDesc: prometheus.NewDesc(
			"gateway_config_last_successful_reload_seconds",
			"Seconds since the last successful config reload, i.e. the age of the "+
				"configuration the gateway is actually serving. A failed reload does not "+
				"reset it, so it climbs for as long as the database is unreachable. Alert "+
				"on it directly (> 300, say): a gateway that cannot reach its database "+
				"keeps serving its last good config indefinitely and is otherwise "+
				"indistinguishable from a healthy one.",
			nil, nil,
		),
		configReloadFailDesc: prometheus.NewDesc(
			"gateway_config_reload_failures_total",
			"Config reloads that failed, from any trigger: the ticker, SIGHUP, the admin "+
				"API, or a mutation applying itself. The live config is retained on failure.",
			nil, nil,
		),

		fanout:          newCounterSet[fanoutKey](),
		suppressed:      newCounterSet[suppressedKey](),
		partial:         newCounterSet[partialKey](),
		writeItems:      newCounterSet[writeItemsKey](),
		rewriteLabels:   newCounterSet[rewriteLabelsKey](),
		reads:           newCounterSet[readKey](),
		operational:     newCounterSet[operationalKey](),
		readFailovers:   newCounterSet[partialKey](),
		readTruncated:   newCounterSet[truncatedKey](),
		readHistory:     make(map[readTargetKey][]string),
		readDisconnects: newCounterSet[truncatedKey](),
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
	ch <- m.readDesc
	ch <- m.operationalDesc
	ch <- m.readFailoverDesc
	ch <- m.readTruncatedDesc
	ch <- m.readClientGone
	ch <- m.authRejectedDesc
	ch <- m.authFailuresDesc
	ch <- m.configReloadAgeDesc
	ch <- m.configReloadFailDesc
}

func (m *Metrics) Collect(ch chan<- prometheus.Metric) {
	m.fanout.collect(ch, m.fanoutDesc)
	m.suppressed.collect(ch, m.suppressedDesc)
	m.partial.collect(ch, m.partialDesc)
	m.writeItems.collect(ch, m.writeItemsDesc)
	m.rewriteLabels.collect(ch, m.rewriteLabelsDesc)
	m.reads.collect(ch, m.readDesc)
	m.operational.collect(ch, m.operationalDesc)
	m.readFailovers.collect(ch, m.readFailoverDesc)
	m.readTruncated.collect(ch, m.readTruncatedDesc)
	m.readDisconnects.collect(ch, m.readClientGone)

	// Emitted unconditionally, zeros included, so an alert on the rate of these
	// has a series to attach to from process start rather than only once the
	// gateway has first been attacked.
	ch <- prometheus.MustNewConstMetric(m.authRejectedDesc, prometheus.CounterValue,
		float64(m.authThrottled.Load()), "throttled")
	ch <- prometheus.MustNewConstMetric(m.authRejectedDesc, prometheus.CounterValue,
		float64(m.authSaturated.Load()), "saturated")
	ch <- prometheus.MustNewConstMetric(m.authFailuresDesc, prometheus.CounterValue,
		float64(m.authFailures.Load()))

	ch <- prometheus.MustNewConstMetric(m.configReloadFailDesc, prometheus.CounterValue,
		float64(m.configReloadFailed.Load()))
	// Resolved at scrape time, and against the gateway's own clock: an age is
	// alertable on its own (> 300) without needing time(), and it does not go
	// wrong when the gateway and Prometheus disagree about what time it is.
	//
	// Only emitted once a reload has actually succeeded. A zero here would read
	// as "reloaded just now" and would mean a gateway that has never managed to
	// load its config looks the healthiest of all.
	if at, ok := m.ConfigReloadedAt(); ok {
		ch <- prometheus.MustNewConstMetric(m.configReloadAgeDesc, prometheus.GaugeValue,
			time.Since(at).Seconds())
	}
}

// ---- recording --------------------------------------------------------------

// RecordRead counts one proxied read attempt against one upstream target.
// A read that fails over records a failure for the target that failed and a
// success for the one that answered, so the counters show both that the read
// was served and that a replica is unwell.
func (m *Metrics) RecordRead(instance, target string, ok bool) {
	result := "failure"
	if ok {
		result = "success"
	}
	m.reads.add(readKey{instance, target, result}, 1)
	m.recordReadHistory(readTargetKey{instance, target}, result)
}

// Operational request outcomes.
const (
	// OperationalSuccess: the target answered 2xx.
	OperationalSuccess = "success"
	// OperationalError: the target answered, but not 2xx. It is up and it said
	// no -- an ingester still replaying a WAL answers /ready this way.
	OperationalError = "error"
	// OperationalUnreachable: the target did not answer at all.
	OperationalUnreachable = "unreachable"
)

// RecordOperational counts one request to one target's operational endpoint.
//
// Deliberately a counter of its own rather than a read: these requests are not
// failed over, do not feed the read cool-off, and asking target 2 whether it is
// ready must never show up as target 1 failing a read. Keeping them apart is
// what lets an alert say "this replica has been unreachable for five minutes"
// without competing with query traffic for the same series.
func (m *Metrics) RecordOperational(instance, target, endpoint, result string) {
	m.operational.add(operationalKey{instance, target, endpoint, result}, 1)
}

// OperationalValue returns how many requests for an instance, target, endpoint
// and result have been counted.
func (m *Metrics) OperationalValue(instance, target, endpoint, result string) uint64 {
	return m.operational.value(operationalKey{instance, target, endpoint, result})
}

// RecordReadFailover counts a read that had to try more than one target.
func (m *Metrics) RecordReadFailover(instance string) {
	m.readFailovers.add(partialKey{instance}, 1)
}

// RecordReadTruncated counts a read whose body was cut short mid-copy. It is
// separate from RecordRead because the read was already counted a success: the
// upstream answered, the status went out, and only then did the body fail. A
// failure counted against the target would misreport that target as unhealthy.
func (m *Metrics) RecordReadTruncated(instance, target string) {
	m.readTruncated.add(truncatedKey{instance, target}, 1)
}

// ReadTruncatedValue returns how many reads for an instance and target were cut
// short mid-body.
func (m *Metrics) ReadTruncatedValue(instance, target string) uint64 {
	return m.readTruncated.value(truncatedKey{instance, target})
}

// RecordReadClientDisconnect counts a read whose body copy stopped because the
// caller went away. Like RecordReadTruncated it does not touch the read result:
// the upstream answered, and the client leaving says nothing about its health.
func (m *Metrics) RecordReadClientDisconnect(instance, target string) {
	m.readDisconnects.add(truncatedKey{instance, target}, 1)
}

// ReadClientDisconnectValue returns how many reads for an instance and target
// ended with the client gone.
func (m *Metrics) ReadClientDisconnectValue(instance, target string) uint64 {
	return m.readDisconnects.value(truncatedKey{instance, target})
}

// ReadValue returns the count for one instance, target and result.
func (m *Metrics) ReadValue(instance, target, result string) uint64 {
	return m.reads.value(readKey{instance, target, result})
}

// ReadFailoverValue returns how many reads for an instance failed over.
func (m *Metrics) ReadFailoverValue(instance string) uint64 {
	return m.readFailovers.value(partialKey{instance})
}

// RecordConfigReload marks a successful config reload as having happened at t.
// Called only on the path that publishes a new config, so the timestamp always
// describes configuration the gateway is genuinely serving.
func (m *Metrics) RecordConfigReload(t time.Time) {
	m.configReloadedAt.Store(t.Unix())
}

// RecordConfigReloadFailure counts a reload that did not publish.
func (m *Metrics) RecordConfigReloadFailure() {
	m.configReloadFailed.Add(1)
}

// ConfigReloadedAt returns the last successful reload time, and false if no
// reload has succeeded yet.
func (m *Metrics) ConfigReloadedAt() (time.Time, bool) {
	at := m.configReloadedAt.Load()
	if at == 0 {
		return time.Time{}, false
	}
	return time.Unix(at, 0), true
}

// ConfigReloadFailures returns how many reloads have failed.
func (m *Metrics) ConfigReloadFailures() uint64 {
	return m.configReloadFailed.Load()
}

// RecordAuthRejected counts a request refused before any credential check.
func (m *Metrics) RecordAuthRejected(reason string) {
	switch reason {
	case "throttled":
		m.authThrottled.Add(1)
	case "saturated":
		m.authSaturated.Add(1)
	default:
		slog.Warn("unknown auth rejection reason", "reason", reason)
	}
}

// RecordAuthFailure counts a credential check that ran and failed.
func (m *Metrics) RecordAuthFailure() { m.authFailures.Add(1) }

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

// AuthFailureValue returns credential checks that ran and were rejected.
func (m *Metrics) AuthFailureValue() uint64 { return m.authFailures.Load() }

// AuthRejectedValue returns requests refused before any credential check, for
// reason "throttled" or "saturated".
func (m *Metrics) AuthRejectedValue(reason string) uint64 {
	switch reason {
	case "throttled":
		return m.authThrottled.Load()
	case "saturated":
		return m.authSaturated.Load()
	}
	return 0
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
		m.rewriteLabels.retain(live) +
		m.reads.retain(live) +
		m.operational.retain(live) +
		m.readFailovers.retain(live) +
		m.readTruncated.retain(live) +
		m.readDisconnects.retain(live)
	m.readHistoryMu.Lock()
	for k := range m.readHistory {
		if _, ok := live[k.instance]; !ok {
			delete(m.readHistory, k)
			dropped++
		}
	}
	m.readHistoryMu.Unlock()
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
	ReadSuccesses    uint64            `json:"read_successes"`
	ReadFailures     uint64            `json:"read_failures"`
	ReadFailovers    uint64            `json:"read_failovers"`
	ReadTruncated    uint64            `json:"read_truncated"`
	ReadDisconnects  uint64            `json:"read_client_disconnects"`
	Instances        []InstanceSummary `json:"instances"`

	// Config freshness, for the Overview page. ConfigAgeSeconds is resolved
	// here rather than in the browser so the age is measured against the
	// gateway's clock, not the operator's laptop's.
	ConfigReloadedAt     *int64 `json:"config_reloaded_at,omitempty"`
	ConfigAgeSeconds     *int64 `json:"config_age_seconds,omitempty"`
	ConfigReloadFailures uint64 `json:"config_reload_failures"`
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
	ReadSuccesses    uint64 `json:"read_successes"`
	ReadFailures     uint64 `json:"read_failures"`
	ReadFailovers    uint64 `json:"read_failovers"`
	ReadTruncated    uint64 `json:"read_truncated"`
	ReadDisconnects  uint64 `json:"read_client_disconnects"`
	// ReadTargets breaks the read counts down by upstream, so a dashboard can
	// show which replica is failing rather than only that something is.
	ReadTargets []ReadTargetSummary `json:"read_targets,omitempty"`
}

// ReadTargetSummary is the read outcome for one upstream target.
type ReadTargetSummary struct {
	Target        string   `json:"target"`
	Successes     uint64   `json:"successes"`
	Failures      uint64   `json:"failures"`
	LastResult    string   `json:"last_result,omitempty"`
	RecentResults []string `json:"recent_results,omitempty"`
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
	out.ConfigReloadFailures = m.configReloadFailed.Load()
	if at, ok := m.ConfigReloadedAt(); ok {
		unix := at.Unix()
		age := int64(time.Since(at).Seconds())
		if age < 0 {
			age = 0
		}
		out.ConfigReloadedAt = &unix
		out.ConfigAgeSeconds = &age
	}

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
	// Read outcomes, aggregated per instance and broken down per target.
	readTargets := map[string]map[string]*ReadTargetSummary{}
	readHistory := m.readHistorySnapshot()
	for k, v := range m.reads.snapshot() {
		s := at(k.instance)
		byTarget, ok := readTargets[k.instance]
		if !ok {
			byTarget = map[string]*ReadTargetSummary{}
			readTargets[k.instance] = byTarget
		}
		t, ok := byTarget[k.target]
		if !ok {
			history := readHistory[readTargetKey{k.instance, k.target}]
			t = &ReadTargetSummary{
				Target:        k.target,
				LastResult:    lastReadResult(history),
				RecentResults: history,
			}
			byTarget[k.target] = t
		}
		if k.result == "success" {
			s.ReadSuccesses += v
			out.ReadSuccesses += v
			t.Successes += v
			continue
		}
		s.ReadFailures += v
		out.ReadFailures += v
		t.Failures += v
	}
	for k, v := range m.readFailovers.snapshot() {
		at(k.instance).ReadFailovers += v
		out.ReadFailovers += v
	}
	for k, v := range m.readTruncated.snapshot() {
		at(k.instance).ReadTruncated += v
		out.ReadTruncated += v
	}
	for k, v := range m.readDisconnects.snapshot() {
		at(k.instance).ReadDisconnects += v
		out.ReadDisconnects += v
	}
	for instance, byTarget := range readTargets {
		s := at(instance)
		for _, t := range byTarget {
			s.ReadTargets = append(s.ReadTargets, *t)
		}
		// Stable output so the dashboard does not reshuffle between polls.
		sort.Slice(s.ReadTargets, func(i, j int) bool {
			return s.ReadTargets[i].Target < s.ReadTargets[j].Target
		})
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

func (m *Metrics) recordReadHistory(k readTargetKey, result string) {
	m.readHistoryMu.Lock()
	defer m.readHistoryMu.Unlock()
	history := append(m.readHistory[k], result)
	if len(history) > readHistoryLimit {
		history = append([]string(nil), history[len(history)-readHistoryLimit:]...)
	}
	m.readHistory[k] = history
}

func (m *Metrics) readHistorySnapshot() map[readTargetKey][]string {
	m.readHistoryMu.RLock()
	defer m.readHistoryMu.RUnlock()
	out := make(map[readTargetKey][]string, len(m.readHistory))
	for k, v := range m.readHistory {
		out[k] = append([]string(nil), v...)
	}
	return out
}

func lastReadResult(history []string) string {
	if len(history) == 0 {
		return ""
	}
	return history[len(history)-1]
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
