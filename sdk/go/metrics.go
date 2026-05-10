package observe

import (
	"context"
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
// positive delta — the SDK keeps the running total per label set and
// emits it on each flush as a cumulative sum.
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
	c      *Client
	name   string
	bounds []float64
}

// Counter returns a counter metric handle. Repeated calls with the same
// name return distinct handles that share the same underlying buffer
// keyed by name + labels — equivalent in behavior.
func (c *Client) Counter(name string) *Counter { return &Counter{c: c, name: name} }

// Gauge returns a gauge metric handle.
func (c *Client) Gauge(name string) *Gauge { return &Gauge{c: c, name: name} }

// Histogram returns a histogram metric handle. bounds are the upper-edge
// inclusive bucket boundaries (e.g. {1, 5, 10, 50, 100, 500} for ms-scale
// latency). The implicit final bucket is +Inf.
func (c *Client) Histogram(name string, bounds ...float64) *Histogram {
	sortedBounds := append([]float64(nil), bounds...)
	sort.Float64s(sortedBounds)
	return &Histogram{c: c, name: name, bounds: sortedBounds}
}

// Add records a counter increment. value MUST be non-negative; negative
// values are silently dropped (matching OTel semantics for monotonic sums).
func (c *Counter) Add(value float64, labels ...Label) {
	if value < 0 || c == nil || c.c == nil {
		return
	}
	c.c.recordCounter(c.name, value, labels)
}

// Set records a gauge observation. Negative / fractional values are fine.
func (g *Gauge) Set(value float64, labels ...Label) {
	if g == nil || g.c == nil {
		return
	}
	g.c.recordGauge(g.name, value, labels)
}

// Observe records a single histogram sample.
func (h *Histogram) Observe(value float64, labels ...Label) {
	if h == nil || h.c == nil {
		return
	}
	h.c.recordHistogram(h.name, h.bounds, value, labels)
}

// counterState holds the running cumulative value for one (name, label) pair.
type counterState struct {
	value float64
}

// gaugePoint is a single buffered gauge observation.
type gaugePoint struct {
	tsNano int64
	value  float64
	labels []Label
}

// histogramState aggregates samples for one (name, label) pair across
// the flush interval. Only the bucket counts + sum + total need to ship.
type histogramState struct {
	bounds []float64
	counts []int64
	sum    float64
	count  int64
}

// metricsBuf is the in-memory metrics buffer attached to Client. We keep
// it inside the same Client struct as logs / spans so the existing flush
// loop can amortize the wakeup.
type metricsBuf struct {
	mu         sync.Mutex
	counters   map[string]*counterState // key = name + "\x00" + labelKey(labels)
	gauges     []gaugePoint             // each gauge Set() is a separate point
	histograms map[string]*histogramState
	gaugeNames map[string]string  // key -> name
	gaugeLbls  map[string][]Label // key -> labels
	cnLabels   map[string][]Label // counter key -> labels
	cnNames    map[string]string
	hLabels    map[string][]Label
	hNames     map[string]string
}

func newMetricsBuf() *metricsBuf {
	return &metricsBuf{
		counters:   map[string]*counterState{},
		histograms: map[string]*histogramState{},
		gaugeNames: map[string]string{},
		gaugeLbls:  map[string][]Label{},
		cnLabels:   map[string][]Label{},
		cnNames:    map[string]string{},
		hLabels:    map[string][]Label{},
		hNames:     map[string]string{},
	}
}

// labelKey produces a stable identifier for a label set. Sorting before
// joining is what makes (k=v, j=w) and (j=w, k=v) collapse to the same key.
func labelKey(labels []Label) string {
	if len(labels) == 0 {
		return ""
	}
	cp := append([]Label(nil), labels...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Key < cp[j].Key })
	var b []byte
	for _, l := range cp {
		b = append(b, l.Key...)
		b = append(b, '=')
		b = append(b, l.Value...)
		b = append(b, '\x01')
	}
	return string(b)
}

func (c *Client) ensureMetricsBuf() *metricsBuf {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.metrics == nil {
		c.metrics = newMetricsBuf()
	}
	return c.metrics
}

func (c *Client) recordCounter(name string, value float64, labels []Label) {
	mb := c.ensureMetricsBuf()
	key := name + "\x00" + labelKey(labels)
	mb.mu.Lock()
	st, ok := mb.counters[key]
	if !ok {
		st = &counterState{}
		mb.counters[key] = st
		mb.cnLabels[key] = append([]Label(nil), labels...)
		mb.cnNames[key] = name
	}
	st.value += value
	mb.mu.Unlock()
}

func (c *Client) recordGauge(name string, value float64, labels []Label) {
	mb := c.ensureMetricsBuf()
	mb.mu.Lock()
	mb.gauges = append(mb.gauges, gaugePoint{
		tsNano: time.Now().UnixNano(),
		value:  value,
		labels: append([]Label(nil), labels...),
	})
	// Also track names for the OTLP envelope grouping. Use the gauge slice
	// index (we'll dedup by name on flush).
	_ = name
	// Cheap workaround: re-purpose gaugeNames keyed on name+labelKey to
	// guarantee a stable Resource->Metric mapping on flush.
	key := name + "\x00" + labelKey(labels)
	mb.gaugeNames[key] = name
	mb.gaugeLbls[key] = append([]Label(nil), labels...)
	// Embed the name in the point itself so flushMetrics can group.
	mb.gauges[len(mb.gauges)-1].labels = append(mb.gauges[len(mb.gauges)-1].labels, Label{Key: "__name__", Value: name})
	mb.mu.Unlock()
}

