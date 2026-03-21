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
			e.tenant_id,
			e.site_id,
			(e.timestamp / 3600000) * 3600000 AS ts_bucket,
			COALESCE(e.pathname, '') AS pathname,
			e.event_type,
			COUNT(*) AS pageviews,
			COUNT(DISTINCT e.session_id) AS visitors,
			COUNT(DISTINCT e.visit_id) AS sessions,
			SUM(CASE WHEN s.is_bounce = 'true' THEN 1 ELSE 0 END) AS bounces,
			COALESCE(SUM(s.last_ts - s.first_ts), 0) AS total_duration,
			$3 AS version
		FROM events e
		LEFT JOIN sessions s ON s.session_id = e.session_id AND s.site_id = e.site_id
		WHERE e.timestamp >= $1 AND e.timestamp < $2
		GROUP BY e.tenant_id, e.site_id, (e.timestamp / 3600000) * 3600000, e.pathname, e.event_type`),
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
			e.tenant_id,
			e.site_id,
			(e.timestamp / 86400000) * 86400000 AS ts_bucket,
			COALESCE(e.pathname, '') AS pathname,
			e.event_type,
			COALESCE(e.referrer, '') AS referrer,
			COALESCE(e.browser, '') AS browser,
			COALESCE(e.os, '') AS os,
			COALESCE(e.country, '') AS country,
			COALESCE(e.device, '') AS device,
			COALESCE(e.utm_source, '') AS utm_source,
			COALESCE(e.utm_medium, '') AS utm_medium,
			COALESCE(e.utm_campaign, '') AS utm_campaign,
			COUNT(*) AS pageviews,
			COUNT(DISTINCT e.session_id) AS visitors,
			COUNT(DISTINCT e.visit_id) AS sessions,
			SUM(CASE WHEN s.is_bounce = 'true' THEN 1 ELSE 0 END) AS bounces,
			COALESCE(SUM(s.last_ts - s.first_ts), 0) AS total_duration,
			$3 AS version
		FROM events e
		LEFT JOIN sessions s ON s.session_id = e.session_id AND s.site_id = e.site_id
		WHERE e.timestamp >= $1 AND e.timestamp < $2
		GROUP BY e.tenant_id, e.site_id, (e.timestamp / 86400000) * 86400000, e.pathname, e.event_type,
		         e.referrer, e.browser, e.os, e.country, e.device,
		         e.utm_source, e.utm_medium, e.utm_campaign`),
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

	// Two-pass approach:
	// 1. Aggregate stats per session
	// 2. Join back to get entry/exit URLs and first-seen dimensions
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
			agg.tenant_id,
			agg.site_id,
			agg.session_id,
			agg.first_ts,
			agg.last_ts,
			agg.pageviews,
			agg.events_count,
			COALESCE(entry.url, '') AS entry_url,
			COALESCE(exit_.url, '') AS exit_url,
			COALESCE(entry.referrer, '') AS referrer,
			COALESCE(entry.browser, '') AS browser,
			COALESCE(entry.os, '') AS os,
			COALESCE(entry.device, '') AS device,
			COALESCE(entry.country, '') AS country,
			COALESCE(entry.language, '') AS language,
			COALESCE(entry.screen_width, 0) AS screen_width,
			COALESCE(entry.screen_height, 0) AS screen_height,
			COALESCE(entry.utm_source, '') AS utm_source,
			COALESCE(entry.utm_medium, '') AS utm_medium,
			COALESCE(entry.utm_campaign, '') AS utm_campaign,
			agg.pageviews <= 1 AS is_bounce,
			$2 AS version
		FROM (
			SELECT
				tenant_id, site_id, session_id,
				MIN(timestamp) AS first_ts,
				MAX(timestamp) AS last_ts,
				COUNT(*) FILTER (WHERE event_type = 'pageview') AS pageviews,
				COUNT(*) AS events_count
			FROM events
			WHERE timestamp >= $1
			GROUP BY tenant_id, site_id, session_id
		) agg
		LEFT JOIN events entry
			ON entry.session_id = agg.session_id
			AND entry.site_id = agg.site_id
			AND entry.timestamp = agg.first_ts
		LEFT JOIN events exit_
			ON exit_.session_id = agg.session_id
			AND exit_.site_id = agg.site_id
			AND exit_.timestamp = agg.last_ts`,
		cutoff, version,
	)
	if err != nil {
		return fmt.Errorf("session rollup: %w", err)
	}

	r.logger.Debug("session rollup complete")
	return nil
}
