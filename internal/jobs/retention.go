package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// RetentionPolicy defines the TTL (in days) for a table + the SQL column to compare against.
type RetentionPolicy struct {
	Table       string
	Column      string // BIGINT-as-text column, typically "timestamp", "start_time", or "ts_bucket"
	Days        int
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

// RunCleanup deletes data older than each policy's cutoff.
// Errors on any one policy are logged but don't halt the others.
func (r *RetentionService) RunCleanup(ctx context.Context) error {
	sql := r.db.SQL()
	now := time.Now().UTC()
	var firstErr error

	for _, p := range r.policies {
		if p.Days <= 0 {
			continue
		}
		cutoff := now.Add(-time.Duration(p.Days) * 24 * time.Hour).UnixMilli()
		// Parameter is cast to BIGINT; column is also cast so text-typed BIGINT
		// columns in Nucleus compare numerically rather than lexicographically.
		query := fmt.Sprintf(
			`DELETE FROM %s WHERE CAST(%s AS BIGINT) < CAST($1 AS BIGINT)`,
			p.Table, p.Column,
		)
		affected, err := sql.Exec(ctx, query, strconv.FormatInt(cutoff, 10))
		if err != nil {
			r.logger.Warn("retention: delete failed", "table", p.Table, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("retention: %s: %w", p.Table, err)
			}
			continue
		}
		r.logger.Info("retention: cleanup",
			"table", p.Table, "deleted", affected, "cutoff_days", p.Days)
	}
	return firstErr
}
