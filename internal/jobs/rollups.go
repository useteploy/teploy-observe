package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
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
			(CAST(timestamp AS BIGINT) / 3600000) * 3600000 AS ts_bucket,
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
		GROUP BY tenant_id, site_id, (CAST(timestamp AS BIGINT) / 3600000) * 3600000, pathname, event_type`),
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
	windowStart := now.Truncate(24 * time.Hour).Add(-48 * time.Hour)
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
			(CAST(timestamp AS BIGINT) / 86400000) * 86400000 AS ts_bucket,
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
		GROUP BY tenant_id, site_id, (CAST(timestamp AS BIGINT) / 86400000) * 86400000, pathname, event_type,
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

// sessionEvent is a raw event row used during session rollup.
type sessionEvent struct {
	TenantID     string `db:"tenant_id"`
	SiteID       string `db:"site_id"`
	SessionID    string `db:"session_id"`
	Timestamp    int64  `db:"timestamp"`
	Pathname     string `db:"pathname"`
	Referrer     string `db:"referrer"`
	Browser      string `db:"browser"`
	OS           string `db:"os"`
	Device       string `db:"device"`
	Country      string `db:"country"`
	Language     string `db:"language"`
	ScreenWidth  int64  `db:"screen_width"`
	ScreenHeight int64  `db:"screen_height"`
	UTMSource    string `db:"utm_source"`
	UTMMedium    string `db:"utm_medium"`
	UTMCampaign  string `db:"utm_campaign"`
	ReleaseTag   string `db:"release_tag"`
}

// RunSessionRollup aggregates events into session summaries.
// Runs every 5 minutes. Sessions table uses ReplacingMergeTree — overlapping
// re-computation is safe because the merge keeps the row with the highest
// version (most recent computation).
//
// Fetches raw events and computes entry/exit URLs, browser, OS, etc. in Go
// since Nucleus doesn't support window functions or correlated subqueries.
func (r *RollupService) RunSessionRollup(ctx context.Context) error {
	now := time.Now().UTC()
	cutoff := now.Add(-30 * time.Minute).UnixMilli()
	version := now.UnixMilli()

	sql := r.db.SQL()

	// Find which sessions received events in the window. We then recompute each
	// touched session over its FULL event history (not just the window), because
	// a session longer than the window would otherwise have its complete row
	// overwritten by a partial-tail computation under ReplacingMergeTree —
	// corrupting first_ts/entry_url/pageviews/is_bounce for long sessions.
	type sidRow struct {
		SessionID string `db:"session_id"`
	}
	sidRows, err := nucleus.Query[sidRow](ctx, sql,
		`SELECT DISTINCT session_id FROM events WHERE timestamp >= $1`, cutoff)
	if err != nil {
		return fmt.Errorf("session rollup keys: %w", err)
	}
	if len(sidRows) == 0 {
		r.logger.Debug("session rollup: no events")
		return nil
	}

	sids := make([]string, 0, len(sidRows))
	for _, s := range sidRows {
		if s.SessionID != "" {
			sids = append(sids, s.SessionID)
		}
	}

	const selectCols = `SELECT tenant_id, site_id, session_id, timestamp,
			COALESCE(pathname, '') AS pathname,
			COALESCE(referrer, '') AS referrer,
			COALESCE(browser, '') AS browser,
			COALESCE(os, '') AS os,
			COALESCE(device, '') AS device,
			COALESCE(country, '') AS country,
			COALESCE(language, '') AS language,
			COALESCE(screen_width, 0) AS screen_width,
			COALESCE(screen_height, 0) AS screen_height,
			COALESCE(utm_source, '') AS utm_source,
			COALESCE(utm_medium, '') AS utm_medium,
			COALESCE(utm_campaign, '') AS utm_campaign,
			COALESCE(release_tag, '') AS release_tag
		 FROM events`

	// Group events by session key (tenant+site+session). Re-read the complete
	// history for the touched sessions in bounded IN-list batches.
	type sessKey struct{ Tenant, Site, Session string }
	grouped := make(map[sessKey][]sessionEvent)
	const chunk = 500
	for i := 0; i < len(sids); i += chunk {
		end := i + chunk
		if end > len(sids) {
			end = len(sids)
		}
		batch := sids[i:end]
		ph := make([]string, len(batch))
		args := make([]any, len(batch))
		for j, s := range batch {
			ph[j] = fmt.Sprintf("$%d", j+1)
			args[j] = s
		}
		q := selectCols + ` WHERE session_id IN (` + strings.Join(ph, ",") + `) ORDER BY session_id, timestamp`
		rows, err := nucleus.Query[sessionEvent](ctx, sql, q, args...)
		if err != nil {
			return fmt.Errorf("session rollup query: %w", err)
		}
		for _, e := range rows {
			k := sessKey{e.TenantID, e.SiteID, e.SessionID}
			grouped[k] = append(grouped[k], e)
		}
	}

	const insertSQL = `INSERT INTO sessions (
		tenant_id, site_id, session_id,
		first_ts, last_ts, pageviews, events_count,
		entry_url, exit_url,
		referrer, browser, os, device, country, language,
		screen_width, screen_height,
		utm_source, utm_medium, utm_campaign,
		is_bounce, version, release_tag
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`

	inserted := 0
	for _, events := range grouped {
		sort.Slice(events, func(i, j int) bool {
			return events[i].Timestamp < events[j].Timestamp
		})
		first := events[0]
		last := events[len(events)-1]
		isBounce := "false"
		if len(events) <= 1 {
			isBounce = "true"
		}
		// Pick the first non-empty release_tag in the session — the SDK
		// includes it on every event but we only need one.
		releaseTag := first.ReleaseTag
		if releaseTag == "" {
			for _, e := range events {
				if e.ReleaseTag != "" {
					releaseTag = e.ReleaseTag
					break
				}
			}
		}

		_, err := sql.Exec(ctx, insertSQL,
			first.TenantID, first.SiteID, first.SessionID,
			first.Timestamp, last.Timestamp,
			len(events), len(events),
			first.Pathname, last.Pathname,
			first.Referrer, first.Browser, first.OS, first.Device,
			first.Country, first.Language,
			first.ScreenWidth, first.ScreenHeight,
			first.UTMSource, first.UTMMedium, first.UTMCampaign,
			isBounce, version, releaseTag,
		)
		if err != nil {
			return fmt.Errorf("session rollup insert: %w", err)
		}
		inserted++
	}

	r.logger.Debug("session rollup complete", "sessions", inserted)
	return nil
}
