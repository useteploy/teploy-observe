package aiquery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGenerateRejectsBeforeConfigOrProvider(t *testing.T) {
	// A nil database would panic if input/admission were deferred to a route.
	svc := NewService(nil, nil)
	for _, tc := range []struct {
		ctx  context.Context
		q    string
		want error
	}{
		{context.Background(), "question", ErrPrincipal},
		{WithPrincipal(context.Background(), "user:oversize"), strings.Repeat("x", 4001), ErrQuestion},
		{WithPrincipal(context.Background(), "mcp:oversize"), "  ", ErrQuestion},
	} {
		_, err := svc.Generate(tc.ctx, tc.q, "schema")
		if !errors.Is(err, tc.want) {
			t.Fatalf("got %v, want %v", err, tc.want)
		}
	}
}

func TestSharedGenerationCapacity(t *testing.T) {
	mcpPrincipal := freshPrincipal("mcp", t)
	var releases []func()
	for i := 0; i < 4; i++ {
		release, err := admit(WithPrincipal(context.Background(), freshPrincipal("user", t)), "question")
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	// This is a different transport and Service, still sharing the global slots.
	_, err := NewService(nil, nil).Generate(WithPrincipal(context.Background(), mcpPrincipal), "question", "schema")
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("fifth call reached config/provider: %v", err)
	}
	releases[0]()
	releases = releases[1:]
	release, err := admit(WithPrincipal(context.Background(), mcpPrincipal), "question")
	if err != nil {
		t.Fatal(err)
	}
	release()
}
func TestPrincipalRateAndRefill(t *testing.T) {
	l := principalLimiter{buckets: map[string]principalBucket{}}
	now := time.Now()
	for i := 0; i < 10; i++ {
		if !l.allow("user:a", now) {
			t.Fatalf("refused burst %d", i)
		}
	}
	if l.allow("user:a", now.Add(59*time.Second)) {
		t.Fatal("early refill")
	}
	if !l.allow("mcp:a", now) {
		t.Fatal("credential namespace collision")
	}
	if !l.allow("user:a", now.Add(time.Minute)) {
		t.Fatal("no minute refill")
	}
}
func TestGenerateRateLimitAndCancellation(t *testing.T) {
	ctx := WithPrincipal(context.Background(), freshPrincipal("mcp", t))
	for i := 0; i < 10; i++ {
		release, err := admit(ctx, "question")
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	_, err := NewService(nil, nil).Generate(ctx, "question", "schema")
	if !errors.Is(err, ErrRateLimit) {
		t.Fatalf("rate failed: %v", err)
	}
	canceled, cancel := context.WithCancel(WithPrincipal(context.Background(), "user:cancel"))
	cancel()
	_, err = NewService(nil, nil).Generate(canceled, "question", "schema")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation failed: %v", err)
	}
}

var principalTestSequence atomic.Int64

func freshPrincipal(namespace string, t *testing.T) string {
	return fmt.Sprintf("%s:%s:%d", namespace, t.Name(), principalTestSequence.Add(1))
}
