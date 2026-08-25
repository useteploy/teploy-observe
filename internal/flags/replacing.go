package flags

import "github.com/useteploy/teploy-observe/internal/query"

// feature_flags is a ReplacingMergeTree keyed on (tenant_id, site_id, flag_id),
// and Toggle rewrites `enabled` as a new version. Nucleus does not reliably
// collapse the superseded row, so a read of the raw table returns an arbitrary
// version: the SDK's Evaluate could serve the pre-toggle value of a flag
// indefinitely, in either direction. Every read collapses by argMax first, and
// Toggle reads through the same collapse so it writes exactly one row rather
// than one per version already present.
var flagCols = []string{
	"flag_key", "name", "description", "flag_type", "enabled",
	"rollout_pct", "variants", "targeting", "created_at", "version",
}

func flagsLatest(where string) string {
	return query.LatestRows("feature_flags", flagCols, where) + " AS feature_flags"
}
