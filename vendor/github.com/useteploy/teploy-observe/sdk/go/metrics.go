package observe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Label is a single label key/value applied to a metric data point.
// Labels are exact-match filters in the query API — keep cardinality low.
type Label struct {
	Key   string
	Value string
}

// L is a convenience constructor for Label.
func L(k, v string) Label { return Label{Key: k, Value: v} }

// Counter is a monotonically increasing sum metric. Add() records a
// positive, finite delta — the SDK keeps the running total per label set
// and emits it on each flush as a cumulative sum.
type Counter struct {
	c    *Client
	name string
}

// Gauge is an instantaneous value metric. Each Set() emits a new
// observation to the buffer; the server keeps every point so dashboards
// can graph the raw series.
type Gauge struct {
	c    *Client
	name string
}

// Histogram is a fixed-bucket histogram. Observe() records one value
// per call; on flush the SDK emits a single OTLP histogram data point
// per (name, label set) covering all observations in the interval.
type Histogram struct {
	c        *Client
	name     string
	bounds   []float64
	disabled bool
}

// Counter returns a counter metric handle. Repeated calls with the same
// name return distinct handles that share the same underlying buffer
// keyed by the series identity — equivalent in behavior.
func (c *Client) Counter(name string) *Counter { return &Counter{c: c, name: name} }

// Gauge returns a gauge metric handle.
func (c *Client) Gauge(name string) *Gauge { return &Gauge{c: c, name: name} }

// Histogram returns a histogram metric handle. bounds are the upper-edge
// inclusive bucket boundaries (e.g. {1, 5, 10, 50, 100, 500} for ms-scale
// latency); they must be finite and strictly increasing. The implicit
// final bucket is +Inf. Invalid bounds disable the handle (reported
// through OnError; observations on it are rejected, TO-042).
func (c *Client) Histogram(name string, bounds ...float64) *Histogram {
	sorted, err := checkedHistogramBounds(bounds)
	if err != nil {
		if c != nil {
			c.reportError(fmt.Errorf("observe: histogram %q: %w — handle disabled", name, err))
		}
		return &Histogram{c: c, name: name, bounds: nil, disabled: true}
	}
	return &Histogram{c: c, name: name, bounds: sorted}
}

// Add records a counter increment. value MUST be non-negative and finite;
// anything else is reported through OnError and dropped (matching OTel
// semantics for monotonic sums — and a NaN used to contaminate the
// cumulative total forever, TO-042).
func (c *Counter) Add(value float64, labels ...Label) {
	if !finiteMetric(value) || value < 0 || c == nil || c.c == nil {
		if c != nil && c.c != nil && !finiteMetric(value) {
			c.c.reportError(errors.New("observe: counter Add rejected a non-finite value"))
		}
		return
	}
	c.c.recordCounter(c.name, value, labels)
}

// Set records a gauge observation. Negative / fractional values are fine;
// non-finite ones are rejected (TO-042: they cannot be JSON-encoded, and
// one used to poison the whole detached export).
func (g *Gauge) Set(value float64, labels ...Label) {
	if g == nil || g.c == nil {
		return
	}
	if !finiteMetric(value) {
		g.c.reportError(errors.New("observe: gauge Set rejected a non-finite value"))
		return
	}
	g.c.recordGauge(g.name, value, labels)
}

// Observe records a single histogram sample.
func (h *Histogram) Observe(value float64, labels ...Label) {
	if h == nil || h.c == nil {
		return
	}
	if h.disabled {
		h.c.reportError(errors.New("observe: observation on a disabled histogram (invalid bounds) — dropped"))
		return
	}
	if !finiteMetric(value) {
		h.c.reportError(errors.New("observe: histogram Observe rejected a non-finite value"))
		return
	}
	h.c.recordHistogram(h.name, h.bounds, value, labels)
}

// counterState holds the running cumulative value for one series. startNano
// (TO-040) lets a receiver distinguish resets of the cumulative series.
type counterState struct {
	value     float64
	startNano int64
	dirty     bool
}

