package experiments

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

func expTestDB(t *testing.T) (*nucleus.Client, func()) {
	t.Helper()
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		cancel()
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	// Ensure schema exists even if the migration runner has not been run
	// against this instance (tests must be self-contained).
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS experiments (
			experiment_id TEXT NOT NULL, tenant_id TEXT NOT NULL DEFAULT 'default', site_id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '', flag_key TEXT NOT NULL, goal_metric TEXT NOT NULL DEFAULT 'pageview',
			goal_value TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'draft', min_sample TEXT NOT NULL DEFAULT '100',
			started_at TEXT NOT NULL DEFAULT '0', ended_at TEXT NOT NULL DEFAULT '0', created_at TEXT NOT NULL,
			version BIGINT NOT NULL DEFAULT 0
		) WITH (engine = 'replacing_mergetree', version_column = 'version') ORDER BY (tenant_id, site_id, experiment_id)`,
		`CREATE TABLE IF NOT EXISTS experiment_exposures (
			exposure_id TEXT NOT NULL, tenant_id TEXT NOT NULL DEFAULT 'default', experiment_id TEXT NOT NULL,
			site_id TEXT NOT NULL, user_id TEXT NOT NULL DEFAULT '', variant TEXT NOT NULL DEFAULT '',
			converted TEXT NOT NULL DEFAULT 'false', timestamp BIGINT NOT NULL
		) WITH (engine = 'mergetree') ORDER BY (tenant_id, site_id, experiment_id, timestamp)`,
		`CREATE TABLE IF NOT EXISTS experiment_conversions (
			conversion_id TEXT NOT NULL, tenant_id TEXT NOT NULL DEFAULT 'default', experiment_id TEXT NOT NULL,
			site_id TEXT NOT NULL, user_id TEXT NOT NULL DEFAULT '', variant TEXT NOT NULL DEFAULT '',
			timestamp BIGINT NOT NULL
		) WITH (engine = 'mergetree') ORDER BY (tenant_id, site_id, experiment_id, user_id)`,
	} {
		if _, err := db.SQL().Exec(ctx, ddl); err != nil {
			db.Close()
			cancel()
			t.Fatalf("ensure schema: %v", err)
		}
	}
	// The experiments table predates the variants column on existing instances.
	if _, err := db.SQL().Exec(ctx, `ALTER TABLE experiments ADD COLUMN IF NOT EXISTS variants TEXT NOT NULL DEFAULT ''`); err != nil {
		db.Close()
		cancel()
		t.Fatalf("ensure variants column: %v", err)
	}
	return db, func() { db.Close(); cancel() }
}

// TestResults_RecordsAndCountsDistinct is the regression for the HIGH finding
// that exposures/conversions were never recorded and Results was always empty,
// plus the medium that RecordConversion double-counted via row-copy.
func TestResults_RecordsAndCountsDistinct(t *testing.T) {
	db, done := expTestDB(t)
	defer done()
	ctx := context.Background()
	svc := NewExperimentService(db)

	site := fmt.Sprintf("test-exp-%d", time.Now().UnixNano())
	exp, err := svc.Create(ctx, site, "Homepage CTA", "cta_flag", "pageview", "",
		`[{"key":"control","name":"Control","rollout_pct":50},{"key":"treatment","name":"Treatment","rollout_pct":50}]`, 4)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// control: 3 exposures, 1 converts (record conversion twice → must count once)
	for _, u := range []string{"c1", "c2", "c3"} {
		if err := svc.RecordExposure(ctx, exp.ExperimentID, site, u, "control"); err != nil {
			t.Fatalf("expose: %v", err)
		}
	}
	// re-expose c1 (must not inflate exposures: DISTINCT user_id)
	_ = svc.RecordExposure(ctx, exp.ExperimentID, site, "c1", "control")
	_ = svc.RecordConversion(ctx, exp.ExperimentID, site, "c1")
	_ = svc.RecordConversion(ctx, exp.ExperimentID, site, "c1") // duplicate

	// treatment: 3 exposures, 3 convert
	for _, u := range []string{"t1", "t2", "t3"} {
		_ = svc.RecordExposure(ctx, exp.ExperimentID, site, u, "treatment")
		_ = svc.RecordConversion(ctx, exp.ExperimentID, site, u)
	}

	res, err := svc.Results(ctx, exp.ExperimentID, site)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if len(res.Variants) != 2 {
		t.Fatalf("expected 2 variants, got %d (%+v)", len(res.Variants), res.Variants)
	}
	// Control must be index 0 (declared first).
	if res.Variants[0].Variant != "control" {
		t.Fatalf("expected control at index 0, got %q", res.Variants[0].Variant)
	}
	byV := map[string]VariantResult{}
	for _, v := range res.Variants {
		byV[v.Variant] = v
	}
	if got := byV["control"]; got.Exposures != 3 || got.Conversions != 1 {
		t.Fatalf("control: want 3 exposures / 1 conversion, got %d / %d", got.Exposures, got.Conversions)
	}
	if got := byV["treatment"]; got.Exposures != 3 || got.Conversions != 3 {
		t.Fatalf("treatment: want 3 exposures / 3 conversions, got %d / %d", got.Exposures, got.Conversions)
	}
}

// TestResults_GatesOnMinSample verifies a tiny sample never reports significance.
func TestResults_GatesOnMinSample(t *testing.T) {
	db, done := expTestDB(t)
	defer done()
	ctx := context.Background()
	svc := NewExperimentService(db)

	site := fmt.Sprintf("test-exp-min-%d", time.Now().UnixNano())
	exp, err := svc.Create(ctx, site, "Tiny", "tiny_flag", "pageview", "",
		`[{"key":"control"},{"key":"treatment"}]`, 1000)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Extreme split but far below min_sample: 0/3 vs 3/3.
	for _, u := range []string{"c1", "c2", "c3"} {
		_ = svc.RecordExposure(ctx, exp.ExperimentID, site, u, "control")
	}
	for _, u := range []string{"t1", "t2", "t3"} {
		_ = svc.RecordExposure(ctx, exp.ExperimentID, site, u, "treatment")
		_ = svc.RecordConversion(ctx, exp.ExperimentID, site, u)
	}
	res, err := svc.Results(ctx, exp.ExperimentID, site)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if res.Significant || res.Winner != "" {
		t.Fatalf("min_sample=1000 with 6 exposures must not be significant; got significant=%v winner=%q", res.Significant, res.Winner)
	}
}
