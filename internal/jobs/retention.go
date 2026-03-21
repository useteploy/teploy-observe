package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/teploy/observe/internal/dbutil"
)

// RetentionService cleans up old data according to configured retention periods.
type RetentionService struct {
	db                  *nucleus.Client
	logger              *slog.Logger
	rawRetentionDays    int
	hourlyRetentionDays int
}

func NewRetentionService(db *nucleus.Client, logger *slog.Logger, rawDays, hourlyDays int) *RetentionService {
	return &RetentionService{
		db:                  db,
		logger:              logger,
		rawRetentionDays:    rawDays,
		hourlyRetentionDays: hourlyDays,
	}
}

// RunCleanup deletes data older than the configured retention periods.
// Runs once per day.
func (r *RetentionService) RunCleanup(ctx context.Context) error {
	sql := r.db.SQL()
	now := time.Now().UTC()

	// Delete raw events older than retention period
	rawCutoff := dbutil.IntParam(now.Add(-time.Duration(r.rawRetentionDays) * 24 * time.Hour).UnixMilli())
	affected, err := sql.Exec(ctx,
		`DELETE FROM events WHERE timestamp < $1`, rawCutoff)
	if err != nil {
		return fmt.Errorf("retention: delete raw events: %w", err)
	}
	r.logger.Info("retention: raw events", "deleted", affected, "cutoff_days", r.rawRetentionDays)

	// Delete recent events older than 7 days
	recentCutoff := dbutil.IntParam(now.Add(-7 * 24 * time.Hour).UnixMilli())
	affected, err = sql.Exec(ctx,
		`DELETE FROM events_recent WHERE timestamp < $1`, recentCutoff)
	if err != nil {
		return fmt.Errorf("retention: delete recent events: %w", err)
	}
	r.logger.Info("retention: recent events", "deleted", affected)

	// Delete hourly rollups older than retention period
	hourlyCutoff := dbutil.IntParam(now.Add(-time.Duration(r.hourlyRetentionDays) * 24 * time.Hour).UnixMilli())
	affected, err = sql.Exec(ctx,
		`DELETE FROM stats_hourly WHERE ts_bucket < $1`, hourlyCutoff)
	if err != nil {
		return fmt.Errorf("retention: delete hourly rollups: %w", err)
	}
	r.logger.Info("retention: hourly rollups", "deleted", affected, "cutoff_days", r.hourlyRetentionDays)

	// Sessions older than 90 days
	sessionCutoff := dbutil.IntParam(now.Add(-90 * 24 * time.Hour).UnixMilli())
	affected, err = sql.Exec(ctx,
		`DELETE FROM sessions WHERE last_ts < $1`, sessionCutoff)
	if err != nil {
		return fmt.Errorf("retention: delete old sessions: %w", err)
	}
	r.logger.Info("retention: old sessions", "deleted", affected)

	return nil
}
