package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
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
		WHERE timestamp >= CAST($1 AS BIGINT) AND timestamp < CAST($2 AS BIGINT)
		GROUP BY tenant_id, site_id, (CAST(timestamp AS BIGINT) / 3600000) * 3600000, pathname, event_type`),
		startMs, endMs, version,
	)
	if err != nil {
		return fmt.Errorf("hourly rollup: %w", err)
	}

	r.logger.Info("hourly rollup complete", "window_start", windowStart, "window_end", windowEnd)

	// Update HLL keys for approximate visitor counts
	if err := r.updateHLL(ctx, startMs, endMs, 3600000); err != nil {
		r.logger.Error("hourly HLL update failed", "err", err)
		// Non-fatal — SQL rollup succeeded
	}

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
		WHERE timestamp >= CAST($1 AS BIGINT) AND timestamp < CAST($2 AS BIGINT)
		GROUP BY tenant_id, site_id, (CAST(timestamp AS BIGINT) / 86400000) * 86400000, pathname, event_type,
		         referrer, browser, os, country, device,
		         utm_source, utm_medium, utm_campaign`),
		startMs, endMs, version,
	)
	if err != nil {
		return fmt.Errorf("daily rollup: %w", err)
	}

	r.logger.Info("daily rollup complete", "window_start", windowStart, "window_end", windowEnd)

	// Update HLL keys for approximate visitor counts (daily buckets)
	if err := r.updateHLL(ctx, startMs, endMs, 86400000); err != nil {
		r.logger.Error("daily HLL update failed", "err", err)
	}

	return nil
}

// hllEvent is a minimal event row for HLL visitor counting.
type hllEvent struct {
	SiteID    string `db:"site_id"`
	SessionID string `db:"session_id"`
	Bucket    int64  `db:"bucket"`
}

// updateHLL feeds session_ids into HyperLogLog keys per site+time bucket.
// Key format: hll:visitors:{site_id}:{bucket_ms}
// This provides O(1) approximate visitor counts that scale to any volume.
func (r *RollupService) updateHLL(ctx context.Context, startMs, endMs int64, bucketMs int64) error {
	rows, err := nucleus.Query[hllEvent](ctx, r.db.SQL(),
		fmt.Sprintf(`SELECT site_id, session_id,
			(CAST(timestamp AS BIGINT) / %d) * %d AS bucket
		 FROM events
		 WHERE timestamp >= CAST($1 AS BIGINT) AND timestamp < CAST($2 AS BIGINT)`,
			bucketMs, bucketMs),
		startMs, endMs,
	)
	if err != nil {
		return fmt.Errorf("hll query: %w", err)
	}

	kv := r.db.KV()
	added := 0
	for _, row := range rows {
		key := fmt.Sprintf("hll:visitors:%s:%d", row.SiteID, row.Bucket)
		if _, err := kv.PFAdd(ctx, key, row.SessionID); err != nil {
			return fmt.Errorf("hll pfadd: %w", err)
		}
		added++
	}

	r.logger.Debug("HLL update complete", "events", added, "bucket_size", bucketMs)
	return nil
}

// HLLVisitors returns the approximate unique visitor count for a site
// over the given time range using HyperLogLog. Falls back to -1 if
// no HLL data exists (caller should use SQL COUNT DISTINCT).
func (r *RollupService) HLLVisitors(ctx context.Context, siteID string, fromMs, toMs int64, bucketMs int64) (int64, error) {
	kv := r.db.KV()
	var total int64
	hasData := false

	for bucket := (fromMs / bucketMs) * bucketMs; bucket < toMs; bucket += bucketMs {
		key := fmt.Sprintf("hll:visitors:%s:%d", siteID, bucket)
		count, err := kv.PFCount(ctx, key)
		if err != nil {
			continue
		}
		if count > 0 {
			hasData = true
			total += count
		}
	}

	if !hasData {
		return -1, nil
	}
	return total, nil
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

	rows, err := nucleus.Query[sessionEvent](ctx, sql,
		`SELECT tenant_id, site_id, session_id, timestamp,
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
		 FROM events
		 WHERE timestamp >= CAST($1 AS BIGINT)
		 ORDER BY session_id, timestamp`,
		cutoff,
	)
	if err != nil {
		return fmt.Errorf("session rollup query: %w", err)
	}

	if len(rows) == 0 {
		r.logger.Debug("session rollup: no events")
		return nil
	}

	// Group events by session key (tenant+site+session)
	type sessKey struct{ Tenant, Site, Session string }
	grouped := make(map[sessKey][]sessionEvent)
	for _, e := range rows {
		k := sessKey{e.TenantID, e.SiteID, e.SessionID}
		grouped[k] = append(grouped[k], e)
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
