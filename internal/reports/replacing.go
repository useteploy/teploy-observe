package reports

import "github.com/useteploy/teploy-observe/internal/query"

// report_schedules is a ReplacingMergeTree. Delete is a soft delete
// (enabled='false' at a new version) and every send rewrites last_sent, so a
// schedule accumulates versions in normal operation. Nucleus does not reliably
// collapse them, which made RunScheduled read one ReportSchedule per surviving
// version: a deleted schedule still matched `enabled = 'true'` from its
// pre-delete version, and a live schedule was emailed once per duplicate row.
//
// Reads collapse to the highest version per key and filter enabled after it;
// the soft-delete and the last_sent bump read through the same collapse so each
// writes exactly one row.
var reportScheduleCols = []string{
	"name", "frequency", "recipients", "enabled", "last_sent", "created_at", "version",
}

func reportSchedulesLatest(where string) string {
	return query.LatestRows("report_schedules", reportScheduleCols, where) + " AS report_schedules"
}
