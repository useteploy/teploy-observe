package observe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type campaignSecret string

func (campaignSecret) LogValue() slog.Value { return slog.StringValue("redacted") }
func campaignClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c, err := New(Options{Endpoint: server.URL, LogBatchSize: 200, LogFlushInterval: time.Hour, MaxSendAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
func TestCampaignSlogRedactionGroupsAndBreadcrumbOwnership(t *testing.T) {
	var body string
	c := campaignClient(t, func(w http.ResponseWriter, r *http.Request) {
		var value map[string]any
		_ = json.NewDecoder(r.Body).Decode(&value)
		raw, _ := json.Marshal(value)
		body = string(raw)
		w.Write([]byte(`{"accepted":1}`))
	})
	logger := slog.New(c.NewSlogHandler(slog.LevelInfo, nil).WithBreadcrumbs(slog.LevelInfo))
	logger.With("id", "outer", "secret", campaignSecret("RAW_SECRET")).WithGroup("request").With("scope", campaignSecret("RAW_SCOPE")).Info("event", "id", "inner", slog.Group("nested", "token", campaignSecret("RAW_TOKEN")))
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "RAW_") || !strings.Contains(body, `"request.id":"inner"`) || !strings.Contains(body, `"id":"outer"`) || !strings.Contains(body, `"request.nested.token":"redacted"`) {
		t.Fatal(body)
	}
	c.AddBreadcrumb(Breadcrumb{Message: "nested", Data: map[string]any{"nested": map[string]any{"list": []any{"original"}}}})
	first := c.Breadcrumbs()
	first[len(first)-1].Data["nested"].(map[string]any)["list"].([]any)[0] = "changed"
	if c.Breadcrumbs()[len(first)-1].Data["nested"].(map[string]any)["list"].([]any)[0] != "original" {
		t.Fatal("snapshot aliases retained data")
	}
}
func TestCampaignShutdownExportsAllFreshMetrics(t *testing.T) {
	var metrics atomic.Int64
	c := campaignClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/metrics" {
			metrics.Add(1)
		}
		w.Write([]byte(`{"accepted":1}`))
	})
	c.Counter("counter").Add(1)
	c.Gauge("gauge").Set(2)
	c.Histogram("hist", 10, 100).Observe(3)
	c.Info("log")
	_, span := c.StartSpan(context.Background(), "span")
	span.End()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if metrics.Load() != 1 || c.Stats().DeliveredMetricPoints != 3 || c.Stats().DeliveredSpans != 1 || c.Stats().DeliveredLogs != 1 {
		t.Fatal(c.Stats())
	}
}
func TestCampaignConcurrentRetainedMetricFlushes(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	var calls atomic.Int64
	c := campaignClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte(`{}`))
	})
	c.Gauge("point").Set(1)
	if c.FlushMetrics(context.Background()) == nil {
		t.Fatal("expected failed send")
	}
	fail.Store(false)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.FlushMetrics(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 2 || c.Stats().DeliveredMetricPoints != 1 {
		t.Fatal(calls.Load(), c.Stats())
	}
}
func TestCampaignMetricOverflowLabelsAndBoundaryIdentity(t *testing.T) {
	var bodies []string
	c := campaignClient(t, func(w http.ResponseWriter, r *http.Request) {
		var v any
		_ = json.NewDecoder(r.Body).Decode(&v)
		raw, _ := json.Marshal(v)
		bodies = append(bodies, string(raw))
		w.Write([]byte(`{}`))
	})
	labels := []Label{L("tenant", "original")}
	counter := c.Counter("count")
	counter.Add(math.MaxFloat64, labels...)
	labels[0].Value = "mutated"
	counter.Add(math.MaxFloat64, L("tenant", "original"))
	h := c.Histogram("hist", 10, 100)
	h.Observe(math.MaxFloat64)
	h.Observe(math.MaxFloat64)
	c.Histogram("hist", 20, 200).Observe(15)
	c.Gauge("good").Set(1)
	if err := c.FlushMetrics(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bodies[0], "mutated") || !strings.Contains(bodies[0], "original") {
		t.Fatal(bodies)
	}
	c.Histogram("hist", 20, 200).Observe(15)
	c.metrics.mu.Lock()
	_, exists := c.metrics.histograms[mustMetricKey("hist", nil)]
	c.metrics.mu.Unlock()
	if exists {
		t.Fatal("histogram schema changed after flush")
	}
	if c.Stats().DeliveredMetricPoints != 3 {
		t.Fatal(c.Stats())
	}
}
func mustMetricKey(name string, labels []Label) string {
	key, _ := metricSeriesKey(name, labels)
	return key
}
func TestCampaignOwnershipDeadlineAndConcurrentShutdown(t *testing.T) {
	c := campaignClient(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	c.metricFlushMu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.FlushMetrics(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 200*time.Millisecond {
		t.Fatal(err, time.Since(start))
	}
	c.metricFlushMu.Unlock()
	c.logFlushMu.Lock()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	if err := c.Shutdown(ctx2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	c.logFlushMu.Unlock()
}
func TestCampaignSpanReservationAndFreshRetryBudget(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	c := campaignClient(t, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(503)
		} else {
			w.Write([]byte(`{}`))
		}
	})
	_, huge := c.StartSpan(context.Background(), "huge")
	huge.SetAttribute("blob", strings.Repeat("x", 11<<20))
	huge.End()
	if c.Stats().QueuedSpans != 0 || c.Stats().Dropped["spans_entry_oversize"] != 1 {
		t.Fatal(c.Stats())
	}
	for batch := 0; batch < 2; batch++ {
		_, s := c.StartSpan(context.Background(), "span")
		s.End()
		_ = c.flushSpans(context.Background())
		if c.Stats().QueuedSpans != 1 {
			t.Fatal("new batch inherited attempts", c.Stats())
		}
		_ = c.flushSpans(context.Background())
		if c.Stats().QueuedSpans != 0 {
			t.Fatal(c.Stats())
		}
	}
	fail.Store(false)
}

func TestCampaignOTLPPackingAndAdmissionDiagnostics(t *testing.T) {
	var bodiesMu sync.Mutex
	var sizes []int
	c := campaignClient(t, func(w http.ResponseWriter, r *http.Request) {
		var v any
		_ = json.NewDecoder(r.Body).Decode(&v)
		raw, _ := json.Marshal(v)
		bodiesMu.Lock()
		sizes = append(sizes, len(raw))
		bodiesMu.Unlock()
		w.Write([]byte(`{}`))
	})
	for i := 0; i < 50; i++ {
		c.Gauge("large").Set(float64(i), L("label", strings.Repeat("x", 10000)))
	}
	if err := c.FlushMetrics(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.Stats().DeliveredMetricPoints != 50 {
		t.Fatal(c.Stats())
	}
	bodiesMu.Lock()
	if len(sizes) < 2 {
		t.Fatal("expected packed requests", sizes)
	}
	for _, size := range sizes {
		if size > 1<<20 {
			t.Fatal("oversized request", size)
		}
	}
	bodiesMu.Unlock()
	c.Gauge("oversized").Set(1, L("blob", strings.Repeat("x", 1<<20)))
	if c.Stats().Dropped["metric_series_budget"] != 1 {
		t.Fatal(c.Stats())
	}
}

func TestCampaignSpanInflightReservationsStayCharged(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c := campaignClient(t, func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered); <-release })
		w.WriteHeader(503)
	})
	for i := 0; i < 7; i++ {
		_, span := c.StartSpan(context.Background(), "bounded")
		span.SetAttribute("blob", strings.Repeat("x", 900<<10))
		span.End()
	}
	before := c.Stats().QueuedSpans
	done := make(chan struct{})
	go func() { _ = c.flushSpans(context.Background()); close(done) }()
	<-entered
	if c.Stats().QueuedSpans != before {
		t.Fatal("in-flight spans disappeared from queue stats")
	}
	for i := 0; i < 20; i++ {
		_, span := c.StartSpan(context.Background(), "new")
		span.SetAttribute("blob", strings.Repeat("x", 900<<10))
		span.End()
	}
	c.mu.Lock()
	charged := c.pendingSpanBytes
	c.mu.Unlock()
	if charged > maxPendingSpanBytes {
		t.Fatal("reservation cap exceeded", charged)
	}
	close(release)
	<-done
	if c.Stats().Dropped["spans_queue_full"] == 0 {
		t.Fatal("expected charged overflow")
	}
}

