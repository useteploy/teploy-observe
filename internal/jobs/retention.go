package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"
)

// retentionChunkSize bounds each DELETE statement in RunCleanup so a large
// backlog (e.g. after retention sat inert for a while, or a burst of
// traffic) is removed in many small batches instead of one giant DML burst.
// An unbounded multi-million-row DELETE is itself heavy enough to worsen the
// same Nucleus memory-pressure write-reject that batched ingest (see
// internal/ingest/buffer.go, internal/tracing/ingest.go, internal/logs/logs.go,
// internal/metrics/metrics.go) exists to avoid.
const retentionChunkSize = 5000

// MaxRetentionDays fits duration arithmetic and the supported configuration.
const MaxRetentionDays = 100000

// TimeUnit is the unit of a policy's BIGINT time column. The zero value is
// epoch milliseconds, which is what nearly every table stores, so existing
// policy literals keep their meaning.
type TimeUnit int

const (
	// UnitMillis is epoch milliseconds (the default).
	UnitMillis TimeUnit = iota
	// UnitNanos is epoch nanoseconds (metric_points.ts_ns, OTLP TimeUnixNano).
	// A millisecond cutoff compared against such a column is ~1e6 times too
	// small, so the TTL would silently match nothing.
	UnitNanos
)

// CutoffAt returns the cutoff for a retention window of days ending at now,
// expressed in the unit of the column it will be compared against.
func (u TimeUnit) CutoffAt(now time.Time, days int) int64 {
	if days <= 0 || days > MaxRetentionDays {
		return math.MinInt64
	}
	t := now.Add(-time.Duration(days) * 24 * time.Hour)
	if u == UnitNanos {
		if t.Before(time.Unix(0, math.MinInt64)) || t.After(time.Unix(0, math.MaxInt64)) {
			return math.MinInt64
		}
		return t.UnixNano()
	}
	return t.UnixMilli()
}

// RetentionPolicy defines the TTL (in days) for a table + the SQL column to compare against.
type RetentionPolicy struct {
	Table  string
	Column string // BIGINT epoch column (see Unit), typically "timestamp", "start_time", or "ts_bucket"
	Days   int
	// Unit is the time unit of Column; the zero value is epoch milliseconds.
	Unit TimeUnit
	// ExtraWhere is an optional additional predicate ANDed into the
	// chunk-boundary SELECT and the DELETE (e.g. "processed_at > 0" so the
	// derived_outbox policy prunes processed intents but never dead
	// letters). Callers construct policies in code — this is never bound
	// from user input, so it is raw SQL by design.
	ExtraWhere string
}

// and returns the policy's predicate conjoined with any extra WHERE term.
func (p RetentionPolicy) and() string {
	if p.ExtraWhere == "" {
		return ""
	}
	return " AND " + p.ExtraWhere
}

// RetentionService cleans up old data according to configured retention periods.
type RetentionService struct {
	now      func() time.Time
	db       *nucleus.Client
	logger   *slog.Logger
	policies []RetentionPolicy
}

// DefaultPolicies returns the out-of-box retention policies, using configured
// durations for raw events, hourly rollups and LLM traces.
func DefaultPolicies(rawDays, hourlyDays, llmDays int) []RetentionPolicy {
	return []RetentionPolicy{
		{Table: "events", Column: "timestamp", Days: rawDays},
		{Table: "events_recent", Column: "timestamp", Days: 7},
		{Table: "stats_hourly", Column: "ts_bucket", Days: hourlyDays},
		{Table: "sessions", Column: "last_ts", Days: 90},
		{Table: "error_events", Column: "timestamp", Days: 180},
		{Table: "logs", Column: "timestamp", Days: 30},
		{Table: "llm_traces", Column: "timestamp", Days: llmDays},
		{Table: "spans", Column: "start_time", Days: 14},
		{Table: "span_links", Column: "start_time", Days: 14},
		{Table: "experiment_metric_events", Column: "timestamp", Days: 730},
		{Table: "service_stats", Column: "ts_bucket", Days: 30},
		{Table: "replay_sessions", Column: "start_time", Days: 14},
		// O10: the evaluation ledger is a log, not a dedupe set - 14 days
		// of transition edges is far beyond any operator investigation
		// window while bounding the one-row-per-tick growth.
		{Table: "alert_evaluations", Column: "evaluated_at", Days: 14},
	}
}

// DefaultTelemetryPolicies returns retention for the telemetry tables that
// DefaultPolicies historically did not cover and that therefore grew without
// bound. Units and columns were verified against the writers:
//
//   - metric_points.ts_ns is epoch NANOSECONDS (OTLP TimeUnixNano), hence
//     UnitNanos.
//   - host_metrics.timestamp and uptime_results.timestamp are epoch ms
//     (time.Now().UnixMilli() in internal/infra and internal/monitoring).
//   - performance_issues is a replacing_mergetree keyed on (tenant, site,
//     fingerprint) versioned by last_seen (span-time ms). Deleting
//     last_seen < cutoff removes superseded versions and issues not
//     re-detected within the window; a live issue keeps writing a newer
//     last_seen, so it survives. Fixed 90 days, well beyond the 14-day span
//     retention its trace_id drill-down depends on.
//   - service_dependencies is a replacing_mergetree whose ORDER BY includes
//     ts_bucket (ms), so each time bucket is its own key and a bucket is only
//     ever rewritten with the same key. Deleting old buckets is the same shape
//     as the existing service_stats policy and is aligned to it (30 days).
func DefaultTelemetryPolicies(metricsDays, infraDays, uptimeDays int) []RetentionPolicy {
	return []RetentionPolicy{
		{Table: "metric_points", Column: "ts_ns", Days: metricsDays, Unit: UnitNanos},
		{Table: "host_metrics", Column: "timestamp", Days: infraDays},
		{Table: "uptime_results", Column: "timestamp", Days: uptimeDays},
		{Table: "performance_issues", Column: "last_seen", Days: 90},
		{Table: "service_dependencies", Column: "ts_bucket", Days: 30},
	}
}

