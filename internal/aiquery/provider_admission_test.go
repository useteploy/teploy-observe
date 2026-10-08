package aiquery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestGenerateSharesProviderBudgetAcrossServices(t *testing.T) {
	mcpPrincipal := freshPrincipal("mcp", t)
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"SELECT 1"}}]}`)
	}))
	defer provider.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	services := []*Service{NewService(nil, slog.Default()), NewService(nil, slog.Default())}
	for _, svc := range services {
		svc.generateConfig = func(context.Context) (Config, error) {
			return Config{APIKey: "synthetic", Endpoint: provider.URL, Model: "fixture"}, nil
		}
	}
	results := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func(i int) {
			_, err := services[i%2].Generate(WithPrincipal(ctx, freshPrincipal("user", t)), "question", "schema")
			results <- err
		}(i)
	}
	for i := 0; i < 4; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			close(release)
			t.Fatal("provider requests did not start")
		}
	}
	_, err := services[1].Generate(WithPrincipal(ctx, mcpPrincipal), "question", "schema")
	if !errors.Is(err, ErrCapacity) || calls.Load() != 4 {
		close(release)
		t.Fatalf("excess provider work: calls=%d err=%v", calls.Load(), err)
	}
	close(release)
	for i := 0; i < 4; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	_, err = services[1].Generate(WithPrincipal(ctx, mcpPrincipal), "question", "schema")
	if err != nil || calls.Load() != 5 {
		t.Fatalf("slot not released: calls=%d err=%v", calls.Load(), err)
	}
}
func TestGenerateRateLimitStopsProvider(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"SELECT 1"}}]}`)
	}))
	defer provider.Close()
	svc := NewService(nil, slog.Default())
	svc.generateConfig = func(context.Context) (Config, error) {
		return Config{APIKey: "synthetic", Endpoint: provider.URL, Model: "fixture"}, nil
	}
	ctx := WithPrincipal(context.Background(), freshPrincipal("mcp", t))
	for i := 0; i < 10; i++ {
		if _, err := svc.Generate(ctx, "question", "schema"); err != nil {
			t.Fatal(err)
		}
	}
	_, err := svc.Generate(ctx, "question", "schema")
	if !errors.Is(err, ErrRateLimit) || calls.Load() != 10 {
		t.Fatalf("eleventh provider call: %d %v", calls.Load(), err)
	}
}
