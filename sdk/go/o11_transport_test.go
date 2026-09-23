package observe

// O11 adverse-transport tests. The fault modes mirror sdk/testing/README.md
// (the shared matrix implemented for Node in sdk/testing/faultserver.mjs
// and for Python in sdk/python/tests/faultserver.py): server 500s,
// non-retryable 4xx poison heads, partial per-record rejections, slow
// responses against shutdown deadlines, and the visible loss accounting.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// faultMode drives one o11 fault server. kind is one of:
// ok, status, failN, partial, slow, reset.
type faultMode struct {
	kind   string
	code   int
	n      int
	ack    string // JSON body for partial
	delay  time.Duration
}

type o11Server struct {
	srv  *httptest.Server
	mu   sync.Mutex
	mode faultMode
	// counts per path: total requests received
	posts map[string]int
}

func newO11Server(t *testing.T, mode faultMode) *o11Server {
	t.Helper()
	o := &o11Server{mode: mode, posts: map[string]int{}}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		o.posts[r.URL.Path]++
		posts := o.posts[r.URL.Path]
		mode := o.mode
		o.mu.Unlock()
		switch mode.kind {
		case "ok":
			w.WriteHeader(200)
			fmt.Fprintf(w, `{"accepted":%d,"rejected":0}`, countLogEntries(r))
		case "status":
			w.WriteHeader(mode.code)
			fmt.Fprint(w, `{"error":"injected"}`)
		case "failN":
			if posts <= mode.n {
				w.WriteHeader(500)
				fmt.Fprint(w, `{"error":"injected 500"}`)
				return
			}
			w.WriteHeader(200)
			fmt.Fprintf(w, `{"accepted":%d,"rejected":0}`, countLogEntries(r))
		case "partial":
			w.WriteHeader(200)
			fmt.Fprint(w, mode.ack)
		case "slow":
			select {
			case <-time.After(mode.delay):
				w.WriteHeader(200)
				fmt.Fprint(w, `{"accepted":1,"rejected":0}`)
			case <-r.Context().Done():
			}
		case "reset":
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, _ := hj.Hijack()
				if conn != nil {
					_ = conn.Close()
				}
			}
		}
	}))
	t.Cleanup(o.srv.Close)
	return o
}

func (o *o11Server) setMode(m faultMode) {
	o.mu.Lock()
	o.mode = m
	o.mu.Unlock()
}

func (o *o11Server) postsFor(path string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.posts[path]
}

// drainLogs flushes until the queue empties or the loss counters settle,
// honoring the retry backoff with small sleeps.
func drainLogs(t *testing.T, c *Client, rounds int) {
	t.Helper()
	for i := 0; i < rounds; i++ {
		if c.Stats().QueuedLogs == 0 {
			return
		}
		_ = c.Flush(context.Background())
		time.Sleep(5 * time.Millisecond)
	}
}

func lossCount(s Stats, reason string) int64 {
	return s.Dropped[reason]
}

