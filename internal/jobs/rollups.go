package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// RollupService aggregates raw events into hourly and daily summary tables.
type RollupService struct {
	db     *nucleus.Client
	logger *slog.Logger
}

func NewRollupService(db *nucleus.Client, logger *slog.Logger) *RollupService {
	return &RollupService{db: db, logger: logger}
}

// RunHourlyRollup aggregates events from the last 2 hours into stats_hourly.
// Runs every hour. Uses a 2-hour window to catch late-arriving events.
// The stats_hourly table uses ReplacingMergeTree with a version column,
// so re-inserting for the same bucket key is idempotent — the merge keeps
// the row with the highest version (most recent computation).
func (r *RollupService) RunHourlyRollup(ctx context.Context) error {
	now := time.Now().UTC()
	windowStart := now.Truncate(time.Hour).Add(-2 * time.Hour)
	windowEnd := now.Truncate(time.Hour).Add(time.Hour)

	// Exec uses extended protocol — pass raw int64, NOT string
	startMs := windowStart.UnixMilli()
	endMs := windowEnd.UnixMilli()
	version := now.UnixMilli()

	sql := r.db.SQL()

	// Bucket size inlined in SQL (can't use parameter — Exec uses extended protocol
	// which types string params as TEXT, and Nucleus can't divide BIGINT by TEXT)
	_, err := sql.Exec(ctx, fmt.Sprintf(`
		INSERT INTO stats_hourly (
			tenant_id, site_id, ts_bucket, pathname, event_type,
			pageviews, visitors, sessions, bounces, total_duration,
			version
		)
		SELECT
			tenant_id,
			site_id,
			(timestamp / 3600000) * 3600000 AS ts_bucket,
			COALESCE(pathname, '') AS pathname,
			event_type,
			COUNT(*) AS pageviews,
			COUNT(DISTINCT session_id) AS visitors,
			COUNT(DISTINCT visit_id) AS sessions,
			0 AS bounces,
			0 AS total_duration,
			$3 AS version
		FROM events
		WHERE timestamp >= $1 AND timestamp < $2
		GROUP BY tenant_id, site_id, (timestamp / 3600000) * 3600000, pathname, event_type`),
		startMs, endMs, version,
	)
	if err != nil {
		return fmt.Errorf("hourly rollup: %w", err)
	}

	r.logger.Info("hourly rollup complete", "window_start", windowStart, "window_end", windowEnd)
	return nil
}

// RunDailyRollup aggregates events from the last 2 days into stats_daily.
// Runs once per day. ReplacingMergeTree deduplicates by key on merge.
func (r *RollupService) RunDailyRollup(ctx context.Context) error {
	now := time.Now().UTC()
	windowStart := now.Truncate(24*time.Hour).Add(-48 * time.Hour)
	windowEnd := now.Truncate(24 * time.Hour).Add(24 * time.Hour)

	startMs := windowStart.UnixMilli()
	endMs := windowEnd.UnixMilli()
	version := now.UnixMilli()

	sql := r.db.SQL()

	_, err := sql.Exec(ctx, fmt.Sprintf(`
		INSERT INTO stats_daily (
			tenant_id, site_id, ts_bucket, pathname, event_type,
			referrer, browser, os, country, device,
			utm_source, utm_medium, utm_campaign,
			pageviews, visitors, sessions, bounces, total_duration,
			version
		)
		SELECT
			tenant_id,
			site_id,
			(timestamp / 86400000) * 86400000 AS ts_bucket,
			COALESCE(pathname, '') AS pathname,
			event_type,
			COALESCE(referrer, '') AS referrer,
			COALESCE(browser, '') AS browser,
			COALESCE(os, '') AS os,
			COALESCE(country, '') AS country,
			COALESCE(device, '') AS device,
			COALESCE(utm_source, '') AS utm_source,
			COALESCE(utm_medium, '') AS utm_medium,
			COALESCE(utm_campaign, '') AS utm_campaign,
			COUNT(*) AS pageviews,
			COUNT(DISTINCT session_id) AS visitors,
			COUNT(DISTINCT visit_id) AS sessions,
			0 AS bounces,
			0 AS total_duration,
			$3 AS version
		FROM events
		WHERE timestamp >= $1 AND timestamp < $2
		GROUP BY tenant_id, site_id, (timestamp / 86400000) * 86400000, pathname, event_type,
		         referrer, browser, os, country, device,
		         utm_source, utm_medium, utm_campaign`),
		startMs, endMs, version,
	)
	if err != nil {
		return fmt.Errorf("daily rollup: %w", err)
	}

	r.logger.Info("daily rollup complete", "window_start", windowStart, "window_end", windowEnd)
	return nil
}

// RunSessionRollup aggregates events into session summaries.
// Runs every 5 minutes. Sessions table uses ReplacingMergeTree — overlapping
// re-computation is safe because the merge keeps the row with the highest
// version (most recent computation).
func (r *RollupService) RunSessionRollup(ctx context.Context) error {
	now := time.Now().UTC()
	cutoff := now.Add(-30 * time.Minute).UnixMilli()
	version := now.UnixMilli()

	sql := r.db.SQL()

	// Simple aggregation — no JOINs or subqueries (Nucleus limitations).
	// Entry/exit URLs, referrer, browser etc. are left empty for now.
	// Bounce = sessions with only 1 event total.
	_, err := sql.Exec(ctx, `
		INSERT INTO sessions (
			tenant_id, site_id, session_id,
			first_ts, last_ts, pageviews, events_count,
			entry_url, exit_url,
			referrer, browser, os, device, country, language,
			screen_width, screen_height,
			utm_source, utm_medium, utm_campaign,
			is_bounce, version
		)
		SELECT
			tenant_id,
			site_id,
			session_id,
			MIN(CAST(timestamp AS BIGINT)) AS first_ts,
			MAX(CAST(timestamp AS BIGINT)) AS last_ts,
			COUNT(*) AS pageviews,
			COUNT(*) AS events_count,
			'' AS entry_url,
			'' AS exit_url,
			'' AS referrer,
			'' AS browser,
			'' AS os,
			'' AS device,
			'' AS country,
			'' AS language,
			0 AS screen_width,
			0 AS screen_height,
			'' AS utm_source,
			'' AS utm_medium,
			'' AS utm_campaign,
			CASE WHEN COUNT(*) <= 1 THEN 'true' ELSE 'false' END AS is_bounce,
			$2 AS version
		FROM events
		WHERE timestamp >= $1
		GROUP BY tenant_id, site_id, session_id`,
		cutoff, version,
	)
	if err != nil {
		return fmt.Errorf("session rollup: %w", err)
	}

	r.logger.Debug("session rollup complete")
	return nil
}
