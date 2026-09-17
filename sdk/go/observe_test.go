package observe

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