// gaugePoint is a single buffered gauge observation. It carries its own
// metric name (TO-041: the old __name__ pseudo-label could collide with a
// user label and merged distinct series).
type gaugePoint struct {
	name   string
	tsNano int64
	value  float64
	labels []Label
}

// histogramState aggregates samples for one (name, label) pair across
// the flush interval. intervalStartNano (TO-040) is when this interval's
// accumulation began — the exported point is DELTA temporality.
type histogramState struct {
	bounds            []float64
	counts            []int64
	sum               float64
	count             int64
	intervalStartNano int64
}

// metricsBuf is the in-memory metrics buffer attached to Client.
type metricsBuf struct {
	mu         sync.Mutex
	counters   map[string]*counterState
	gauges     []gaugePoint
	histograms map[string]*histogramState
	seriesKeys map[string]seriesIdentity
	bounds     map[string][]float64
	bytes      int64
}

// seriesIdentity remembers the name/labels behind each series key, for
// export grouping (TO-041).
type seriesIdentity struct {
	name   string
	labels []Label
}

// Metric series/admission budgets (TO-043): a high-cardinality producer or
// a recording loop after Close must not grow memory without limit.
const (
	maxMetricSeries     = 1000
	maxBufferedGaugePts = 10000
)

// metricSeriesKey encodes one series identity UNAMBIGUOUSLY (TO-041): the
// JSON tuple of name and sorted [key,value] pairs. The old
// \x01-delimited k=v concatenation collided — the single label
// a="x\x01b=y" and two labels a="x", b="y" produced the same key and
// merged distinct series.
func metricSeriesKey(name string, labels []Label) (string, error) {
	cp := append([]Label(nil), labels...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Key < cp[j].Key })
	pairs := make([][2]string, len(cp))
	for i, l := range cp {
		if l.Key == "" || (i > 0 && cp[i-1].Key == l.Key) {
			return "", errors.New("metric label keys must be nonempty and unique")
		}
		pairs[i] = [2]string{l.Key, l.Value}
	}
	raw, err := json.Marshal(struct {
		Name   string      `json:"name"`
		Labels [][2]string `json:"labels"`
	}{name, pairs})
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// finiteMetric rejects NaN and both infinities (TO-042).
func finiteMetric(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// checkedHistogramBounds validates and sorts bucket boundaries (TO-042):
// finite and strictly increasing.
func checkedHistogramBounds(bounds []float64) ([]float64, error) {
	cp := append([]float64(nil), bounds...)
	sort.Float64s(cp)
	for i, v := range cp {
		if !finiteMetric(v) {
			return nil, errors.New("bounds must be finite")
		}
		if i > 0 && v <= cp[i-1] {
			return nil, errors.New("bounds must be strictly increasing (duplicates rejected)")
		}
	}
	return cp, nil
}

func newMetricsBuf() *metricsBuf {
	return &metricsBuf{
		counters:   map[string]*counterState{},
		histograms: map[string]*histogramState{},
		seriesKeys: map[string]seriesIdentity{},
		bounds:     map[string][]float64{},
	}
}

// ensureMetricsBuf returns the live buffer, or nil when the client is
// closing/closed (TO-043: metric handles used after Close stop
// accumulating instead of retaining memory with no consumer).
func (c *Client) ensureMetricsBuf() *metricsBuf {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return nil
	}
	if c.metrics == nil {
		c.metrics = newMetricsBuf()
	}
	return c.metrics
}

// registerSeries books a series key against the budget (TO-043); over
// budget, the observation is refused with a report.
func metricCost(name string, labels []Label, bounds []float64) int64 {
	n := len(name)
	for _, l := range labels {
		n += len(l.Key) + len(l.Value)
	}
	return int64(2048 + 6*n + 64*len(bounds))
}
func (mb *metricsBuf) registerSeries(key string, id seriesIdentity) bool {
	if _, ok := mb.seriesKeys[key]; ok {
		return true
	}
	if metricCost(id.name, id.labels, nil) > 512<<10 || len(mb.seriesKeys) >= maxMetricSeries || mb.bytes+metricCost(id.name, id.labels, nil) > 4<<20 {
		return false
	}
	id.labels = append([]Label(nil), id.labels...)
	mb.seriesKeys[key] = id
	mb.bytes += metricCost(id.name, id.labels, nil)
	return true
}

