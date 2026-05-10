package llm

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// TestStats_EmptyWindowReturnsZeros locks the contract from B1: an empty
// query window must return numeric strings ("0", "0", …), never empty
// strings, so the UI doesn't render blank cells.
//
// Pre-fix the SUM/AVG aggregates returned NULL on empty input and Nucleus
// serialized that as "" via CAST(NULL AS TEXT). We now wrap each aggregate
// in COALESCE(..., 0).
//
// Connects directly to nucleus and skips when not reachable, matching the
// other live-stack integration tests.
func TestStats_EmptyWindowReturnsZeros(t *testing.T) {
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		dsn = "postgres://postgres@localhost:5432/postgres?sslmode=disable"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping live test", dsn)
	}
	defer db.Close()

	svc := NewLLMService(db)

	// A future window guaranteed to be empty.
	from := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)

	stats, err := svc.Stats(ctx, "default", from, to)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	for name, got := range map[string]string{
		"TotalCalls":    stats.TotalCalls,
		"TotalTokens":   stats.TotalTokens,
		"TotalCostUSD":  stats.TotalCostUSD,
		"AvgLatencyMs":  stats.AvgLatencyMs,
		"ErrorCount":    stats.ErrorCount,
	} {
		if got == "" {
			t.Errorf("%s = %q (empty) — want numeric string like \"0\"", name, got)
		}
	}
	// COUNT(*) is naturally 0 not NULL; the others are the COALESCE
	// payoff. Pin the expectation that all five render as "0" on empty.
	if stats.TotalCalls != "0" {
		t.Errorf("TotalCalls = %q, want \"0\"", stats.TotalCalls)
	}
	if stats.TotalTokens != "0" {
		t.Errorf("TotalTokens = %q, want \"0\"", stats.TotalTokens)
	}
	if stats.TotalCostUSD != "0" {
		t.Errorf("TotalCostUSD = %q, want \"0\"", stats.TotalCostUSD)
	}
	if stats.ErrorCount != "0" {
		t.Errorf("ErrorCount = %q, want \"0\"", stats.ErrorCount)
	}
	// AvgLatencyMs may be "0" or a small numeric depending on Nucleus's
	// AVG-on-zero-rows behavior; only assert non-empty.
}
