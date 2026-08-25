package monitoring

import "github.com/useteploy/teploy-observe/internal/query"

// uptime_monitors and cron_monitors are ReplacingMergeTree tables, and delete on
// both is a soft delete: a new version with enabled='false'. Nucleus does not
// reliably collapse the superseded row, so `WHERE enabled = 'true'` read against
// the raw table still matched the pre-delete version. A deleted uptime monitor
// kept issuing HTTP checks and a deleted cron monitor kept alerting on missed
// runs — and, the security-relevant one, RecordCheckinByToken kept accepting a
// deleted monitor's ping token, so revoking a heartbeat credential by deleting
// the monitor did nothing.
//
// Reads collapse to the highest version per key and filter enabled AFTER the
// collapse; the soft-delete reads through the same collapse so it writes exactly
// one row instead of one per version already present.

var uptimeMonitorCols = []string{
	"name", "url", "method", "interval_secs", "expected_status",
	"enabled", "created_at", "version",
}

func uptimeMonitorsLatest(where string) string {
	return query.LatestRows("uptime_monitors", uptimeMonitorCols, where) + " AS uptime_monitors"
}

// ping_token is added by migration 026, after 005 created the table.
var cronMonitorCols = []string{
	"name", "slug", "schedule", "grace_period", "enabled",
	"ping_token", "created_at", "version",
}

func cronMonitorsLatest(where string) string {
	return query.LatestRows("cron_monitors", cronMonitorCols, where) + " AS cron_monitors"
}