func TestO11Transient500sRecoverWithZeroLoss(t *testing.T) {
	o := newO11Server(t, faultMode{kind: "failN", n: 2})
	var errsMu sync.Mutex
	var errs []error
	c, err := New(Options{
		Endpoint:         o.srv.URL,
		MaxSendAttempts:  6,
		RetryBackoff:     time.Millisecond,
		LogFlushInterval: time.Hour,
		OnError:          func(e error) { errsMu.Lock(); errs = append(errs, e); errsMu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	c.Info("one")
	c.Info("two")
	drainLogs(t, c, 50)

	s := c.Stats()
	if s.DeliveredLogs != 2 {
		t.Fatalf("delivered %d, want 2 (stats %+v)", s.DeliveredLogs, s)
	}
	if s.Retries < 2 {
		t.Fatalf("retries %d, want >= 2", s.Retries)
	}
	if len(s.Dropped) != 0 || s.QueuedLogs != 0 {
		t.Fatalf("unexpected losses: %+v", s)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestO11RetryBudgetExhaustsIntoVisibleLoss(t *testing.T) {
	o := newO11Server(t, faultMode{kind: "status", code: 500})
	var errsMu sync.Mutex
	var errs []error
	c, err := New(Options{
		Endpoint:         o.srv.URL,
		MaxSendAttempts:  3,
		RetryBackoff:     time.Millisecond,
		LogFlushInterval: time.Hour,
		OnError:          func(e error) { errsMu.Lock(); errs = append(errs, e); errsMu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	c.Info("doomed-a")
	c.Info("doomed-b")

	// Ride the budget to exhaustion (3 attempts), then past it.
	for i := 0; i < 40; i++ {
		_ = c.Flush(context.Background())
		if lossCount(c.Stats(), "logs_retry_exhausted") > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	s := c.Stats()
	if got := lossCount(s, "logs_retry_exhausted"); got != 2 {
		t.Fatalf("retry_exhausted = %d, want 2 (stats %+v)", got, s)
	}
	if s.QueuedLogs != 0 {
		t.Fatalf("queue must drain after exhaustion, got %d queued", s.QueuedLogs)
	}

	// The dropped chunk must not poison the client: new entries deliver.
	o.setMode(faultMode{kind: "ok"})
	c.Info("after-recovery")
	drainLogs(t, c, 20)
	s = c.Stats()
	if s.DeliveredLogs != 1 {
		t.Fatalf("recovery delivery = %d, want 1 (stats %+v)", s.DeliveredLogs, s)
	}
	errsMu.Lock()
	defer errsMu.Unlock()
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "logs_retry_exhausted") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the loss must be reported through OnError, got %v", errs)
	}
}

func TestO11NonRetryable4xxDropsPoisonHeadAndDrains(t *testing.T) {
	var mu sync.Mutex
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o := countLogEntries(r)
		mu.Lock()
		call++
		n := call
		mu.Unlock()
		if n == 1 {
			// The first chunk is permanently refused.
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"bad shape"}`)
			return
		}
		w.WriteHeader(200)
		fmt.Fprintf(w, `{"accepted":%d,"rejected":0}`, o)
	}))
	defer srv.Close()

	c, err := New(Options{
		Endpoint:         srv.URL,
		MaxSendAttempts:  6,
		RetryBackoff:     time.Millisecond,
		LogFlushInterval: time.Hour,
		LogBatchSize:     100, // manual flushes only; one entry per chunk below
	})
	if err != nil {
		t.Fatal(err)
	}
	// First chunk: permanently refused — dropped with a counted loss
	// instead of blocking the queue head forever.
	c.Info("poison")
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("flush past the 400 should clear, got %v", err)
	}
	if got := lossCount(c.Stats(), "logs_non_retryable"); got != 1 {
		t.Fatalf("non_retryable = %d, want 1 (stats %+v)", got, c.Stats())
	}
	// Second chunk: the survivor delivers — no stall, no silent loss.
	c.Info("survivor")
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("survivor flush: %v", err)
	}
	s := c.Stats()
	if got := lossCount(s, "logs_non_retryable"); got != 1 {
		t.Fatalf("non_retryable grew to %d, want 1 (stats %+v)", got, s)
	}
	if s.DeliveredLogs != 1 {
		t.Fatalf("survivor lost behind the poison: %+v", s)
	}
	if s.QueuedLogs != 0 {
		t.Fatalf("queue stalled on a poison head: %+v", s)
	}
	_ = c.Close()
}

func TestO11PartialRejectionCountsPerRecordOutcomes(t *testing.T) {
	o := newO11Server(t, faultMode{kind: "partial", ack: `{"accepted":1,"rejected":1}`})
	c, err := New(Options{
		Endpoint:         o.srv.URL,
		LogFlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.Info("good")
	c.Info("bad")
	drainLogs(t, c, 10)
	time.Sleep(20 * time.Millisecond) // a resend would appear as a 2nd POST

	s := c.Stats()
	if got := o.postsFor("/api/v1/logs/batch"); got != 1 {
		t.Fatalf("partially accepted batch resent %d times, want 1", got)
	}
	if got := lossCount(s, "logs_server_rejected"); got != 1 {
		t.Fatalf("server_rejected = %d, want 1 (stats %+v)", got, s)
	}
	if s.DeliveredLogs != 1 {
		t.Fatalf("delivered = %d, want 1", s.DeliveredLogs)
	}
	_ = c.Close()
}

func TestO11ShutdownDeadlineCountsUnflushedAsLosses(t *testing.T) {
	o := newO11Server(t, faultMode{kind: "slow", delay: 5 * time.Second})
	var errsMu sync.Mutex
	var errs []string
	c, err := New(Options{
		Endpoint:         o.srv.URL,
		LogFlushInterval: time.Hour,
		OnError:          func(e error) { errsMu.Lock(); errs = append(errs, e.Error()); errsMu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	c.Info("stuck-1")
	c.Info("stuck-2")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := c.Shutdown(ctx); err == nil {
		t.Fatal("Shutdown must surface the expired deadline")
	}

	s := c.Stats()
	if got := lossCount(s, "logs_shutdown_unflushed"); got != 2 {
		t.Fatalf("shutdown_unflushed = %d, want 2 (stats %+v)", got, s)
	}
	errsMu.Lock()
	defer errsMu.Unlock()
	if !anyContains(errs, "logs_shutdown_unflushed") || !anyContains(errs, "shutdown loss summary") {
		t.Fatalf("shutdown losses must be reported, got %v", errs)
	}
}

func TestO11SpanBudgetExhaustsIntoVisibleLoss(t *testing.T) {
	o := newO11Server(t, faultMode{kind: "status", code: 500})
	c, err := New(Options{
		Endpoint:         o.srv.URL,
		ServiceName:      "o11",
		MaxSendAttempts:  2,
		RetryBackoff:     time.Millisecond,
		LogFlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, s1 := c.StartSpan(context.Background(), "one")
	s1.End()
	_, s2 := c.StartSpan(context.Background(), "two")
	s2.End()

	for i := 0; i < 20; i++ {
		_ = c.flushSpans(context.Background())
		if lossCount(c.Stats(), "spans_retry_exhausted") > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	s := c.Stats()
	if got := lossCount(s, "spans_retry_exhausted"); got != 2 {
		t.Fatalf("spans_retry_exhausted = %d, want 2 (stats %+v)", got, s)
	}
	if s.QueuedSpans != 0 {
		t.Fatalf("span queue must drop the exhausted batch: %+v", s)
	}
	_ = c.Close()
}

func TestO11StatsAccountingIdentity(t *testing.T) {
	o := newO11Server(t, faultMode{kind: "ok"})
	c, err := New(Options{
		Endpoint:         o.srv.URL,
		LogFlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	const admitted = 10
	for i := 0; i < admitted; i++ {
		c.Info(fmt.Sprintf("entry-%d", i))
	}
	drainLogs(t, c, 20)
	_ = c.Close()

	s := c.Stats()
	var lost int64
	for _, v := range s.Dropped {
		lost += v
	}
	if s.DeliveredLogs+lost != admitted {
		t.Fatalf("delivered(%d) + dropped(%d) != admitted(%d): %+v", s.DeliveredLogs, lost, admitted, s)
	}
}

func TestO11CleanShutdownReportsNoLosses(t *testing.T) {
	o := newO11Server(t, faultMode{kind: "ok"})
	var errsMu sync.Mutex
	var errs []string
	c, err := New(Options{
		Endpoint:         o.srv.URL,
		LogFlushInterval: time.Hour,
		OnError:          func(e error) { errsMu.Lock(); errs = append(errs, e.Error()); errsMu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	c.Info("clean")
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	errsMu.Lock()
	defer errsMu.Unlock()
	for _, e := range errs {
		if strings.Contains(e, "loss summary") {
			t.Fatalf("clean shutdown must not report losses, got %q", e)
		}
	}
	if lossCount(c.Stats(), "logs_shutdown_unflushed") != 0 {
		t.Fatalf("clean shutdown counted unflushed losses: %+v", c.Stats())
	}
}

func anyContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// countLogEntries reports how many entries the client packed into this
// /logs/batch request, so the ok-mode ack echoes a truthful accepted count.
func countLogEntries(r *http.Request) int {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return 1
	}
	var wire struct {
		Logs []json.RawMessage `json:"logs"`
	}
	if err := json.Unmarshal(body, &wire); err != nil || len(wire.Logs) == 0 {
		return 1
	}
	return len(wire.Logs)
}
