package authguard

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/useteploy/teploy-observe/internal/auth"
)

type fakeInner struct {
	calls atomic.Int64
	good  map[string]auth.ValidatedKey
	err   error
}

func (f *fakeInner) ValidateAPIKey(_ context.Context, key string) (auth.ValidatedKey, error) {
	f.calls.Add(1)
	if f.err != nil {
		return auth.ValidatedKey{}, f.err
	}
	if v, ok := f.good[key]; ok {
		return v, nil
	}
	return auth.ValidatedKey{}, errors.New("auth: invalid api key")
}

func TestNegativeCacheCollapsesRepeatedBadKeys(t *testing.T) {
	in := &fakeInner{good: map[string]auth.ValidatedKey{"good": {SiteID: "s"}}}
	now := time.Unix(1000, 0)
	g := New(in, Config{FailuresPerMinute: -1, Now: func() time.Time { return now }})
	for i := 0; i < 100; i++ {
		if _, err := g.Validate(context.Background(), "bad", "1.1.1.1"); err == nil {
			t.Fatal("bad key accepted")
		}
	}
	if in.calls.Load() != 1 {
		t.Fatalf("store hit %d times for one bad key, want 1", in.calls.Load())
	}
	now = now.Add(31 * time.Second)
	_, _ = g.Validate(context.Background(), "bad", "1.1.1.1")
	if in.calls.Load() != 2 {
		t.Fatalf("expired entry must re-check the store, calls=%d", in.calls.Load())
	}
}

func TestSuccessesAreNeverCached(t *testing.T) {
	in := &fakeInner{good: map[string]auth.ValidatedKey{"good": {SiteID: "s"}}}
	g := New(in, Config{})
	for i := 0; i < 5; i++ {
		if v, err := g.Validate(context.Background(), "good", "2.2.2.2"); err != nil || v.SiteID != "s" {
			t.Fatalf("good key: %v %v", v, err)
		}
	}
	if in.calls.Load() != 5 {
		t.Fatalf("successes must hit the store every time, calls=%d", in.calls.Load())
	}
	// Revocation takes effect at once: the key now fails and is then cached.
	delete(in.good, "good")
	if _, err := g.Validate(context.Background(), "good", "2.2.2.2"); err == nil {
		t.Fatal("revoked key accepted")
	}
}

func TestOutageIsNotCachedOrCounted(t *testing.T) {
	in := &fakeInner{err: fmt.Errorf("%w: down", auth.ErrAuthUnavailable)}
	g := New(in, Config{FailuresPerMinute: 3})
	for i := 0; i < 20; i++ {
		_, err := g.Validate(context.Background(), "k", "3.3.3.3")
		if !errors.Is(err, auth.ErrAuthUnavailable) {
			t.Fatalf("got %v", err)
		}
	}
	if in.calls.Load() != 20 {
		t.Fatalf("outage cached: calls=%d", in.calls.Load())
	}
}

func TestPerIPFailureLimiterBlocksBeforeStore(t *testing.T) {
	in := &fakeInner{good: map[string]auth.ValidatedKey{"good": {SiteID: "s"}}}
	now := time.Unix(5000, 0)
	g := New(in, Config{FailuresPerMinute: 60, Now: func() time.Time { return now }})
	for i := 0; i < 60; i++ {
		_, _ = g.Validate(context.Background(), fmt.Sprintf("random-%d", i), "9.9.9.9")
	}
	before := in.calls.Load()
	if before != 60 {
		t.Fatalf("calls=%d", before)
	}
	for i := 0; i < 50; i++ {
		if _, err := g.Validate(context.Background(), fmt.Sprintf("more-%d", i), "9.9.9.9"); !errors.Is(err, ErrTooManyAttempts) {
			t.Fatalf("expected ErrTooManyAttempts, got %v", err)
		}
	}
	if in.calls.Load() != before {
		t.Fatal("blocked IP still reached the store")
	}
	// Another IP is unaffected, and the window resets.
	if _, err := g.Validate(context.Background(), "good", "8.8.8.8"); err != nil {
		t.Fatalf("other IP: %v", err)
	}
	now = now.Add(61 * time.Second)
	if _, err := g.Validate(context.Background(), "good", "9.9.9.9"); err != nil {
		t.Fatalf("window reset: %v", err)
	}
	if g.Stats().IPBlocked != 50 {
		t.Fatalf("stats: %+v", g.Stats())
	}
}

func TestCachesAreBounded(t *testing.T) {
	in := &fakeInner{}
	g := New(in, Config{NegativeMax: 100, TrackedIPs: 50, FailuresPerMinute: 1000})
	for i := 0; i < 5000; i++ {
		_, _ = g.Validate(context.Background(), fmt.Sprintf("k%d", i), fmt.Sprintf("10.0.%d.%d", i/250, i%250))
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.neg) > 100 || len(g.ips) > 50 {
		t.Fatalf("unbounded: neg=%d ips=%d", len(g.neg), len(g.ips))
	}
}