func (c *Client) recordHistogram(name string, bounds []float64, value float64, labels []Label) {
	mb := c.ensureMetricsBuf()
	key := name + "\x00" + labelKey(labels)
	mb.mu.Lock()
	st, ok := mb.histograms[key]
	if !ok {
		st = &histogramState{
			bounds: append([]float64(nil), bounds...),
			counts: make([]int64, len(bounds)+1),
		}
		mb.histograms[key] = st
		mb.hLabels[key] = append([]Label(nil), labels...)
		mb.hNames[key] = name
	}
	idx := sort.SearchFloat64s(st.bounds, value)
	if idx < len(st.counts) {
		st.counts[idx]++
	}
	st.sum += value
	st.count++
	mb.mu.Unlock()
}

// FlushMetrics emits any buffered metric points to the server. Called
// automatically by the background flush loop; exposed publicly so tests
// can force a flush without waiting on the ticker.
func (c *Client) FlushMetrics(ctx context.Context) error {
	c.mu.Lock()
	mb := c.metrics
	c.mu.Unlock()
	if mb == nil {
		return nil
	}

	mb.mu.Lock()
	if len(mb.counters) == 0 && len(mb.gauges) == 0 && len(mb.histograms) == 0 {
		mb.mu.Unlock()
		return nil
	}
	counters := mb.counters
	gauges := mb.gauges
	histograms := mb.histograms
	cnLabels := mb.cnLabels
	cnNames := mb.cnNames
	hLabels := mb.hLabels
	hNames := mb.hNames

	mb.counters = map[string]*counterState{}
	mb.gauges = nil
	mb.histograms = map[string]*histogramState{}
	mb.cnLabels = map[string][]Label{}
	mb.cnNames = map[string]string{}
	mb.gaugeNames = map[string]string{}
	mb.gaugeLbls = map[string][]Label{}
	mb.hLabels = map[string][]Label{}
	mb.hNames = map[string]string{}
	mb.mu.Unlock()

	// Snapshot counter states preserving cumulative semantics — re-seed
	// the next interval with the running totals so consumers see a
	// monotonically increasing series.
	for key, st := range counters {
		st2 := *st
		c.recordCounter(cnNames[key], 0, cnLabels[key]) // re-create entry
		c.metrics.mu.Lock()
		c.metrics.counters[key].value = st2.value
		c.metrics.mu.Unlock()
	}

	envelope := buildMetricsOTLP(c.opts.ServiceName, c.opts.Environment, counters, gauges, histograms, cnNames, cnLabels, hNames, hLabels)
	return c.postMetrics(ctx, envelope)
}

func (c *Client) postMetrics(ctx context.Context, body any) error {
	url := c.opts.Endpoint
	for len(url) > 0 && url[len(url)-1] == '/' {
		url = url[:len(url)-1]
	}
	url += "/v1/metrics"
	return c.postRaw(ctx, url, body, map[string]string{"X-Observe-Site": c.opts.SiteID})
}

// buildMetricsOTLP assembles an ExportMetricsServiceRequest in OTLP JSON
// shape. Mirrors buildOTLPRequest in traces.go so the SDK has zero
// dependency on the server packages.
func buildMetricsOTLP(serviceName, environment string,
	counters map[string]*counterState,
	gauges []gaugePoint,
	histograms map[string]*histogramState,
	cnNames map[string]string, cnLabels map[string][]Label,
	hNames map[string]string, hLabels map[string][]Label,
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
		dp := map[string]any{
			"timeUnixNano": now,
			"asDouble":     st.value,
			"attributes":   labelsToOTLP(cnLabels[key]),
		}
		counterByName[cnNames[key]] = append(counterByName[cnNames[key]], dp)
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

	// Gauges → OTLP gauge. Group by the synthetic __name__ label so each
	// metric name lands in its own OTLPMetric entry.
	gaugeByName := map[string][]map[string]any{}
	for _, g := range gauges {
		var name string
		filtered := make([]Label, 0, len(g.labels))
		for _, l := range g.labels {
			if l.Key == "__name__" {
				name = l.Value
				continue
			}
			filtered = append(filtered, l)
		}
		dp := map[string]any{
			"timeUnixNano": strconv.FormatInt(g.tsNano, 10),
			"asDouble":     g.value,
			"attributes":   labelsToOTLP(filtered),
		}
		gaugeByName[name] = append(gaugeByName[name], dp)
	}
	for name, dps := range gaugeByName {
		otlpMetrics = append(otlpMetrics, map[string]any{
			"name": name,
			"gauge": map[string]any{
				"dataPoints": dps,
			},
		})
	}

	// Histograms → OTLP histogram, one DataPoint per (name, labels).
	histByName := map[string][]map[string]any{}
	for key, st := range histograms {
		bucketCounts := make([]string, len(st.counts))
		for i, c := range st.counts {
			bucketCounts[i] = strconv.FormatInt(c, 10)
		}
		dp := map[string]any{
			"timeUnixNano":   now,
			"count":          strconv.FormatInt(st.count, 10),
			"sum":            st.sum,
			"bucketCounts":   bucketCounts,
			"explicitBounds": st.bounds,
			"attributes":     labelsToOTLP(hLabels[key]),
		}
		histByName[hNames[key]] = append(histByName[hNames[key]], dp)
	}
	for name, dps := range histByName {
		otlpMetrics = append(otlpMetrics, map[string]any{
			"name": name,
			"histogram": map[string]any{
				"dataPoints":             dps,
				"aggregationTemporality": 2, // cumulative
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
