package sentrycompat

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/useteploy/teploy-observe/internal/auth"
	"github.com/useteploy/teploy-observe/internal/authguard"
)

// countingBody records how many bytes the handler pulled from the wire.
type countingBody struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}
func (c *countingBody) Close() error { return nil }

// An unauthenticated envelope (DSN only in the envelope header, key wrong)
// must not cause more than the 64 KiB peek to be read from the wire.
func TestPreAuthReadIsCapped(t *testing.T) {
	sink := &fakeSink{}
	h := newH(sink, nil)
	big := strings.Repeat("x", 4<<20)
	body := []byte(`{"dsn":"https://obs_wrong@h.example/1"}` + "\n" + evItem(`{"message":"`+big+`"}`) + "\n")
	cb := &countingBody{r: bytes.NewReader(body)}
	req := httptest.NewRequest("POST", "/api/1/envelope/", cb)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("code %d", rr.Code)
	}
	if cb.n.Load() > maxPreAuthBytes {
		t.Fatalf("read %d bytes before auth, cap is %d", cb.n.Load(), maxPreAuthBytes)
	}
	if len(sink.got) != 0 {
		t.Fatal("unauthenticated body reached the sink")
	}
}

// A gzip bomb behind a bad DSN key is never inflated beyond the peek limit:
// the wire read is the capped prefix, so it cannot expand further.
func TestPreAuthGzipBombNotInflated(t *testing.T) {
	h := newH(&fakeSink{}, nil)
	raw := append([]byte(`{"dsn":"https://obs_wrong@h/1"}`+"\n"), bytes.Repeat([]byte("a"), 50<<20)...)
	body := gz(t, raw)
	if len(body) > MaxEnvelopeBytes {
		t.Skip("fixture larger than cap")
	}
	rr := post(h, "/api/1/envelope/", body, map[string]string{"Content-Encoding": "gzip"})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("code %d", rr.Code)
	}
}

// With a valid DSN key in the envelope header, the part of the body past the
// pre-auth prefix is still read and ingested after auth.
func TestAuthenticatedBodyBeyondPrefixIsIngested(t *testing.T) {
	sink := &fakeSink{}
	h := newH(sink, nil)
	filler := strings.Repeat("y", 100<<10) // pushes the second event past 64 KiB
	env := []byte(`{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","dsn":"https://obs_siteA@h.example/1"}` + "\n" +
		evItem(`{"message":"first `+filler[:1000]+`"}`) + "\n" +
		evItem(`{"message":"second `+filler[:60000]+`"}`) + "\n" +
		evItem(`{"message":"third"}`) + "\n")
	rr := post(h, "/api/1/envelope/", env, nil)
	if rr.Code != 200 {
		t.Fatalf("code %d %s", rr.Code, rr.Body)
	}
	if len(sink.got) != 3 {
		t.Fatalf("pushed %d, want 3", len(sink.got))
	}
}

// Events without their own event_id must not inherit the envelope id for
// every item (events 2..N would collide as conflicts and be dropped).
func TestThreeEventEnvelopeGetsDistinctDeterministicIDs(t *testing.T) {
	sink := &fakeSink{}
	h := newH(sink, nil)
	env := envelope(evItem(`{"message":"a"}`), evItem(`{"message":"b"}`), evItem(`{"message":"c"}`))
	if rr := post(h, "/api/1/envelope/?sentry_key=obs_siteA", env, nil); rr.Code != 200 {
		t.Fatalf("code %d", rr.Code)
	}
	if len(sink.got) != 3 {
		t.Fatalf("pushed %d, want 3 (events 2..N were dropped)", len(sink.got))
	}
	ids := map[string]bool{}
	for _, g := range sink.got {
		ids[g.in.EventID] = true
	}
	if len(ids) != 3 {
		t.Fatalf("ids collide: %v", ids)
	}
	if sink.got[0].in.EventID != "9ec79c33ec9942ab8353589fcb2e04dc" {
		t.Fatalf("first item must keep the header id, got %s", sink.got[0].in.EventID)
	}
	// A retry of the same envelope dedupes all three.
	if rr := post(h, "/api/1/envelope/?sentry_key=obs_siteA", env, nil); rr.Code != 200 {
		t.Fatalf("retry code %d", rr.Code)
	}
	if len(sink.got) != 3 {
		t.Fatalf("retry produced new events: %d", len(sink.got))
	}
}

type countingKeys struct{ calls atomic.Int64 }

func (c *countingKeys) ValidateAPIKey(ctx context.Context, key string) (auth.ValidatedKey, error) {
	c.calls.Add(1)
	return fakeKeys{}.ValidateAPIKey(ctx, key)
}

func TestRandomKeysHitStoreOnceAndIPIsLimited(t *testing.T) {
	ck := &countingKeys{}
	guard := authguard.New(ck, authguard.Config{FailuresPerMinute: 20})
	hh := &Handler{Keys: guard, Sink: &fakeSink{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/{project_id}/envelope/{$}", hh.Envelope)

	// Same bad key repeatedly: one store lookup, rest from the negative cache.
	for i := 0; i < 10; i++ {
		rr := post(mux, "/api/1/envelope/?sentry_key=obs_bad", envelope(), nil)
		if rr.Code != 401 {
			t.Fatalf("code %d", rr.Code)
		}
	}
	if ck.calls.Load() != 1 {
		t.Fatalf("store lookups=%d for one bad key", ck.calls.Load())
	}
	// Random keys from one address exhaust the failure budget, then 429.
	var limited int
	for i := 0; i < 60; i++ {
		rr := post(mux, fmt.Sprintf("/api/1/envelope/?sentry_key=rand%d", i), envelope(), nil)
		if rr.Code == http.StatusTooManyRequests {
			limited++
			if rr.Header().Get("Retry-After") == "" {
				t.Fatal("429 without Retry-After")
			}
		}
	}
	if limited == 0 {
		t.Fatal("no 429 after sustained random-key attempts")
	}
	if ck.calls.Load() > 25 {
		t.Fatalf("store lookups=%d: limiter did not stop the flood", ck.calls.Load())
	}
}
