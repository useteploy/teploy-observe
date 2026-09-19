package observe

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCaptureException(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/errors" {
			t.Errorf("expected /api/v1/errors, got %s", r.URL.Path)
		}
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Errorf("expected X-API-Key header, got %s", r.Header.Get("X-API-Key"))
		}
		atomic.AddInt32(&count, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c, err := New(Options{Endpoint: srv.URL, APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.CaptureException(errors.New("boom"), WithRelease("v1")); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&count) != 1 {
		t.Errorf("expected 1 error POST, got %d", count)
	}
}

func TestLogBatchingFlushesOnClose(t *testing.T) {
	var entries int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/logs/batch" {
			var body struct {
				Logs []LogEntry `json:"logs"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			atomic.AddInt32(&entries, int32(len(body.Logs)))
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c, err := New(Options{
		Endpoint:         srv.URL,
		APIKey:           "k",
		LogBatchSize:     10,
		LogFlushInterval: time.Hour, // rely on Close() to trigger flush
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		c.Info("hello", F("i", i))
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&entries); got != 5 {
		t.Errorf("expected all 5 logs in the close-time batch, got %d", got)
	}
}

func TestAutoFlushAtBatchSize(t *testing.T) {
	var entries int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/logs/batch" {
			var body struct {
				Logs []LogEntry `json:"logs"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			atomic.AddInt32(&entries, int32(len(body.Logs)))
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c, err := New(Options{
		Endpoint:         srv.URL,
		LogBatchSize:     3,
		LogFlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	for i := 0; i < 3; i++ {
		c.Warn("warn", F("n", i))
	}
	// Size-triggered flush runs on the owned worker — give it a moment.
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&entries); got < 3 {
		t.Errorf("expected >=3 flushed logs, got %d", got)
	}
}

// TestConcurrentCloseIsSafe is the audit F35 regression: the old
// select/default + close(ch) let two concurrent closers both take the
// default branch and the second close panicked.
func TestConcurrentCloseIsSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c, err := New(Options{Endpoint: srv.URL, LogFlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

// AUD-038 (round 2): a failed flush leaves the queue intact — the old code
// detached everything up front and dropped failed chunks, so Close later
// saw an empty queue and reported success over lost logs.
func TestFlushFailureRetainsQueue(t *testing.T) {
	var fail = true
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		f := fail
		mu.Unlock()
		if f {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c, err := New(Options{Endpoint: srv.URL, LogBatchSize: 100, LogFlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	c.Info("kept")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := c.Flush(ctx); err == nil {
		t.Fatal("flush against a 503 endpoint must fail")
	}
	cancel()
	c.mu.Lock()
	queued := c.logsN
	c.mu.Unlock()
	if queued != 1 {
		t.Fatalf("failed flush must retain the queue, got %d queued", queued)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := c.Flush(ctx2); err != nil {
		t.Fatalf("retry after recovery must succeed: %v", err)
	}
	_ = c.Close()
}

// AUD-036 (round 2): entries are frozen at admission — caller mutation
// after log() cannot change the delivered body.
func TestQueuedEntriesAreImmutableSnapshots(t *testing.T) {
	var got map[string]any
	var bodyMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/logs/batch" {
			var body struct {
				Logs []map[string]any `json:"logs"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			bodyMu.Lock()
			if len(body.Logs) > 0 {
				got = body.Logs[0]
			}
			bodyMu.Unlock()
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c, err := New(Options{Endpoint: srv.URL, LogBatchSize: 100, LogFlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	attrs := map[string]any{"k": "original"}
	c.Info("m", Field{Key: "attrs", Value: attrs})
	attrs["k"] = "mutated"
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	bodyMu.Lock()
	defer bodyMu.Unlock()
	inner, _ := got["attributes"].(map[string]any)
	nested, _ := inner["attrs"].(map[string]any)
	if nested["k"] != "original" {
		t.Fatalf("delivered entry must be the admission-time snapshot, got %v", nested)
	}
}

// AUD-038: admission is byte-bounded.
func TestLogQueueByteLimitDropsNewEntries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	var errs atomic.Int32
	c, err := New(Options{
		Endpoint: srv.URL, LogBatchSize: 1 << 30, LogFlushInterval: time.Hour,
		OnError: func(error) { errs.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	big := strings.Repeat("x", 64*1024-128)
	for i := 0; i < 200; i++ {
		c.Info(big) // each ~64 KiB; the 8 MiB cap rejects well before 200
	}
	if errs.Load() == 0 {
		t.Fatal("byte-bounded admission must report drops")
	}
}

// TO-037: telemetry refuses redirects — a cross-origin 307 must never
// carry the API key to the redirect target, and a 3xx status is a failure,
// not a silent success.
func TestRedirectNeverForwardsAPIKey(t *testing.T) {
	keySeen := make(chan string, 4)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keySeen <- r.Header.Get("X-API-Key")
		w.WriteHeader(204)
	}))
	defer sink.Close()
	target := strings.Replace(sink.URL, "127.0.0.1", "localhost", 1)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	c, err := New(Options{Endpoint: source.URL, APIKey: "REPRO_KEY", LogFlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.CaptureException(errors.New("x")); err == nil {
		t.Fatal("a redirected request must fail, not report success")
	}
	select {
	case k := <-keySeen:
		t.Fatalf("the key must never reach the redirect target, got %q", k)
	default:
	}
}

// TO-038: a failed span export retains its batch and Close surfaces the
// failure; spans admitted after Close are refused with a report.
func TestSpanExportRetainedAndPostCloseRefused(t *testing.T) {
	var errs []error
	var fail atomic.Bool
	fail.Store(true)
	var spanPosts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/traces" {
			spanPosts.Add(1)
			if fail.Load() {
				w.WriteHeader(503)
				return
			}
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c, _ := New(Options{
		Endpoint:         srv.URL,
		ServiceName:      "t038",
		LogFlushInterval: time.Hour,
		OnError:          func(e error) { errs = append(errs, e) },
	})
	_, span := c.StartSpan(context.Background(), "work")
	span.SetAttribute("k", "v")
	span.End()
	if err := c.flushSpans(context.Background()); err == nil {
		t.Fatal("the first export must fail against 503")
	}
	if spanPosts.Load() != 1 {
		t.Fatalf("expected exactly one attempt, got %d", spanPosts.Load())
	}
	// The batch survived: retry succeeds and ships it.
	fail.Store(false)
	if err := c.flushSpans(context.Background()); err != nil {
		t.Fatalf("retry must succeed: %v", err)
	}
	if spanPosts.Load() != 2 {
		t.Fatalf("the retained batch must have been re-sent, got %d posts", spanPosts.Load())
	}

	// Post-close admission is refused and reported.
	c.Close()
	_, late := c.StartSpan(context.Background(), "late")
	late.End()
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "after close") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the post-close span must be reported, got %v", errs)
	}
}

// TO-054: unsupported span attribute types are rejected with a diagnostic
// instead of silently encoding as an empty string; the supported scalar
// widening (float32, small/unsigned ints) round-trips.
func TestSpanAttributeNormalization(t *testing.T) {
	var errs []error
	c, _ := New(Options{Endpoint: "http://unused.example", OnError: func(e error) { errs = append(errs, e) }})
	defer c.Close()

	_, span := c.StartSpan(context.Background(), "attrs")
	span.SetAttribute("str", "s")
	span.SetAttribute("i", 7)
	span.SetAttribute("i8", int8(3))
	span.SetAttribute("u16", uint16(9))
	span.SetAttribute("f32", float32(1.5))
	span.SetAttribute("slice", []int{1})         // unsupported
	span.SetAttribute("overflow", uint64(1)<<63) // exceeds int64
	span.SetAttribute("nan", math.NaN())
	span.mu.Lock()
	defer span.mu.Unlock()
	vals := map[string]any{}
	for _, a := range span.attributes {
		vals[a.key] = a.value
	}
	if _, ok := vals["slice"]; ok {
		t.Fatal("an unsupported slice attribute must be rejected")
	}
	if _, ok := vals["overflow"]; ok {
		t.Fatal("an overflowing uint64 must be rejected")
	}
	if _, ok := vals["nan"]; ok {
		t.Fatal("NaN must be rejected")
	}
	if vals["f32"] != 1.5 || vals["i8"] != int64(3) || vals["u16"] != int64(9) {
		t.Fatalf("widened scalars must normalize: %#v", vals)
	}
	if len(errs) != 3 {
		t.Fatalf("each rejection must be reported, got %d: %v", len(errs), errs)
	}
}