func (c *Client) recordCounter(name string, value float64, labels []Label) {
	mb := c.ensureMetricsBuf()
	if mb == nil {
		return
	}
	key, err := metricSeriesKey(name, labels)
	if err != nil {
		c.reportError(fmt.Errorf("observe: counter %q: %w", name, err))
		return
	}
	mb.mu.Lock()
	var notify func()
	defer func() {
		mb.mu.Unlock()
		if notify != nil {
			notify()
		}
	}()
	if c.isClosing() {
		return
	}
	st, ok := mb.counters[key]
	if !ok {
		if !mb.registerSeries(key, seriesIdentity{name: name, labels: labels}) {
			notify = func() { c.countLoss("metric_series_budget", 1, "metric series budget reached") }
			return
		}
		st = &counterState{startNano: time.Now().UnixNano()}
		mb.counters[key] = st
	}
	next := st.value + value
	if !finiteMetric(next) { // overflow to +Inf across finite addends
		notify = func() { c.reportError(errors.New("observe: counter overflowed — increment rejected")) }
		return
	}
	st.value = next
	st.dirty = true
}

func (c *Client) recordGauge(name string, value float64, labels []Label) {
	mb := c.ensureMetricsBuf()
	if mb == nil {
		return
	}
	key, err := metricSeriesKey(name, labels)
	if err != nil {
		c.reportError(fmt.Errorf("observe: gauge %q: %w", name, err))
		return
	}
	mb.mu.Lock()
	var notify func()
	defer func() {
		mb.mu.Unlock()
		if notify != nil {
			notify()
		}
	}()
	if c.isClosing() {
		return
	}
	if _, ok := mb.counters[key]; !ok {
		// Gauges have no persistent state; the series still counts against
		// the budget (cleaned up when the point list drains).
		if _, ok := mb.seriesKeys[key]; !ok {
			if !mb.registerSeries(key, seriesIdentity{name: name, labels: labels}) {
				notify = func() { c.countLoss("metric_series_budget", 1, "metric series budget reached") }
				return
			}
		}
	}
	if metricCost(name, labels, nil) > 512<<10 || len(mb.gauges) >= maxBufferedGaugePts || mb.bytes+metricCost(name, labels, nil) > 4<<20 {
		notify = func() { c.countLoss("gauge_point_budget", 1, "gauge point budget reached") }
		return
	}
	mb.bytes += metricCost(name, labels, nil)
	mb.gauges = append(mb.gauges, gaugePoint{
		name:   name,
		tsNano: time.Now().UnixNano(),
		value:  value,
		labels: append([]Label(nil), labels...),
	})
}

func (c *Client) recordHistogram(name string, bounds []float64, value float64, labels []Label) {
	mb := c.ensureMetricsBuf()
	if mb == nil {
		return
	}
	key, err := metricSeriesKey(name, labels)
	if err != nil {
		c.reportError(fmt.Errorf("observe: histogram %q: %w", name, err))
		return
	}
	mb.mu.Lock()
	var notify func()
	defer func() {
		mb.mu.Unlock()
		if notify != nil {
			notify()
		}
	}()
	if c.isClosing() {
		return
	}
	if prior, ok := mb.bounds[key]; ok && !slices.Equal(prior, bounds) {
		notify = func() { c.reportError(errors.New("observe: histogram bounds redefined")) }
		return
	}
	if _, ok := mb.bounds[key]; !ok {
		if metricCost(name, labels, bounds) > 512<<10 || mb.bytes+metricCost(name, labels, bounds) > 4<<20 || !mb.registerSeries(key, seriesIdentity{name: name, labels: labels}) {
			notify = func() { c.countLoss("metric_series_budget", 1, "histogram byte/series budget reached") }
			return
		}
		mb.bounds[key] = append([]float64(nil), bounds...)
		mb.bytes += int64(64 * len(bounds))
	}
	st, ok := mb.histograms[key]
	if !ok {
		if !mb.registerSeries(key, seriesIdentity{name: name, labels: labels}) {
			notify = func() { c.countLoss("metric_series_budget", 1, "metric series budget reached") }
			return
		}
		st = &histogramState{
			bounds:            append([]float64(nil), bounds...),
			counts:            make([]int64, len(bounds)+1),
			intervalStartNano: time.Now().UnixNano(),
		}
		mb.histograms[key] = st
	} else if !slices.Equal(st.bounds, bounds) {
		// TO-042: two handles over the same series with different bounds
		// silently shared whichever state existed first — reject the
		// incompatible definition instead of switching schemas between
		// flushes.
		notify = func() {
			c.reportError(fmt.Errorf("observe: histogram %q redefined with %d bounds (series has %d) — observation dropped", name, len(bounds), len(st.bounds)))
		}
		return
	}
	if !finiteMetric(st.sum + value) {
		notify = func() { c.reportError(errors.New("observe: histogram sum overflow — observation rejected")) }
		return
	}
	idx := sort.SearchFloat64s(st.bounds, value)
	if idx < len(st.counts) {
		st.counts[idx]++
	}
	st.sum += value
	st.count++
}

