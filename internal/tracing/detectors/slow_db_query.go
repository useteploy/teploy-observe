package detectors

import "fmt"

// SlowDBQueryThresholdMs is the minimum duration for a single DB span to be
// flagged. Sentry's default is 1000ms; we match that — anything under a
// second is too noisy at scale.
const SlowDBQueryThresholdMs int64 = 1000

// SlowDBQuery detects single DB spans whose duration exceeds a threshold.
type SlowDBQuery struct {
	ThresholdMs int64
}

func NewSlowDBQuery() *SlowDBQuery {
	return &SlowDBQuery{ThresholdMs: SlowDBQueryThresholdMs}
}

func (d *SlowDBQuery) Name() string { return "slow_db_query" }

func (d *SlowDBQuery) Detect(spans []Span) []Issue {
	threshold := d.ThresholdMs
	if threshold <= 0 {
		threshold = SlowDBQueryThresholdMs
	}

	var issues []Issue
	for _, s := range spans {
		if !isDBSpan(s) {
			continue
		}
		if s.DurationMs < threshold {
			continue
		}

		stmt := fingerprintSQL(dbStatement(s))
		if stmt == "" {
			// Use the raw operation name for fingerprint when no SQL was
			// present. Better than dropping the issue entirely.
			stmt = s.OperationName
		}
		fp := hashFingerprint("slow_db_query", s.ServiceName, stmt)
		issues = append(issues, Issue{
			TraceID:      s.TraceID,
			DetectorName: "slow_db_query",
			Fingerprint:  fp,
			Title:        fmt.Sprintf("Slow DB query: %dms", s.DurationMs),
			Description:  fmt.Sprintf("%s — %s", s.ServiceName, truncate(stmt, 200)),
			Severity:     severityForDuration(s.DurationMs, threshold),
			FirstSeen:    s.StartMs,
			LastSeen:     s.EndMs,
		})
	}
	return issues
}

// severityForDuration upgrades "warning" to "error" when a span runs more
// than 5x the threshold — keeps loud incidents distinguishable from
// merely-slow ones in the UI.
func severityForDuration(durationMs, threshold int64) string {
	if durationMs >= threshold*5 {
		return "error"
	}
	return "warning"
}
