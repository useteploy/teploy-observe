package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/neutron"
	"github.com/useteploy/teploy-observe/internal/ingest"
)

// stalledStore stands in for Nucleus during a stall: Insert blocks until its
// context ends or release closes, and only records a write if its context is
// still live when it gets to write.
type stalledStore struct {
	release chan struct{}
	writes  atomic.Int32
}

func (s *stalledStore) Insert(ctx context.Context) error {
	select {
	case <-ctx.Done():
	case <-s.release:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.writes.Add(1)
	return nil
}

// A Nucleus stall behind an ingest route must answer a CORS-readable 503 with
// Retry-After inside the deadline - not hold the connection until the
// listener's WriteTimeout kills it (which the tunnel reports as a CORS-less
// 502) - and nothing the handler does afterwards may reach the client or the
// store.
func TestIngestDeadlineStalledStoreAnswers503WithCORS(t *testing.T) {
	const deadline = 200 * time.Millisecond
	store := &stalledStore{release: make(chan struct{})}
	lateWrite := make(chan error, 1)

	app := neutron.New(neutron.WithMiddleware(queryRefusalMetadata, securityHeadersMiddleware(true)))
	r := app.Router()
	g := r.Group("/api/v1", ingestMiddleware(fakeSiteKey, ingest.NewRateLimiter(1000, time.Second, 1000), deadline)...)
	g.HandleFunc("POST /replays", func(w http.ResponseWriter, r *http.Request) {
		err := store.Insert(r.Context())
		if err == nil {
			w.WriteHeader(http.StatusAccepted)
		}
		_, werr := w.Write([]byte(`{"ok":true}`))
		lateWrite <- werr
	})
	h := ingestOnly(app.Handler())

	rec := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rec, corsPost("/api/v1/replays", "k", `{}`))
	elapsed := time.Since(start)

	if elapsed > deadline+time.Second {
		t.Fatalf("stalled ingest answered after %v, want ~%v", elapsed, deadline)
	}
	assertCORS(t, "stalled replays", rec, http.StatusServiceUnavailable)
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("503 from the deadline has no Retry-After")
	}
	body := rec.Body.String()

	close(store.release)
	select {
	case err := <-lateWrite:
		if !errors.Is(err, http.ErrHandlerTimeout) {
			t.Fatalf("handler write after the deadline returned %v, want ErrHandlerTimeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler never finished after the deadline")
	}
	if got := rec.Body.String(); got != body {
		t.Fatalf("response changed after the deadline: %q -> %q", body, got)
	}
	if n := store.writes.Load(); n != 0 {
		t.Fatalf("store recorded %d writes after the deadline", n)
	}
}

// Within the deadline the handler's response passes through unchanged.
func TestIngestDeadlineFastPathUnchanged(t *testing.T) {
	app := neutron.New()
	r := app.Router()
	g := r.Group("/api/v1", ingestMiddleware(fakeSiteKey, ingest.NewRateLimiter(1000, time.Second, 1000), time.Second)...)
	g.HandleFunc("POST /replays", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "1")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	rec := httptest.NewRecorder()
	ingestOnly(app.Handler()).ServeHTTP(rec, corsPost("/api/v1/replays", "k", `{}`))
	assertCORS(t, "fast replays", rec, http.StatusAccepted)
	if rec.Header().Get("X-Test") != "1" || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("fast path altered response: headers %v body %q", rec.Header(), rec.Body.String())
	}
	if rec.Header().Get("Retry-After") != "" {
		t.Fatal("fast path response carries Retry-After")
	}
}