// frozenMetrics is one detached, export-ready snapshot (TO-039): the
// envelope is serialized ONCE and retained until its POST succeeds — a
// failed flush re-sends the same body instead of having already cleared
// the gauges/histogram state it described.
type frozenMetrics struct {
	body   string
	points int
}

// FlushMetrics emits any buffered metric points to the server. Called
// automatically by the background flush loop; exposed publicly so tests
// can force a flush without waiting on the ticker.
//
// Delivery semantics (TO-039): a failed POST retains the frozen envelope
// and it is retried by the next flush; an ambiguous outcome (timeout
// after server acceptance) can duplicate points at a receiver without
// dedupe — at-least-once, the same contract the log queue documents.
// O11: the retry budget is bounded — after MaxSendAttempts the envelope
// is dropped and counted (metrics_retry_exhausted); a permanent 4xx is
// dropped immediately (metrics_non_retryable).
func (c *Client) FlushMetrics(ctx context.Context) error {
	if err := lockContext(ctx, &c.metricFlushMu); err != nil {
		return err
	}
	defer c.metricFlushMu.Unlock()
	if !c.isClosing() && backoffGated(c.metricAttempts, c.metricNotBefore) {
		return nil
	}
	c.mu.Lock()
	mb := c.metrics
	c.mu.Unlock()
	if mb == nil {
		return nil
	}
	snapshotted := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		pending := c.pendingMetrics
		c.mu.Unlock()
		if len(pending) == 0 {
			if snapshotted {
				return nil
			}
			envelopes, err := c.snapshotMetricRequests(mb)
			if err != nil {
				return err
			}
			snapshotted = true
			if len(envelopes) == 0 {
				return nil
			}
			c.mu.Lock()
			c.pendingMetrics = envelopes
			pending = envelopes
			c.mu.Unlock()
		}
		req := pending[0]
		if err := c.postMetricsBody(ctx, req.body); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !retryableSendErr(err) {
				c.dropPendingMetric(req, "metrics_non_retryable", err.Error())
			} else {
				c.metricAttempts++
				if c.metricAttempts >= c.maxSendAttempts {
					c.dropPendingMetric(req, "metrics_retry_exhausted", err.Error())
				} else {
					c.countDelivered(func(s *Stats) { s.Retries++ })
					c.metricNotBefore = time.Now().Add(c.backoffFor(c.metricAttempts))
				}
			}
			return err
		}
		c.metricAttempts = 0
		c.countDelivered(func(s *Stats) { s.DeliveredMetricPoints += int64(req.points) })
		c.mu.Lock()
		c.pendingMetrics = c.pendingMetrics[1:]
		c.mu.Unlock()
	}
}

