package persons

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWriteLimiterWindowAndIsolation(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewWriteLimiter(2, time.Minute)
	l.now = func() time.Time { return now }
	a1, a2, a3 := l.Allow("s", "a"), l.Allow("s", "a"), l.Allow("s", "a")
	if !a1 || !a2 || a3 {
		t.Fatal("limit of 2 not enforced")
	}
	if !l.Allow("s", "b") || !l.Allow("t", "a") {
		t.Fatal("other person / other site must be independent")
	}
	now = now.Add(61 * time.Second)
	if !l.Allow("s", "a") {
		t.Fatal("window did not reset")
	}
}

func TestWriteLimiterBounded(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewWriteLimiter(1, time.Minute)
	l.max = 2
	l.now = func() time.Time { return now }
	l.Allow("s", "a")
	l.Allow("s", "b")
	if l.Allow("s", "c") {
		t.Fatal("full table must refuse new keys")
	}
	now = now.Add(2 * time.Minute)
	if !l.Allow("s", "c") {
		t.Fatal("expired entries must be swept to make room")
	}
	if len(l.m) > 2 {
		t.Fatalf("table grew past max: %d", len(l.m))
	}
}

func TestSetKnownPropertiesRequiresExistingAndReturnsNoValues(t *testing.T) {
	m := NewMemory()
	s := m.Service()
	ctx := context.Background()
	if _, err := s.SetKnownProperties(ctx, "s1", "ghost", map[string]any{"a": 1.0}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown person: %v", err)
	}
	if _, found, _ := m.GetProps(ctx, "s1", "ghost"); found {
		t.Fatal("row created for unknown person")
	}
	m.AddPerson("s1", Person{DistinctID: "p1", EventCount: 1})
	names, err := s.SetKnownProperties(ctx, "s1", "p1", map[string]any{"b": 1.0, "a": "x"})
	if err != nil || len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("names=%v err=%v", names, err)
	}
	// Site isolation: p1 exists in s1 only.
	if _, err := s.SetKnownProperties(ctx, "s2", "p1", map[string]any{"a": 1.0}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-site: %v", err)
	}
}
