package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// retentionChunkSize bounds each DELETE statement in RunCleanup so a large
// backlog (e.g. after retention sat inert for a while, or a burst of
// traffic) is removed in many small batches instead of one giant DML burst.
// An unbounded multi-million-row DELETE is itself heavy enough to worsen the
// same Nucleus memory-pressure write-reject that batched ingest (see
// internal/ingest/buffer.go, internal/tracing/ingest.go, internal/logs/logs.go,
// internal/metrics/metrics.go) exists to avoid.
const retentionChunkSize = 5000

// RetentionPolicy defines the TTL (in days) for a table + the SQL column to compare against.
type RetentionPolicy struct {
	Table  string
	Column string // BIGINT epoch-ms column, typically "timestamp", "start_time", or "ts_bucket"
	Days   int
}

// RetentionService cleans up old data according to configured retention periods.
type RetentionService struct {
	db       *nucleus.Client
	logger   *slog.Logger
	policies []RetentionPolicy
}

// DefaultPolicies returns the out-of-box retention policies. Called with the two
// legacy env-configured durations for raw events + hourly rollups so existing
// deployments keep their behavior.
func DefaultPolicies(rawDays, hourlyDays int) []RetentionPolicy {
	return []RetentionPolicy{
		{"events", "timestamp", rawDays},
		{"events_recent", "timestamp", 7},
		{"stats_hourly", "ts_bucket", hourlyDays},
		{"sessions", "last_ts", 90},
		{"error_events", "timestamp", 180},
		{"logs", "timestamp", 30},
		{"spans", "start_time", 14},
		{"service_stats", "ts_bucket", 30},
		{"replay_sessions", "start_time", 14},
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
	return NewRetentionServiceWithPolicies(db, logger, DefaultPolicies(rawDays, hourlyDays))
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
	var firstErr error

	for _, p := range r.policies {
		if p.Days <= 0 {
			continue
		}
		cutoff := now.Add(-time.Duration(p.Days) * 24 * time.Hour).UnixMilli()
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

// cleanupTable deletes rows older than cutoff in chunks of at most
// retentionChunkSize instead of one unbounded DELETE. The retention tables
// don't share a common single-column primary key (events has event_id,
// logs has log_id, service_stats/stats_hourly have none at all — they're
// rollup facts keyed by a composite), so chunking can't key off "id IN
// (SELECT id ... LIMIT N)" generically. Instead each iteration finds the
// value of the chunkSize-th oldest surviving row in the policy's own cutoff
// column (already BIGINT epoch-ms, already used for the WHERE bound) and
// deletes everything up to and including it; once fewer than a full chunk
// remain, one final small DELETE clears the rest.
func (r *RetentionService) cleanupTable(ctx context.Context, sql *nucleus.SQLModel, p RetentionPolicy, cutoff int64) (int64, error) {
	var total int64
	for {
		boundary, ok, err := chunkBoundary(ctx, sql, p.Table, p.Column, cutoff, retentionChunkSize)
		if err != nil {
			return total, err
		}
		if !ok {
			// Fewer than a full chunk remain below cutoff — clear the rest
			// in one final (bounded) statement and stop.
			query := fmt.Sprintf(`DELETE FROM %s WHERE %s < $1`, p.Table, p.Column)
			affected, err := sql.Exec(ctx, query, cutoff)
			if err != nil {
				return total, err
			}
			return total + affected, nil
		}

		// boundary ties (many rows sharing the exact same column value, e.g.
		// a 60s ts_bucket) mean this DELETE can remove somewhat more than
		// chunkSize rows — still bounded to "one bucket's worth", nowhere
		// near the size of an unbounded whole-policy DELETE.
		query := fmt.Sprintf(`DELETE FROM %s WHERE %s <= $1`, p.Table, p.Column)
		affected, err := sql.Exec(ctx, query, boundary)
		if err != nil {
			return total, err
		}
		total += affected
		if affected == 0 {
			// Defensive: a boundary was found but nothing was deleted (e.g.
			// a concurrent cleanup already won the race) — stop instead of
			// looping forever.
			return total, nil
		}
	}
}

type boundaryRow struct {
	C string `db:"c"`
}

// chunkBoundary returns the cutoff column's value at the chunkSize-th
// oldest surviving row (ok=true), so the caller can DELETE ... WHERE col <=
// boundary as one bounded batch. ok=false means fewer than chunkSize rows
// remain below cutoff — the caller should do one final, already-small
// DELETE instead.
func chunkBoundary(ctx context.Context, sql *nucleus.SQLModel, table, column string, cutoff int64, chunkSize int) (int64, bool, error) {
	query := fmt.Sprintf(
		`SELECT %s AS c FROM %s WHERE %s < $1 ORDER BY %s ASC LIMIT 1 OFFSET %d`,
		column, table, column, column, chunkSize-1)
	rows, err := nucleus.Query[boundaryRow](ctx, sql, query, cutoff)
	if err != nil {
		return 0, false, err
	}
	if len(rows) == 0 {
		return 0, false, nil
	}
	v, err := strconv.ParseInt(rows[0].C, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("chunk boundary: parse %q: %w", rows[0].C, err)
	}
	return v, true, nil
}