func TestCampaignShutdownBypassesSignalBackoff(t *testing.T) {
	c := campaignClient(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"accepted":1}`)) })
	c.Info("log")
	_, span := c.StartSpan(context.Background(), "span")
	span.End()
	c.Gauge("point").Set(1)
	c.logAttempts = 2
	c.spanAttempts = 2
	c.metricAttempts = 2
	c.logNotBefore = time.Now().Add(time.Hour)
	c.spanNotBefore = c.logNotBefore
	c.metricNotBefore = c.logNotBefore
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	s := c.Stats()
	if s.DeliveredLogs != 1 || s.DeliveredSpans != 1 || s.DeliveredMetricPoints != 1 {
		t.Fatal(s)
	}
}

func TestCampaignActiveWorkerAndSecondCloserHonorOwnDeadline(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	var once sync.Once
	c := campaignClient(t, func(w http.ResponseWriter, r *http.Request) { once.Do(func() { close(entered) }); <-release })
	c.Info("worker")
	c.flushWake <- struct{}{}
	<-entered
	firstDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		firstDone <- c.Shutdown(ctx)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.Shutdown(ctx); err == nil {
		t.Fatal("expected incomplete drain")
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("deadline excluded active worker")
	}
	<-firstDone
}

func TestCampaignRetainedCounterAndFreshMetricShutdown(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	var bodiesMu sync.Mutex
	var bodies []string
	c := campaignClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodiesMu.Lock()
		bodies = append(bodies, string(raw))
		bodiesMu.Unlock()
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	})
	c.Counter("counter").Add(1)
	if c.FlushMetrics(context.Background()) == nil {
		t.Fatal("expected failure")
	}
	c.Gauge("tail").Set(2)
	c.Histogram("hist", 10).Observe(3)
	fail.Store(false)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	bodiesMu.Lock()
	defer bodiesMu.Unlock()
	if len(bodies) != 3 || bodies[0] != bodies[1] {
		t.Fatalf("retained head/tail: %v", bodies)
	}
	s := c.Stats()
	if s.DeliveredMetricPoints != 3 || s.QueuedMetricPoints != 0 || len(s.Dropped) != 0 {
		t.Fatal(s)
	}
}

func TestCampaignMetricOnlyShutdownFailureIsVisible(t *testing.T) {
	c := campaignClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
	c.Counter("counter").Add(1)
	if c.Close() == nil {
		t.Fatal("incomplete metric shutdown reported success")
	}
	s := c.Stats()
	if s.QueuedMetricPoints != 1 || s.Dropped["metrics_shutdown_unflushed"] != 1 {
		t.Fatal(s)
	}
}

func TestCampaignMetricTerminalAndAdmissionAccounting(t *testing.T) {
	for _, code := range []int{400, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			c := campaignClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) })
			c.Counter("counter").Add(1)
			c.Gauge("gauge").Set(2)
			c.Histogram("hist", 10).Observe(3)
			_ = c.FlushMetrics(context.Background())
			if code == 503 {
				_ = c.FlushMetrics(context.Background())
			}
			s := c.Stats()
			reason := "metrics_non_retryable"
			if code == 503 {
				reason = "metrics_retry_exhausted"
			}
			if s.Dropped[reason] != 3 || s.QueuedMetricPoints != 0 {
				t.Fatal(s)
			}
		})
	}
	c := campaignClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	for i := 0; i < maxMetricSeries+1; i++ {
		c.Counter(fmt.Sprint("counter", i)).Add(1)
	}
	if c.Stats().Dropped["metric_series_budget"] != 1 {
		t.Fatal(c.Stats())
	}
	// Exercise the independent point ceiling without a large allocation.
	c.metrics.mu.Lock()
	c.metrics.gauges = make([]gaugePoint, maxBufferedGaugePts)
	c.metrics.mu.Unlock()
	c.Gauge("counter0").Set(1)
	if c.Stats().Dropped["gauge_point_budget"] != 1 {
		t.Fatal(c.Stats())
	}
	c.metrics.mu.Lock()
	c.metrics.gauges = nil
	c.metrics.mu.Unlock()
}

func TestCampaignSpanRetryDoesNotAbsorbFreshTail(t *testing.T) {
	var bodies []string
	c := campaignClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		w.WriteHeader(503)
	})
	_, a := c.StartSpan(context.Background(), "head")
	a.End()
	_ = c.flushSpans(context.Background())
	_, b := c.StartSpan(context.Background(), "tail")
	b.End()
	_ = c.flushSpans(context.Background())
	if len(bodies) != 2 || bodies[0] != bodies[1] || c.Stats().QueuedSpans != 1 {
		t.Fatal(bodies, c.Stats())
	}
	_ = c.flushSpans(context.Background())
	if c.Stats().QueuedSpans != 1 {
		t.Fatal("tail did not receive its own budget", c.Stats())
	}
	_ = c.flushSpans(context.Background())
	if c.Stats().Dropped["spans_retry_exhausted"] != 2 {
		t.Fatal(c.Stats())
	}
}

func TestCampaignWorkerCancellationPreservesFinalSendBudget(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	c := campaignClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			return
		}
		w.WriteHeader(200)
	})
	c.maxSendAttempts = 1
	c.Gauge("point").Set(1)
	c.flushWake <- struct{}{}
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if c.Stats().DeliveredMetricPoints != 1 || len(c.Stats().Dropped) != 0 {
		t.Fatal(c.Stats())
	}
}

func TestCampaignHistogramLabelsNegativeOverflowAndEmptyGroup(t *testing.T) {
	var bodies []string
	c := campaignClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		w.WriteHeader(200)
	})
	labels := []Label{L("tenant", "original")}
	h := c.Histogram("hist", 10)
	h.Observe(-math.MaxFloat64, labels...)
	labels[0].Value = "changed"
	h.Observe(-math.MaxFloat64, L("tenant", "original"))
	if err := c.FlushMetrics(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bodies[0], "changed") || !strings.Contains(bodies[0], `"count":"1"`) {
		t.Fatal(bodies)
	}
	slog.New(c.NewSlogHandler(slog.LevelInfo, nil)).WithGroup("request").Info("group", slog.Group("", "id", "inline"))
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bodies[1], `"request.id":"inline"`) || strings.Contains(bodies[1], "request..id") {
		t.Fatal(bodies)
	}
}

func TestCampaignNativeBreadcrumbAggregateBudget(t *testing.T) {
	var body []byte
	c := campaignClient(t, func(w http.ResponseWriter, r *http.Request) { body, _ = io.ReadAll(r.Body); w.WriteHeader(200) })
	for i := 0; i < 100; i++ {
		c.AddBreadcrumb(Breadcrumb{Message: strings.Repeat("x", 256), Data: map[string]any{"k": strings.Repeat("x", 1000)}})
	}
	if err := c.CaptureException(errors.New("bounded breadcrumb error")); err != nil {
		t.Fatal(err)
	}
	var payload ErrorPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if len(body) >= 256<<10 || len(payload.Breadcrumbs) == 0 || len(payload.Breadcrumbs) >= 100 {
		t.Fatalf("bytes=%d crumbs=%d", len(body), len(payload.Breadcrumbs))
	}
}
