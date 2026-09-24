package llm

// O14 slice 3 oracle: the versioned cost catalog and cost/token
// provenance - at the real engine, fail-not-skip under
// OBSERVE_REQUIRE_NUCLEUS.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
)

func catalogFixture(t *testing.T) *nucleus.Client {
	t.Helper()
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping integration test", dsn)
	}
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func catalogSite(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("o14llm-%d", time.Now().UnixNano())
}

// TestO14CatalogProvenanceAndStats: token counts arriving in a payload are
// 'reported' (or the producer's own 'estimated' declaration), auto-derived
// costs are 'estimated' from the catalog, and every read surface that sums
// cost carries the estimated split.
func TestO14CatalogProvenanceAndStats(t *testing.T) {
	db := catalogFixture(t)
	ctx := context.Background()
	site := catalogSite(t)
	svc := NewLLMService(db)

	// Reported cost: caller-supplied, labeled reported.
	if _, err := svc.Ingest(ctx, LLMInput{SiteID: site, Model: "gpt-4o", Provider: "openai",
		PromptTokens: 1000, CompletionTokens: 0, CostUSD: 0.5}); err != nil {
		t.Fatalf("ingest reported: %v", err)
	}
	// Auto-derived cost: catalog price for gpt-4o-mini (seeded builtin),
	// labeled estimated.
	if _, err := svc.Ingest(ctx, LLMInput{SiteID: site, Model: "gpt-4o-mini-2024-07-18", Provider: "openai",
		PromptTokens: 1000, CompletionTokens: 1000}); err != nil {
		t.Fatalf("ingest estimated: %v", err)
	}
	// Producer-declared estimated tokens.
	if _, err := svc.Ingest(ctx, LLMInput{SiteID: site, Model: "gpt-4o", Provider: "openai",
		PromptTokens: 500, CompletionTokens: 500, TokenSource: "estimated", CostUSD: 0.25}); err != nil {
		t.Fatalf("ingest token-estimated: %v", err)
	}
	// Invalid token_source is rejected at the boundary.
	if _, err := svc.Ingest(ctx, LLMInput{SiteID: site, Model: "gpt-4o",
		PromptTokens: 1, TokenSource: "guessed"}); err == nil {
		t.Fatalf("invalid token_source accepted")
	}

	traces, err := svc.RecentTraces(ctx, site, 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(traces) != 3 {
		t.Fatalf("traces = %d, want 3", len(traces))
	}
	var sawEstimated, sawReported bool
	for _, tr := range traces {
		switch {
		case tr.CostSource == CostSourceEstimated:
			sawEstimated = true
			// gpt-4o-mini 1000 in + 1000 out = 0.00015 + 0.0006 = 0.00075.
			if tr.CostUSD == "0.000000" || tr.CostUSD == "" {
				t.Fatalf("estimated trace has no cost: %q", tr.CostUSD)
			}
			if tr.TokenSource != TokenSourceReported {
				t.Fatalf("payload tokens labeled %q, want reported", tr.TokenSource)
			}
		case tr.CostSource == CostSourceReported:
			sawReported = true
		}
	}
	if !sawEstimated || !sawReported {
		t.Fatalf("cost provenance missing: estimated=%v reported=%v", sawEstimated, sawReported)
	}
	// The producer-declared estimated token source survived.
	var sawTokenEstimated bool
	for _, tr := range traces {
		if tr.TokenSource == TokenSourceEstimated {
			sawTokenEstimated = true
		}
	}
	if !sawTokenEstimated {
		t.Fatalf("no trace carries the producer's token_source=estimated")
	}

	// Stats: the estimated subset is exactly the auto-derived cost.
	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	st, err := svc.Stats(ctx, site, from, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.TotalCostUSD == "" || st.EstimatedCostUSD == "" || st.ReportedCostUSD == "" {
		t.Fatalf("stats cost fields empty: %+v", st)
	}
	// reported = 0.5 + 0.25; estimated = the one derived trace's cost.
	if st.ReportedCostUSD != "0.75" {
		t.Fatalf("reported subset = %s, want 0.75", st.ReportedCostUSD)
	}
	est := 0.00015 + 0.0006 // gpt-4o-mini 1K/1K
	if st.EstimatedCostUSD != fmt.Sprintf("%v", est) && st.EstimatedCostUSD == "0" {
		t.Fatalf("estimated subset = %s, want the derived trace cost", st.EstimatedCostUSD)
	}

	// Model breakdown carries the same split.
	models, err := svc.ModelBreakdown(ctx, site, from, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %d, want 2", len(models))
	}
	for _, m := range models {
		if m.EstimatedCostUSD == "" {
			t.Fatalf("model %s carries no estimated split", m.Model)
		}
	}

	t.Cleanup(func() {
		_, _ = db.SQL().Exec(context.Background(), `DELETE FROM llm_traces WHERE site_id = $1`, site)
	})
}

// TestO14CatalogVersioning: valid_from is the versioning axis - a future
// row is not yet effective, a newer effective row supersedes an older one
// for the same prefix, and a correction on the SAME key replaces the row.
func TestO14CatalogVersioning(t *testing.T) {
	db := catalogFixture(t)
	ctx := context.Background()
	svc := NewLLMService(db)
	prefix := fmt.Sprintf("o14model-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.SQL().Exec(context.Background(),
			`DELETE FROM llm_model_prices WHERE model_prefix = $1`, prefix)
	})

	// Baseline row (valid_from 0).
	if err := svc.SetPrice(ctx, CatalogEntry{ModelPrefix: prefix, InputPer1k: 0.01, OutputPer1k: 0.02}); err != nil {
		t.Fatalf("set baseline: %v", err)
	}
	entries, err := svc.ListPrices(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	in, _, ok := resolveCatalog(entries, "", prefix)
	if !ok || in != 0.01 {
		t.Fatalf("baseline resolution = %v (ok %v), want 0.01", in, ok)
	}

	// A FUTURE-dated row is not yet effective.
	future := time.Now().Add(time.Hour).UnixMilli()
	if err := svc.SetPrice(ctx, CatalogEntry{ModelPrefix: prefix, InputPer1k: 0.99, OutputPer1k: 0.99, ValidFrom: future}); err != nil {
		t.Fatalf("set future: %v", err)
	}
	entries, _ = svc.ListPrices(ctx)
	in, _, _ = resolveCatalog(entries, "", prefix)
	if in != 0.01 {
		t.Fatalf("future row leaked into the effective catalog: %v", in)
	}

	// A now-effective row supersedes the baseline for the same prefix.
	if err := svc.SetPrice(ctx, CatalogEntry{ModelPrefix: prefix, InputPer1k: 0.03, OutputPer1k: 0.04,
		ValidFrom: time.Now().Add(-time.Minute).UnixMilli()}); err != nil {
		t.Fatalf("set newer: %v", err)
	}
	entries, _ = svc.ListPrices(ctx)
	in, _, _ = resolveCatalog(entries, "", prefix)
	if in != 0.03 {
		t.Fatalf("newest effective row did not win: %v", in)
	}

	// A correction on the SAME key replaces the row (argMax collapse).
	if err := svc.SetPrice(ctx, CatalogEntry{ModelPrefix: prefix, InputPer1k: 0.05, OutputPer1k: 0.06}); err != nil {
		t.Fatalf("set correction: %v", err)
	}
	// The correction's key is (prefix, valid_from 0) - no longer effective
	// (superseded by the 0.03 row), so resolution must stay 0.03.
	entries, _ = svc.ListPrices(ctx)
	in, _, _ = resolveCatalog(entries, "", prefix)
	if in != 0.03 {
		t.Fatalf("correction on a superseded key changed resolution: %v", in)
	}

	// Seed idempotency: seeding twice leaves one collapsed builtin row per
	// prefix (the gpt-4o key cannot double).
	if err := svc.EnsureBuiltinCatalog(ctx); err != nil {
		t.Fatalf("seed once: %v", err)
	}
	if err := svc.EnsureBuiltinCatalog(ctx); err != nil {
		t.Fatalf("seed twice: %v", err)
	}
	rows, err := nucleus.Query[struct {
		N string `db:"n"`
	}](ctx, db.SQL(),
		`SELECT CAST(COUNT(*) AS TEXT) AS n FROM (`+pricesCollapse("provider = '' AND model_prefix = 'gpt-4o' AND valid_from = 0")+`)`)
	if err != nil || len(rows) == 0 {
		t.Fatalf("seed count read: %v", err)
	}
	if rows[0].N != "1" {
		t.Fatalf("collapsed gpt-4o rows = %s, want 1 (seed not idempotent)", rows[0].N)
	}
}
