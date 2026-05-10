package observe

import (
	"context"
	"encoding/json"
	"io"
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
