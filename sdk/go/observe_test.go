package observe

import (
	"context"
	"encoding/json"
	"errors"
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
