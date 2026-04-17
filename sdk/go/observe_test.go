package observe

import (
	"errors"
	"net/http"
	"net/http/httptest"
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
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/logs" {
			atomic.AddInt32(&count, 1)
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
	if got := atomic.LoadInt32(&count); got != 5 {
		t.Errorf("expected 5 log POSTs after flush, got %d", got)
	}
}

func TestAutoFlushAtBatchSize(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/logs" {
			atomic.AddInt32(&count, 1)
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
	// Auto-flush runs in a goroutine — give it a moment.
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&count); got < 3 {
		t.Errorf("expected >=3 flushed logs, got %d", got)
	}
}
