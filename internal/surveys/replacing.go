package surveys

import "github.com/useteploy/teploy-observe/internal/query"

// surveys is a ReplacingMergeTree keyed on (tenant_id, site_id, survey_id), and
// Activate / Close rewrite `status` as a new version. Nucleus does not reliably
// collapse the superseded row, so a raw read returns an arbitrary version:
// GetActive matched a closed survey through its old status='active' row, and
// SubmitResponse's ownership-and-status gate — the only check on a public,
// unauthenticated endpoint — decided against whichever version came back first.
//
// status changes between versions, so it must be filtered AFTER the collapse,
// never inside it. Activate / Close read through the collapse so each writes
// exactly one row.
var surveyCols = []string{
	"name", "questions", "appearance", "targeting", "status", "created_at", "version",
}

func surveysLatest(where string) string {
	return query.LatestRows("surveys", surveyCols, where) + " AS surveys"
}
