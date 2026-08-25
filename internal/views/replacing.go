package views

import "github.com/useteploy/teploy-observe/internal/query"

// saved_views is a ReplacingMergeTree keyed on (tenant_id, site_id, view_id).
// Nucleus does not reliably collapse superseded versions, so List has to do it
// with argMax rather than trust the engine.
var viewCols = []string{
	"name", "view_config", "created_by", "created_at", "version",
}

// savedViewsLatest renders the collapsed derived table, aliased `saved_views`
// so the surrounding query reads unchanged. where is applied before the
// collapse, so pass only version-stable predicates; `name` is rewritten by the
// legacy tombstone delete and must be filtered outside.
func savedViewsLatest(where string) string {
	return query.LatestRows("saved_views", viewCols, where) + " AS saved_views"
}
