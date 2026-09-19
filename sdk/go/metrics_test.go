package observe

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// captureMetricsServer returns an httptest server that records every
// /v1/metrics POST body so tests can assert on the OTLP wire shape.
func captureMetricsServer(t *testing.T, posts *atomic.Int32, bodies *[]map[string]any, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/metrics" {
			w.WriteHeader(404)
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			w.WriteHeader(500)
			return
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode: %v", err)
			w.WriteHeader(500)
			return
		}
		mu.Lock()
		*bodies = append(*bodies, body)
		mu.Unlock()
		posts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"points":1}`))
	}))
}

func TestCounterFlushEmitsOTLPSum(t *testing.T) {
	var posts atomic.Int32
	var bodies []map[string]any
	var mu sync.Mutex
	srv := captureMetricsServer(t, &posts, &bodies, &mu)
	defer srv.Close()

	c, err := New(Options{
		Endpoint:         srv.URL,
		ServiceName:      "test-svc",
		LogFlushInterval: time.Hour, // explicit flush only
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctr := c.Counter("requests_total")
	ctr.Add(3, L("route", "/login"))
	ctr.Add(2, L("route", "/login"))
	ctr.Add(7, L("route", "/checkout"))

	if err := c.FlushMetrics(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 {
		t.Fatalf("expected 1 POST, got %d", posts.Load())
	}

	mu.Lock()
	defer mu.Unlock()
	body := bodies[0]
	rms := body["resourceMetrics"].([]any)
	if len(rms) != 1 {
		t.Fatalf("expected 1 resourceMetrics, got %d", len(rms))
	}
	scope := rms[0].(map[string]any)["scopeMetrics"].([]any)[0].(map[string]any)
	metrics := scope["metrics"].([]any)
	if len(metrics) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(metrics))
	}
	m := metrics[0].(map[string]any)
	if m["name"] != "requests_total" {
		t.Errorf("name = %v", m["name"])
	}
	sum := m["sum"].(map[string]any)
	if sum["isMonotonic"] != true {
		t.Errorf("isMonotonic = %v", sum["isMonotonic"])
	}
	dps := sum["dataPoints"].([]any)
	if len(dps) != 2 {
		t.Errorf("expected 2 datapoints (one per label set), got %d", len(dps))
	}
	// Check that the sum values are 5 (login) and 7 (checkout) in some order.
	values := []float64{}
	for _, dp := range dps {
		v := dp.(map[string]any)["asDouble"].(float64)
		values = append(values, v)
	}
	if !((values[0] == 5 && values[1] == 7) || (values[0] == 7 && values[1] == 5)) {
		t.Errorf("expected {5,7}, got %v", values)
	}
}

func TestGaugeFlushEmitsOTLPGauge(t *testing.T) {
	var posts atomic.Int32
	var bodies []map[string]any
	var mu sync.Mutex
	srv := captureMetricsServer(t, &posts, &bodies, &mu)
	defer srv.Close()

	c, _ := New(Options{Endpoint: srv.URL, ServiceName: "g", LogFlushInterval: time.Hour})
	defer c.Close()

	g := c.Gauge("queue_depth")
	g.Set(5)
	g.Set(10)
	g.Set(7, L("queue", "low_priority"))

	if err := c.FlushMetrics(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	scope := bodies[0]["resourceMetrics"].([]any)[0].(map[string]any)["scopeMetrics"].([]any)[0].(map[string]any)
	metrics := scope["metrics"].([]any)
	if len(metrics) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(metrics))
	}
	m := metrics[0].(map[string]any)
	if m["name"] != "queue_depth" {
		t.Errorf("name = %v", m["name"])
	}
	if _, ok := m["gauge"]; !ok {
		t.Errorf("expected 'gauge' field, got %v", m)
	}
	dps := m["gauge"].(map[string]any)["dataPoints"].([]any)
	if len(dps) != 3 {
		t.Errorf("expected 3 gauge points, got %d", len(dps))
	}
}

func TestHistogramFlushEmitsOTLPHistogram(t *testing.T) {
	var posts atomic.Int32
	var bodies []map[string]any
	var mu sync.Mutex
	srv := captureMetricsServer(t, &posts, &bodies, &mu)
	defer srv.Close()

	c, _ := New(Options{Endpoint: srv.URL, ServiceName: "h", LogFlushInterval: time.Hour})
	defer c.Close()

	h := c.Histogram("latency_ms", 10, 50, 100, 500)
	h.Observe(5)    // bucket 0
	h.Observe(40)   // bucket 1
	h.Observe(75)   // bucket 2
	h.Observe(200)  // bucket 3
	h.Observe(1000) // bucket 4 (overflow)

	if err := c.FlushMetrics(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	scope := bodies[0]["resourceMetrics"].([]any)[0].(map[string]any)["scopeMetrics"].([]any)[0].(map[string]any)
	metrics := scope["metrics"].([]any)
	if len(metrics) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(metrics))
	}
	m := metrics[0].(map[string]any)
	hist := m["histogram"].(map[string]any)
	dps := hist["dataPoints"].([]any)
	if len(dps) != 1 {
		t.Fatalf("expected 1 datapoint, got %d", len(dps))
	}
	dp := dps[0].(map[string]any)
	if dp["count"] != "5" {
		t.Errorf("count = %v want 5", dp["count"])
	}
	if dp["sum"].(float64) != 1320 {
		t.Errorf("sum = %v want 1320", dp["sum"])
	}
	bucketCounts := dp["bucketCounts"].([]any)
	if len(bucketCounts) != 5 {
		t.Errorf("expected 5 bucket counts (4 bounds + overflow), got %d", len(bucketCounts))
	}
	for i, want := range []string{"1", "1", "1", "1", "1"} {
		if bucketCounts[i] != want {
			t.Errorf("bucket %d = %v, want %s", i, bucketCounts[i], want)
		}
	}
}

func TestCounterRetainsCumulativeValueAcrossFlushes(t *testing.T) {
	// After a flush the counter's running total must be preserved so
	// the next flush continues to report monotonically-increasing values.
	var posts atomic.Int32
	var bodies []map[string]any
	var mu sync.Mutex
	srv := captureMetricsServer(t, &posts, &bodies, &mu)
	defer srv.Close()

	c, _ := New(Options{Endpoint: srv.URL, LogFlushInterval: time.Hour})
	defer c.Close()

	ctr := c.Counter("hits")
	ctr.Add(1)
	if err := c.FlushMetrics(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctr.Add(2)
	if err := c.FlushMetrics(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("want 2 bodies, got %d", len(bodies))
	}
	get := func(b map[string]any) float64 {
		scope := b["resourceMetrics"].([]any)[0].(map[string]any)["scopeMetrics"].([]any)[0].(map[string]any)
		m := scope["metrics"].([]any)[0].(map[string]any)
		return m["sum"].(map[string]any)["dataPoints"].([]any)[0].(map[string]any)["asDouble"].(float64)
	}
	if v := get(bodies[0]); v != 1 {
		t.Errorf("first flush = %v, want 1", v)
	}
	if v := get(bodies[1]); v != 3 {
		t.Errorf("second flush = %v, want 3 (cumulative)", v)
	}
}

// TestConcurrentAddDuringFlushConservesTotal guards the re-seed race: when
// Add() runs concurrently with FlushMetrics, every increment must survive. The
// old two-critical-section re-seed could overwrite an Add() that landed between
// the re-create and the value-restore. Run with -race for the data race itself;
// the conservation assertion catches lost updates even without the race flag.
func TestConcurrentAddDuringFlushConservesTotal(t *testing.T) {
	var posts atomic.Int32
	var bodies []map[string]any
	var mu sync.Mutex
	srv := captureMetricsServer(t, &posts, &bodies, &mu)
	defer srv.Close()

	c, _ := New(Options{Endpoint: srv.URL, LogFlushInterval: time.Hour})
	defer c.Close()

	const goroutines, perG = 8, 500
	ctr := c.Counter("hits")

	var writers sync.WaitGroup
	done := make(chan struct{})
	var flusher sync.WaitGroup
	// Flusher: hammer FlushMetrics concurrently with the writers until the
	// writers signal completion via done.
	flusher.Add(1)
	go func() {
		defer flusher.Done()
		for {
			select {
			case <-done:
				return
			default:
				_ = c.FlushMetrics(context.Background())
			}
		}
	}()
	for g := 0; g < goroutines; g++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := 0; i < perG; i++ {
				ctr.Add(1)
			}
		}()
	}
	writers.Wait()
	close(done)
	flusher.Wait()
	if err := c.FlushMetrics(context.Background()); err != nil {
		t.Fatal(err)
	}

	c.mu.Lock()
	mb := c.metrics
	c.mu.Unlock()
	mb.mu.Lock()
	var got float64
	for _, st := range mb.counters {
		got += st.value
	}
	mb.mu.Unlock()
	if want := float64(goroutines * perG); got != want {
		t.Fatalf("counter total = %v, want %v (lost updates from flush race)", got, want)
	}
}

func TestNegativeCounterAddIsDropped(t *testing.T) {
	c, _ := New(Options{Endpoint: "http://example", LogFlushInterval: time.Hour})
	defer c.Close()
	c.Counter("x").Add(-1)
	c.mu.Lock()
	mb := c.metrics
	c.mu.Unlock()
	if mb != nil && len(mb.counters) != 0 {
		t.Errorf("negative add should be dropped, buf=%v", mb.counters)
	}
}

// TO-040: histograms export DELTA temporality with interval start/end
// timestamps; counters stay cumulative with a series start time.
func TestHistogramTemporalityDeltaAndCounterCumulative(t *testing.T) {
	var posts atomic.Int32
	var bodies []map[string]any
	var mu sync.Mutex
	srv := captureMetricsServer(t, &posts, &bodies, &mu)
	defer srv.Close()

	c, _ := New(Options{Endpoint: srv.URL, ServiceName: "t040", LogFlushInterval: time.Hour})
	defer c.Close()

	h := c.Histogram("lat", 10, 100)
	h.Observe(5)
	h.Observe(50)
	ctr := c.Counter("hits")
	ctr.Add(2)
	if err := c.FlushMetrics(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Second interval: one histogram sample, one counter increment.
	h.Observe(5)
	ctr.Add(1)
	if err := c.FlushMetrics(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("expected 2 exports, got %d", len(bodies))
	}
	hist := func(b map[string]any) map[string]any {
		scope := b["resourceMetrics"].([]any)[0].(map[string]any)["scopeMetrics"].([]any)[0].(map[string]any)
		for _, m := range scope["metrics"].([]any) {
			mm := m.(map[string]any)
			if _, ok := mm["histogram"]; ok {
				return mm["histogram"].(map[string]any)
			}
		}
		return nil
	}
	sum := func(b map[string]any) map[string]any {
		scope := b["resourceMetrics"].([]any)[0].(map[string]any)["scopeMetrics"].([]any)[0].(map[string]any)
		for _, m := range scope["metrics"].([]any) {
			mm := m.(map[string]any)
			if _, ok := mm["sum"]; ok {
				return mm["sum"].(map[string]any)
			}
		}
		return nil
	}
	for i, b := range bodies {
		hh := hist(b)
		if hh == nil {
			t.Fatalf("export %d: no histogram", i)
		}
		if got := hh["aggregationTemporality"].(float64); got != 1 {
			t.Fatalf("export %d: histogram temporality = %v, want 1 (delta)", i, got)
		}
		dp := hh["dataPoints"].([]any)[0].(map[string]any)
		if dp["startTimeUnixNano"] == nil {
			t.Fatalf("export %d: histogram point lacks startTimeUnixNano", i)
		}
		ss := sum(b)
		if ss == nil {
			t.Fatalf("export %d: no counter", i)
		}
		if got := ss["aggregationTemporality"].(float64); got != 2 {
			t.Fatalf("export %d: counter temporality = %v, want 2 (cumulative)", i, got)
		}
		cdp := ss["dataPoints"].([]any)[0].(map[string]any)
		if cdp["startTimeUnixNano"] == nil {
			t.Fatalf("export %d: counter point lacks startTimeUnixNano", i)
		}
	}
	count1 := hist(bodies[0])["dataPoints"].([]any)[0].(map[string]any)["count"]
	count2 := hist(bodies[1])["dataPoints"].([]any)[0].(map[string]any)["count"]
	if count1 != "2" || count2 != "1" {
		t.Fatalf("histogram counts must be per-interval deltas: %v then %v", count1, count2)
	}
	v1 := sum(bodies[0])["dataPoints"].([]any)[0].(map[string]any)["asDouble"].(float64)
	v2 := sum(bodies[1])["dataPoints"].([]any)[0].(map[string]any)["asDouble"].(float64)
	if v1 != 2 || v2 != 3 {
		t.Fatalf("counter must accumulate cumulatively: %v then %v", v1, v2)
	}
}

// TO-041: the label-key encoding cannot collide — the delimiter-carrying
// value and the two-label set are two distinct series.
func TestMetricLabelEncodingDoesNotCollide(t *testing.T) {
	var posts atomic.Int32
	var bodies []map[string]any
	var mu sync.Mutex
	srv := captureMetricsServer(t, &posts, &bodies, &mu)
	defer srv.Close()

	c, _ := New(Options{Endpoint: srv.URL, ServiceName: "t041", LogFlushInterval: time.Hour})
	defer c.Close()

	one := c.Counter("c")
	one.Add(1, Label{Key: "a", Value: "x\x01b=y"})
	two := c.Counter("c")
	two.Add(1, Label{Key: "a", Value: "x"}, Label{Key: "b", Value: "y"})
	if err := c.FlushMetrics(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	scope := bodies[0]["resourceMetrics"].([]any)[0].(map[string]any)["scopeMetrics"].([]any)[0].(map[string]any)
	sum := scope["metrics"].([]any)[0].(map[string]any)["sum"].(map[string]any)
	dps := sum["dataPoints"].([]any)
	if len(dps) != 2 {
		t.Fatalf("the colliding label sets must stay 2 distinct series, got %d", len(dps))
	}
}

// TO-042: non-finite values and duplicate bounds are rejected at the
// boundary with a report; a poisoned observation cannot kill the export.
func TestMetricValidationRejectsNonFiniteAndBadBounds(t *testing.T) {
	var errs []error
	c, _ := New(Options{
		Endpoint:         "http://unused.example",
		ServiceName:      "t042",
		LogFlushInterval: time.Hour,
		OnError:          func(e error) { errs = append(errs, e) },
	})
	defer c.Close()

	c.Counter("n").Add(math.NaN())
	c.Counter("n").Add(math.Inf(1))
	c.Counter("n").Add(-1)
	c.Gauge("g").Set(math.Inf(-1))
	badHist := c.Histogram("bad", 10, 10) // duplicate bounds
	badHist.Observe(1)
	good := c.Counter("ok")
	good.Add(1)
	// The good series must still export (a local capture server).
	var posts atomic.Int32
	var bodies []map[string]any
	var mu sync.Mutex
	srv := captureMetricsServer(t, &posts, &bodies, &mu)
	defer srv.Close()
	c.opts.Endpoint = srv.URL
	if err := c.FlushMetrics(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	scope := bodies[0]["resourceMetrics"].([]any)[0].(map[string]any)["scopeMetrics"].([]any)[0].(map[string]any)
	if len(scope["metrics"].([]any)) != 1 {
		t.Fatalf("only the healthy series should export, got %d", len(scope["metrics"].([]any)))
	}
	if len(errs) == 0 {
		t.Fatal("the rejected observations must be reported through OnError")
	}
}

// TO-039: a failed metric export retains its frozen envelope and re-sends
// the SAME body once the endpoint recovers.
func TestFailedMetricExportIsRetainedAndRetried(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	var sentBodies []string
	blocking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		sentBodies = append(sentBodies, string(raw))
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	defer blocking.Close()

	c, _ := New(Options{Endpoint: blocking.URL, ServiceName: "t039", LogFlushInterval: time.Hour})
	defer c.Close()
	c.Gauge("g").Set(1.25)
	if err := c.FlushMetrics(context.Background()); err == nil {
		t.Fatal("the first export must fail against the 503 endpoint")
	}
	fail.Store(false)
	if err := c.FlushMetrics(context.Background()); err != nil {
		t.Fatalf("retry must succeed: %v", err)
	}
	if len(sentBodies) != 2 {
		t.Fatalf("expected the same envelope twice (fail + retry), got %d", len(sentBodies))
	}
	if sentBodies[0] != sentBodies[1] {
		t.Fatal("the retry must resend the exact frozen body")
	}
}