// dropPendingMetric removes the head retained envelope (the one whose send
// just failed) and books its points as a counted loss (O11). Called with
// metricFlushMu held.
func (c *Client) dropPendingMetric(req *frozenMetrics, reason, detail string) {
	c.mu.Lock()
	if len(c.pendingMetrics) > 0 && c.pendingMetrics[0] == req {
		c.pendingMetrics = c.pendingMetrics[1:]
	}
	c.mu.Unlock()
	c.metricAttempts = 0
	c.countLoss(reason, int64(req.points), detail)
}

// snapshotMetrics detaches the current state under one lock hold and
// serializes it. Counters are re-seeded (cumulative); gauges and
// histograms are cleared — the frozen envelope now carries them. A fully
// idle buffer exports nothing (an empty envelope is not a data point).
func (c *Client) snapshotMetricRequests(mb *metricsBuf) ([]*frozenMetrics, error) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if len(mb.counters)+len(mb.gauges)+len(mb.histograms) == 0 {
		return nil, nil
	}
	// Serialize before detaching so errors leave interval state recoverable.
	var reqs []*frozenMetrics
	cs := map[string]*counterState{}
	var gs []gaugePoint
	hs := map[string]*histogramState{}
	overhead := int64(1024 + 6*(len(c.opts.ServiceName)+len(c.opts.Environment)))
	size := overhead
	flush := func() error {
		if len(cs)+len(gs)+len(hs) == 0 {
			return nil
		}
		raw, err := json.Marshal(buildMetricsOTLP(c.opts.ServiceName, c.opts.Environment, cs, gs, hs, mb.seriesKeys))
		if err != nil {
			return err
		}
		if len(raw) > 1<<20 {
			return errors.New("observe: metric envelope exceeds byte budget")
		}
		reqs = append(reqs, &frozenMetrics{body: string(raw), points: len(cs) + len(gs) + len(hs)})
		cs = map[string]*counterState{}
		gs = nil
		hs = map[string]*histogramState{}
		size = overhead
		return nil
	}
	reserve := func(cost int64) error {
		if size+cost > 1<<20 {
			if err := flush(); err != nil {
				return err
			}
		}
		size += cost
		return nil
	}
	for k, st := range mb.counters {
		if !st.dirty {
			continue
		}
		id := mb.seriesKeys[k]
		if err := reserve(metricCost(id.name, id.labels, nil)); err != nil {
			return nil, err
		}
		cs[k] = st
	}
	for _, g := range mb.gauges {
		if err := reserve(metricCost(g.name, g.labels, nil)); err != nil {
			return nil, err
		}
		gs = append(gs, g)
	}
	for k, st := range mb.histograms {
		id := mb.seriesKeys[k]
		if err := reserve(metricCost(id.name, id.labels, st.bounds)); err != nil {
			return nil, err
		}
		hs[k] = st
	}
	if err := flush(); err != nil {
		return nil, err
	}
	for _, st := range mb.counters {
		st.dirty = false
	}
	mb.gauges = nil
	mb.histograms = map[string]*histogramState{}
	mb.bytes = 0
	for key, id := range mb.seriesKeys {
		_, counter := mb.counters[key]
		_, histogram := mb.bounds[key]
		if !counter && !histogram {
			delete(mb.seriesKeys, key)
			continue
		}
		mb.bytes += metricCost(id.name, id.labels, nil)
	}
	for _, bounds := range mb.bounds {
		mb.bytes += int64(64 * len(bounds))
	}
	return reqs, nil
}

// postMetricsBody sends one frozen envelope body to the OTLP metrics
// endpoint.
func (c *Client) postMetricsBody(ctx context.Context, body string) error {
	url := c.opts.Endpoint
	for len(url) > 0 && url[len(url)-1] == '/' {
		url = url[:len(url)-1]
	}
	url += "/v1/metrics"
	return c.postRaw(ctx, url, json.RawMessage(body), map[string]string{"X-Observe-Site": c.opts.SiteID})
}