// DefaultLedgerPolicies returns the O01 ADR section 5.8 retention for the
// durability ledgers. Each window is the decided 2026-09-23 default
// (DELEGATED_DECISIONS section 5) and is env-tunable by the caller.
//
// Expiry semantics (the ADR's honest statement, pinned here so it travels
// with the policy): a ledger row that expires is gone from the dedupe set —
// a producer retrying that same record after expiry is processed as NEW.
// The ledgers dedupe data whose own retention is at least as long
// (error_events 180d vs the inbox 14d; replay_sessions 14d matching its
// ledger exactly), so the ordering never re-inserts orphaned children.
//
// derived_outbox prunes PROCESSED intents only (processed_at > 0): rows
// still pending, retrying, or dead-lettered are never auto-deleted — the
// dead letters are the operator's queue, surfaced at /healthz counters.
//
// notification_outbox (O10) follows the same decided posture at the same
// window: DELIVERED or SUPPRESSED intents prune at 7 days; pending,
// retrying and dead-lettered notifications are never auto-deleted. The
// window is fixed here (not env-tunable) to match the decided default.
func DefaultLedgerPolicies(errorInboxDays, replayBatchesDays, outboxDays int) []RetentionPolicy {
	return []RetentionPolicy{
		{Table: "error_inbox", Column: "applied_at", Days: errorInboxDays},
		{Table: "replay_batches", Column: "first_seen", Days: replayBatchesDays},
		{Table: "derived_outbox", Column: "processed_at", Days: outboxDays, ExtraWhere: "processed_at > 0"},
		{Table: "notification_outbox", Column: "created_at", Days: 7, ExtraWhere: "(delivered_at > 0 OR suppressed_at > 0)"},
	}
}

// PolicyDays returns the retention window configured for a table, in days, or
// 0 if the table has no policy (0 also being "prunes nothing", which is what an
// unlisted table gets). The analytics read path uses it to decide which table
// can still answer a unique count for a given range.
func PolicyDays(policies []RetentionPolicy, table string) int {
	for _, p := range policies {
		if p.Table == table {
			return p.Days
		}
	}
	return 0
}

// NewRetentionService keeps the old two-arg constructor for backwards compat.
func NewRetentionService(db *nucleus.Client, logger *slog.Logger, rawDays, hourlyDays int) *RetentionService {
	return NewRetentionServiceWithPolicies(db, logger, DefaultPolicies(rawDays, hourlyDays, 30))
}

// NewRetentionServiceWithPolicies allows callers to supply a fully custom policy set.
func NewRetentionServiceWithPolicies(db *nucleus.Client, logger *slog.Logger, policies []RetentionPolicy) *RetentionService {
	return &RetentionService{db: db, logger: logger, policies: policies}
}

// Policies returns a copy of the configured policies (for /meta endpoints).
func (r *RetentionService) Policies() []RetentionPolicy {
	out := make([]RetentionPolicy, len(r.policies))
	copy(out, r.policies)
	return out
}

// RunCleanup deletes data older than each policy's cutoff, in bounded
// chunks (see cleanupTable). Errors on any one policy are logged but don't
// halt the others.
func (r *RetentionService) RunCleanup(ctx context.Context) error {
	sql := r.db.SQL()
	now := time.Now().UTC()
	if r.now != nil {
		now = r.now().UTC()
	}
	var firstErr error

	for _, p := range r.policies {
		if p.Days <= 0 {
			continue
		}
		cutoff := p.Unit.CutoffAt(now, p.Days)
		if cutoff == math.MinInt64 || (p.Unit != UnitMillis && p.Unit != UnitNanos) {
			if firstErr == nil {
				firstErr = fmt.Errorf("retention: %s: unsupported retention window or unit", p.Table)
			}
			continue
		}
		if p.Table == "replay_sessions" {
			if err := r.cleanupReplayPayloads(ctx, cutoff); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				// Preserve parents until child disposition is complete, so a
				// failed/restarted cleanup still has the lifecycle evidence.
				continue
			}
		}
		// Bind the cutoff as an int64 and compare against the BIGINT column
		// directly. The old form (quoted text literal vs CAST(col AS BIGINT))
		// matched nothing — Nucleus compared the column's numeric value against
		// a text literal lexicographically, so every TTL was inert and storage
		// grew unbounded. Nucleus now coerces a numeric param against a BIGINT
		// (or text-numeric) column, so the plain `col < $1` deletes correctly.
		deleted, err := r.cleanupTable(ctx, sql, p, cutoff)
		if err != nil {
			r.logger.Warn("retention: delete failed", "table", p.Table, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("retention: %s: %w", p.Table, err)
			}
			continue
		}
		r.logger.Info("retention: cleanup",
			"table", p.Table, "deleted", deleted, "cutoff_days", p.Days)
	}
	return firstErr
}

// cleanupTable uses stable composite identities plus a physical-row budget,
// so timestamp ties cannot expand a DELETE beyond retentionChunkSize.
func (r *RetentionService) cleanupTable(ctx context.Context, sql *nucleus.SQLModel, p RetentionPolicy, cutoff int64) (int64, error) {
	return r.cleanupWhere(ctx, p.Table, fmt.Sprintf("%s < $1%s", p.Column, p.and()), []any{cutoff}, append(append([]string(nil), retentionKeys[p.Table]...), p.Column))
}

type boundaryRow struct {
	C string `db:"c"`
}
