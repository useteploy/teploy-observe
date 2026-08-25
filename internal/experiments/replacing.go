package experiments

import "github.com/useteploy/teploy-observe/internal/query"

// experiments is a ReplacingMergeTree keyed on (tenant_id, site_id,
// experiment_id), and Start / Stop rewrite `status`, `started_at` and
// `ended_at` as a new version. Nucleus does not reliably collapse the
// superseded rows, so a read of the raw table returns an arbitrary version — a
// stopped experiment could keep reporting as running — and a write shaped as
// `INSERT INTO experiments SELECT ... FROM experiments` inserted one row per
// version already present, doubling the row count on every transition.
//
// Reads collapse by argMax first, and the two writers read through the same
// collapse so each writes exactly one row.
var experimentCols = []string{
	"name", "flag_key", "goal_metric", "goal_value", "status", "min_sample",
	"variants", "started_at", "ended_at", "created_at", "version",
}

// experimentsLatest renders the collapsed derived table, aliased `experiments`
// so the surrounding query reads unchanged. where is applied before the
// collapse, so pass only version-stable predicates (the key itself); status,
// started_at and ended_at change between versions and must be filtered outside.
func experimentsLatest(where string) string {
	return query.LatestRows("experiments", experimentCols, where) + " AS experiments"
}