// buildMetricsOTLP assembles an ExportMetricsServiceRequest in OTLP JSON
// shape. Mirrors buildOTLPRequest in traces.go so the SDK has zero
// dependency on the server packages.
//
// TO-040: histograms export DELTA temporality (1) with the interval's
// start time — their accumulation resets every flush, and the old
// cumulative label told receivers to compute deltas/rates off resets.
// Counters are genuinely cumulative (2, re-seeded across flushes) and now
// carry their series start time so receivers can distinguish resets.
func buildMetricsOTLP(serviceName, environment string,
	counters map[string]*counterState,
	gauges []gaugePoint,
	histograms map[string]*histogramState,
	series map[string]seriesIdentity,
) map[string]any {
	resourceAttrs := []map[string]any{}
	if serviceName != "" {
		resourceAttrs = append(resourceAttrs, map[string]any{
			"key":   "service.name",
			"value": map[string]any{"stringValue": serviceName},
		})
	}
	if environment != "" {
		resourceAttrs = append(resourceAttrs, map[string]any{
			"key":   "deployment.environment",
			"value": map[string]any{"stringValue": environment},
		})
	}

	now := strconv.FormatInt(time.Now().UnixNano(), 10)
	otlpMetrics := []map[string]any{}

	// Counters → OTLP sum (monotonic, cumulative).
	counterByName := map[string][]map[string]any{}
	for key, st := range counters {
		id := series[key]
		dp := map[string]any{
			"startTimeUnixNano": strconv.FormatInt(st.startNano, 10),
			"timeUnixNano":      now,
			"asDouble":          st.value,
			"attributes":        labelsToOTLP(id.labels),
		}
		counterByName[id.name] = append(counterByName[id.name], dp)
	}
	for name, dps := range counterByName {
		otlpMetrics = append(otlpMetrics, map[string]any{
			"name": name,
			"sum": map[string]any{
				"dataPoints":             dps,
				"aggregationTemporality": 2, // cumulative
				"isMonotonic":            true,
			},
		})
	}

	// Gauges → OTLP gauge, one data point per observation, grouped by the
	// name every point now carries (TO-041: no __name__ pseudo-label).
	gaugeByName := map[string][]map[string]any{}
	for _, g := range gauges {
		dp := map[string]any{
			"timeUnixNano": strconv.FormatInt(g.tsNano, 10),
			"asDouble":     g.value,
			"attributes":   labelsToOTLP(g.labels),
		}
		gaugeByName[g.name] = append(gaugeByName[g.name], dp)
	}
	for name, dps := range gaugeByName {
		otlpMetrics = append(otlpMetrics, map[string]any{
			"name":  name,
			"gauge": map[string]any{"dataPoints": dps},
		})
	}

	// Histograms → OTLP histogram, one DELTA data point per (name, labels)
	// covering this interval's observations.
	histByName := map[string][]map[string]any{}
	for key, st := range histograms {
		id := series[key]
		bucketCounts := make([]string, len(st.counts))
		for i, n := range st.counts {
			bucketCounts[i] = strconv.FormatInt(n, 10)
		}
		dp := map[string]any{
			"startTimeUnixNano": strconv.FormatInt(st.intervalStartNano, 10),
			"timeUnixNano":      now,
			"count":             strconv.FormatInt(st.count, 10),
			"sum":               st.sum,
			"bucketCounts":      bucketCounts,
			"explicitBounds":    st.bounds,
			"attributes":        labelsToOTLP(id.labels),
		}
		histByName[id.name] = append(histByName[id.name], dp)
	}
	for name, dps := range histByName {
		otlpMetrics = append(otlpMetrics, map[string]any{
			"name": name,
			"histogram": map[string]any{
				"dataPoints":             dps,
				"aggregationTemporality": 1, // delta: observations since the interval start
			},
		})
	}

	return map[string]any{
		"resourceMetrics": []map[string]any{{
			"resource": map[string]any{"attributes": resourceAttrs},
			"scopeMetrics": []map[string]any{{
				"scope":   map[string]any{"name": "github.com/useteploy/teploy-observe/sdk/go", "version": "0.1.0"},
				"metrics": otlpMetrics,
			}},
		}},
	}
}

func labelsToOTLP(labels []Label) []map[string]any {
	out := make([]map[string]any, 0, len(labels))
	for _, l := range labels {
		out = append(out, map[string]any{
			"key":   l.Key,
			"value": map[string]any{"stringValue": l.Value},
		})
	}
	return out
}
