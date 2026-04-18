package query

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// StatsService provides analytics query methods for the dashboard.
type StatsService struct {
	db *nucleus.Client
}

func NewStatsService(db *nucleus.Client) *StatsService {
	return &StatsService{db: db}
}

// tableFor picks the optimal source table based on query time range.
//
//	< 24h  -> events (raw, freshest data)
//	24h-7d -> stats_hourly (pre-aggregated)
//	> 7d   -> stats_daily (coarsest, fastest)
func tableFor(from, to time.Time) string {
	dur := to.Sub(from)
	switch {
	case dur <= 24*time.Hour:
		return "events"
	case dur <= 7*24*time.Hour:
		return "stats_hourly"
	default:
		return "stats_daily"
	}
}

// tsColumn returns the timestamp column name for the given table.
func tsColumn(table string) string {
	if table == "events" {
		return "timestamp"
	}
	return "ts_bucket"
}

// filterSQL returns the SQL fragment and params for a FilterBuilder, or empty
// values if the builder is nil or has no filters.
func filterSQL(filters *FilterBuilder) (string, []any) {
	if filters == nil {
		return "", nil
	}
	return filters.SQL(), filters.Params()
}

// baseParams builds the standard parameter list: siteID, fromMs, toMs, plus any filter params.
func baseParams(siteID string, fromMs, toMs string, filters *FilterBuilder) []any {
	params := []any{siteID, fromMs, toMs}
	if filters != nil {
		params = append(params, filters.Params()...)
	}
	return params
}

// RealtimeResult holds the count of active visitors.
type RealtimeResult struct {
	ActiveVisitors int `json:"active_visitors" db:"active_visitors"`
}

func (s *StatsService) RealtimeVisitors(ctx context.Context, siteID string, minutes int) (int, error) {
	cutoff := dbutil.IntParam(time.Now().UTC().Add(-time.Duration(minutes) * time.Minute).UnixMilli())
	rows, err := nucleus.Query[RealtimeResult](ctx, s.db.SQL(),
		`SELECT COUNT(DISTINCT session_id) AS active_visitors
		 FROM events_recent
		 WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT)`,
		siteID, cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("realtime visitors: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rows[0].ActiveVisitors, nil
}

// TimeSeriesPoint is a single data point for time-series charts.
type TimeSeriesPoint struct {
	Bucket    int64 `json:"bucket" db:"bucket"`
	Pageviews int64 `json:"pageviews" db:"pageviews"`
	Visitors  int64 `json:"visitors" db:"visitors"`
}

func (s *StatsService) PageviewTimeSeries(ctx context.Context, siteID string, from, to time.Time, interval string, filters *FilterBuilder) ([]TimeSeriesPoint, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())
	table := tableFor(from, to)
	ts := tsColumn(table)
	fSQL, _ := filterSQL(filters)
	allParams := baseParams(siteID, fromMs, toMs, filters)

	var q string
	if table == "events" {
		bucketMs := int64(3600000) // default: hour
		switch interval {
		case "day":
			bucketMs = 86400000
		case "week":
			bucketMs = 604800000
		case "month":
			bucketMs = 86400000 * 30
		}
		q = fmt.Sprintf(`SELECT (CAST(timestamp AS BIGINT) / %d) * %d AS bucket,
		        COUNT(*) AS pageviews,
		        COUNT(DISTINCT session_id) AS visitors
		 FROM events
		 WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT)
		   AND event_type = 'pageview'%s
		 GROUP BY (CAST(timestamp AS BIGINT) / %d) * %d
		 ORDER BY (CAST(timestamp AS BIGINT) / %d) * %d`, bucketMs, bucketMs, fSQL, bucketMs, bucketMs, bucketMs, bucketMs)
	} else {
		q = fmt.Sprintf(`SELECT %s AS bucket,
		        SUM(pageviews) AS pageviews,
		        SUM(visitors) AS visitors
		 FROM %s
		 WHERE site_id = $1 AND %s >= CAST($2 AS BIGINT) AND %s < CAST($3 AS BIGINT)
		   AND event_type = 'pageview'%s
		 GROUP BY %s
		 ORDER BY %s`, ts, table, ts, ts, fSQL, ts, ts)
	}

	rows, err := nucleus.Query[TimeSeriesPoint](ctx, s.db.SQL(), q, allParams...)
	if err != nil {
		return nil, fmt.Errorf("pageview timeseries: %w", err)
	}
	return rows, nil
}

// TopPage represents a page with its view count.
type TopPage struct {
	Pathname  string `json:"pathname" db:"pathname"`
	Pageviews int64  `json:"pageviews" db:"pageviews"`
	Visitors  int64  `json:"visitors" db:"visitors"`
}

func (s *StatsService) TopPages(ctx context.Context, siteID string, from, to time.Time, limit int, filters *FilterBuilder) ([]TopPage, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())
	if limit <= 0 {
		limit = 10
	}
	table := tableFor(from, to)
	ts := tsColumn(table)
	fSQL, _ := filterSQL(filters)
	allParams := baseParams(siteID, fromMs, toMs, filters)

	var q string
	if table == "events" {
		q = fmt.Sprintf(`SELECT pathname,
		        COUNT(*) AS pageviews,
		        COUNT(DISTINCT session_id) AS visitors
		 FROM events
		 WHERE site_id = $1 AND %s >= CAST($2 AS BIGINT) AND %s < CAST($3 AS BIGINT)
		   AND event_type = 'pageview'%s
		 GROUP BY pathname
		 ORDER BY pageviews DESC
		 LIMIT %d`, ts, ts, fSQL, limit)
	} else {
		q = fmt.Sprintf(`SELECT pathname,
		        SUM(pageviews) AS pageviews,
		        SUM(visitors) AS visitors
		 FROM %s
		 WHERE site_id = $1 AND %s >= CAST($2 AS BIGINT) AND %s < CAST($3 AS BIGINT)
		   AND event_type = 'pageview'%s
		 GROUP BY pathname
		 ORDER BY pageviews DESC
		 LIMIT %d`, table, ts, ts, fSQL, limit)
	}

	rows, err := nucleus.Query[TopPage](ctx, s.db.SQL(), q, allParams...)
	if err != nil {
		return nil, fmt.Errorf("top pages: %w", err)
	}
	return rows, nil
}

// TopReferrer represents a referrer source with its count.
type TopReferrer struct {
	Referrer string `json:"referrer" db:"referrer"`
	Visitors int64  `json:"visitors" db:"visitors"`
}

func (s *StatsService) TopReferrers(ctx context.Context, siteID string, from, to time.Time, limit int, filters *FilterBuilder) ([]TopReferrer, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())
	if limit <= 0 {
		limit = 10
	}
	table := tableFor(from, to)
	ts := tsColumn(table)
	fSQL, _ := filterSQL(filters)
	allParams := baseParams(siteID, fromMs, toMs, filters)

	var q string
	if table == "events" {
		q = fmt.Sprintf(`SELECT referrer,
		        COUNT(DISTINCT session_id) AS visitors
		 FROM events
		 WHERE site_id = $1 AND %s >= CAST($2 AS BIGINT) AND %s < CAST($3 AS BIGINT)
		   AND referrer != '' AND event_type = 'pageview'%s
		 GROUP BY referrer
		 ORDER BY visitors DESC
		 LIMIT %d`, ts, ts, fSQL, limit)
	} else {
		// stats_daily has referrer; stats_hourly doesn't — fall back to events
		if table == "stats_hourly" {
			q = fmt.Sprintf(`SELECT referrer,
			        COUNT(DISTINCT session_id) AS visitors
			 FROM events
			 WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT)
			   AND referrer != '' AND event_type = 'pageview'%s
			 GROUP BY referrer
			 ORDER BY visitors DESC
			 LIMIT %d`, fSQL, limit)
		} else {
			q = fmt.Sprintf(`SELECT referrer,
			        SUM(visitors) AS visitors
			 FROM stats_daily
			 WHERE site_id = $1 AND %s >= CAST($2 AS BIGINT) AND %s < CAST($3 AS BIGINT)
			   AND referrer != '' AND event_type = 'pageview'%s
			 GROUP BY referrer
			 ORDER BY visitors DESC
			 LIMIT %d`, ts, ts, fSQL, limit)
		}
	}

	rows, err := nucleus.Query[TopReferrer](ctx, s.db.SQL(), q, allParams...)
	if err != nil {
		return nil, fmt.Errorf("top referrers: %w", err)
	}
	return rows, nil
}

// BrowserStat represents a browser breakdown entry.
type BrowserStat struct {
	Browser  string `json:"browser" db:"browser"`
	Visitors int64  `json:"visitors" db:"visitors"`
}

func (s *StatsService) TopBrowsers(ctx context.Context, siteID string, from, to time.Time, limit int, filters *FilterBuilder) ([]BrowserStat, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())
	if limit <= 0 {
		limit = 10
	}
	table := tableFor(from, to)
	ts := tsColumn(table)
	fSQL, _ := filterSQL(filters)
	allParams := baseParams(siteID, fromMs, toMs, filters)

	var q string
	if table == "stats_daily" {
		q = fmt.Sprintf(`SELECT browser,
		        SUM(visitors) AS visitors
		 FROM stats_daily
		 WHERE site_id = $1 AND %s >= CAST($2 AS BIGINT) AND %s < CAST($3 AS BIGINT)
		   AND browser != ''%s
		 GROUP BY browser
		 ORDER BY visitors DESC
		 LIMIT %d`, ts, ts, fSQL, limit)
	} else {
		// events or stats_hourly (hourly doesn't have browser — use events)
		q = fmt.Sprintf(`SELECT browser,
		        COUNT(DISTINCT session_id) AS visitors
		 FROM events
		 WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT)
		   AND browser != ''%s
		 GROUP BY browser
		 ORDER BY visitors DESC
		 LIMIT %d`, fSQL, limit)
	}

	rows, err := nucleus.Query[BrowserStat](ctx, s.db.SQL(), q, allParams...)
	if err != nil {
		return nil, fmt.Errorf("top browsers: %w", err)
	}
	return rows, nil
}

// CountryStat represents a country breakdown entry.
type CountryStat struct {
	Country  string `json:"country" db:"country"`
	Visitors int64  `json:"visitors" db:"visitors"`
}

func (s *StatsService) TopCountries(ctx context.Context, siteID string, from, to time.Time, limit int, filters *FilterBuilder) ([]CountryStat, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())
	if limit <= 0 {
		limit = 10
	}
	table := tableFor(from, to)
	ts := tsColumn(table)
	fSQL, _ := filterSQL(filters)
	allParams := baseParams(siteID, fromMs, toMs, filters)

	var q string
	if table == "stats_daily" {
		q = fmt.Sprintf(`SELECT country,
		        SUM(visitors) AS visitors
		 FROM stats_daily
		 WHERE site_id = $1 AND %s >= CAST($2 AS BIGINT) AND %s < CAST($3 AS BIGINT)
		   AND country != ''%s
		 GROUP BY country
		 ORDER BY visitors DESC
		 LIMIT %d`, ts, ts, fSQL, limit)
	} else {
		q = fmt.Sprintf(`SELECT country,
		        COUNT(DISTINCT session_id) AS visitors
		 FROM events
		 WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT)
		   AND country != ''%s
		 GROUP BY country
		 ORDER BY visitors DESC
		 LIMIT %d`, fSQL, limit)
	}

	rows, err := nucleus.Query[CountryStat](ctx, s.db.SQL(), q, allParams...)
	if err != nil {
		return nil, fmt.Errorf("top countries: %w", err)
	}
	return rows, nil
}

// OSStat represents an OS breakdown entry.
type OSStat struct {
	OS       string `json:"os" db:"os"`
	Visitors int64  `json:"visitors" db:"visitors"`
}

func (s *StatsService) TopOS(ctx context.Context, siteID string, from, to time.Time, limit int, filters *FilterBuilder) ([]OSStat, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())
	if limit <= 0 {
		limit = 10
	}
	table := tableFor(from, to)
	ts := tsColumn(table)
	fSQL, _ := filterSQL(filters)
	allParams := baseParams(siteID, fromMs, toMs, filters)

	var q string
	if table == "stats_daily" {
		q = fmt.Sprintf(`SELECT os,
		        SUM(visitors) AS visitors
		 FROM stats_daily
		 WHERE site_id = $1 AND %s >= CAST($2 AS BIGINT) AND %s < CAST($3 AS BIGINT)
		   AND os != ''%s
		 GROUP BY os
		 ORDER BY visitors DESC
		 LIMIT %d`, ts, ts, fSQL, limit)
	} else {
		q = fmt.Sprintf(`SELECT os,
		        COUNT(DISTINCT session_id) AS visitors
		 FROM events
		 WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT)
		   AND os != ''%s
		 GROUP BY os
		 ORDER BY visitors DESC
		 LIMIT %d`, fSQL, limit)
	}

	rows, err := nucleus.Query[OSStat](ctx, s.db.SQL(), q, allParams...)
	if err != nil {
		return nil, fmt.Errorf("top os: %w", err)
	}
	return rows, nil
}

// DeviceStat represents a device type breakdown entry.
type DeviceStat struct {
	Device   string `json:"device" db:"device"`
	Visitors int64  `json:"visitors" db:"visitors"`
}

func (s *StatsService) TopDevices(ctx context.Context, siteID string, from, to time.Time, limit int, filters *FilterBuilder) ([]DeviceStat, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())
	if limit <= 0 {
		limit = 10
	}
	table := tableFor(from, to)
	ts := tsColumn(table)
	fSQL, _ := filterSQL(filters)
	allParams := baseParams(siteID, fromMs, toMs, filters)

	var q string
	if table == "stats_daily" {
		q = fmt.Sprintf(`SELECT device,
		        SUM(visitors) AS visitors
		 FROM stats_daily
		 WHERE site_id = $1 AND %s >= CAST($2 AS BIGINT) AND %s < CAST($3 AS BIGINT)
		   AND device != ''%s
		 GROUP BY device
		 ORDER BY visitors DESC
		 LIMIT %d`, ts, ts, fSQL, limit)
	} else {
		q = fmt.Sprintf(`SELECT device,
		        COUNT(DISTINCT session_id) AS visitors
		 FROM events
		 WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT)
		   AND device != ''%s
		 GROUP BY device
		 ORDER BY visitors DESC
		 LIMIT %d`, fSQL, limit)
	}

	rows, err := nucleus.Query[DeviceStat](ctx, s.db.SQL(), q, allParams...)
	if err != nil {
		return nil, fmt.Errorf("top devices: %w", err)
	}
	return rows, nil
}

// ChannelStat represents a traffic channel breakdown entry.
type ChannelStat struct {
	Channel  string `json:"channel"`
	Visitors int64  `json:"visitors"`
}

// channelRow is the raw row from the DB before channel classification.
type channelRow struct {
	Referrer  string `db:"referrer"`
	UTMSource string `db:"utm_source"`
	UTMMedium string `db:"utm_medium"`
	SessionID string `db:"session_id"`
}

func (s *StatsService) TopChannels(ctx context.Context, siteID string, from, to time.Time, filters *FilterBuilder) ([]ChannelStat, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())
	fSQL, _ := filterSQL(filters)
	allParams := baseParams(siteID, fromMs, toMs, filters)

	q := fmt.Sprintf(`SELECT DISTINCT session_id,
		        COALESCE(referrer, '') AS referrer,
		        COALESCE(utm_source, '') AS utm_source,
		        COALESCE(utm_medium, '') AS utm_medium
		 FROM events
		 WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT)%s`, fSQL)

	rows, err := nucleus.Query[channelRow](ctx, s.db.SQL(), q, allParams...)
	if err != nil {
		return nil, fmt.Errorf("channels: %w", err)
	}

	// Classify each session and aggregate
	counts := make(map[string]int64)
	for _, r := range rows {
		ch := ClassifyChannel(r.Referrer, r.UTMSource, r.UTMMedium)
		counts[ch]++
	}

	// Sort by count descending
	result := make([]ChannelStat, 0, len(counts))
	for ch, n := range counts {
		result = append(result, ChannelStat{Channel: ch, Visitors: n})
	}
	// Simple insertion sort — always <10 channels
	for i := 1; i < len(result); i++ {
		for j := i; j > 0 && result[j].Visitors > result[j-1].Visitors; j-- {
			result[j], result[j-1] = result[j-1], result[j]
		}
	}

	return result, nil
}

// OverviewStats provides summary stats for the dashboard header.
type OverviewStats struct {
	Pageviews   int64   `json:"pageviews" db:"pageviews"`
	Visitors    int64   `json:"visitors" db:"visitors"`
	Sessions    int64   `json:"sessions" db:"sessions"`
	BounceRate  float64 `json:"bounce_rate"`
	AvgDuration float64 `json:"avg_duration"`
}

// OverviewWithCompare wraps current and previous period overview stats.
type OverviewWithCompare struct {
	Current  OverviewStats  `json:"current"`
	Previous *OverviewStats `json:"previous,omitempty"`
}

// sessionStats is used to scan bounce rate and duration from the sessions table.
type sessionStats struct {
	Bounces       int64 `db:"bounces"`
	TotalSessions int64 `db:"total_sessions"`
	DurationSum   int64 `db:"duration_sum"`
}

func (s *StatsService) Overview(ctx context.Context, siteID string, from, to time.Time, filters *FilterBuilder) (OverviewStats, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())
	table := tableFor(from, to)
	ts := tsColumn(table)
	fSQL, _ := filterSQL(filters)
	allParams := baseParams(siteID, fromMs, toMs, filters)

	type rawStats struct {
		Pageviews int64 `db:"pageviews"`
		Visitors  int64 `db:"visitors"`
		Sessions  int64 `db:"sessions"`
	}

	var q string
	if table == "events" {
		q = fmt.Sprintf(`SELECT COUNT(*) AS pageviews,
		        COUNT(DISTINCT session_id) AS visitors,
		        COUNT(DISTINCT visit_id) AS sessions
		 FROM events
		 WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT)
		   AND event_type = 'pageview'%s`, fSQL)
	} else {
		q = fmt.Sprintf(`SELECT SUM(pageviews) AS pageviews,
		        SUM(visitors) AS visitors,
		        SUM(sessions) AS sessions
		 FROM %s
		 WHERE site_id = $1 AND %s >= CAST($2 AS BIGINT) AND %s < CAST($3 AS BIGINT)
		   AND event_type = 'pageview'%s`, table, ts, ts, fSQL)
	}

	rows, err := nucleus.Query[rawStats](ctx, s.db.SQL(), q, allParams...)
	if err != nil {
		return OverviewStats{}, fmt.Errorf("overview: %w", err)
	}

	var result OverviewStats
	if len(rows) > 0 {
		result.Pageviews = rows[0].Pageviews
		result.Visitors = rows[0].Visitors
		result.Sessions = rows[0].Sessions
	}

	// Query sessions table for bounce rate and average duration.
	// Sessions table uses first_ts for time filtering.
	sessParams := baseParams(siteID, fromMs, toMs, filters)
	sessQ := fmt.Sprintf(`SELECT
		        SUM(CASE WHEN is_bounce = 'true' THEN 1 ELSE 0 END) AS bounces,
		        COUNT(*) AS total_sessions,
		        SUM(CAST(last_ts AS BIGINT) - CAST(first_ts AS BIGINT)) AS duration_sum
		 FROM sessions
		 WHERE site_id = $1 AND first_ts >= CAST($2 AS BIGINT) AND first_ts < CAST($3 AS BIGINT)%s`, fSQL)

	sessRows, err := nucleus.Query[sessionStats](ctx, s.db.SQL(), sessQ, sessParams...)
	if err != nil {
		// Non-fatal: return what we have without bounce/duration
		return result, nil
	}
	if len(sessRows) > 0 && sessRows[0].TotalSessions > 0 {
		result.BounceRate = float64(sessRows[0].Bounces) / float64(sessRows[0].TotalSessions) * 100
		result.AvgDuration = float64(sessRows[0].DurationSum) / float64(sessRows[0].TotalSessions) / 1000.0
	}

	return result, nil
}

// OverviewWithComparison returns current stats alongside a previous period for comparison.
func (s *StatsService) OverviewWithComparison(ctx context.Context, siteID string, from, to time.Time, compare string, filters *FilterBuilder) (OverviewWithCompare, error) {
	current, err := s.Overview(ctx, siteID, from, to, filters)
	if err != nil {
		return OverviewWithCompare{}, err
	}

	dur := to.Sub(from)
	var prevFrom, prevTo time.Time
	if compare == "previous_year" {
		prevFrom = from.AddDate(-1, 0, 0)
		prevTo = to.AddDate(-1, 0, 0)
	} else {
		// default: previous_period
		prevFrom = from.Add(-dur)
		prevTo = from
	}

	previous, _ := s.Overview(ctx, siteID, prevFrom, prevTo, filters)
	return OverviewWithCompare{
		Current:  current,
		Previous: &previous,
	}, nil
}

// LanguageStat represents a language breakdown entry.
type LanguageStat struct {
	Language string `json:"language" db:"language"`
	Visitors int64  `json:"visitors" db:"visitors"`
}

func (s *StatsService) TopLanguages(ctx context.Context, siteID string, from, to time.Time, limit int, filters *FilterBuilder) ([]LanguageStat, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())
	if limit <= 0 {
		limit = 10
	}
	fSQL, _ := filterSQL(filters)
	allParams := baseParams(siteID, fromMs, toMs, filters)

	q := fmt.Sprintf(`SELECT language,
	        COUNT(DISTINCT session_id) AS visitors
	 FROM events
	 WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT)
	   AND language != ''%s
	 GROUP BY language
	 ORDER BY visitors DESC
	 LIMIT %d`, fSQL, limit)

	rows, err := nucleus.Query[LanguageStat](ctx, s.db.SQL(), q, allParams...)
	if err != nil {
		return nil, fmt.Errorf("top languages: %w", err)
	}
	return rows, nil
}

// ScreenStat represents a screen resolution breakdown entry.
type ScreenStat struct {
	Screen   string `json:"screen" db:"screen"`
	Visitors int64  `json:"visitors" db:"visitors"`
}

func (s *StatsService) TopScreens(ctx context.Context, siteID string, from, to time.Time, limit int, filters *FilterBuilder) ([]ScreenStat, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())
	if limit <= 0 {
		limit = 10
	}
	fSQL, _ := filterSQL(filters)
	allParams := baseParams(siteID, fromMs, toMs, filters)

	q := fmt.Sprintf(`SELECT CAST(screen_width AS TEXT) || 'x' || CAST(screen_height AS TEXT) AS screen,
	        COUNT(DISTINCT session_id) AS visitors
	 FROM events
	 WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT)
	   AND CAST(screen_width AS INTEGER) > 0%s
	 GROUP BY CAST(screen_width AS TEXT) || 'x' || CAST(screen_height AS TEXT)
	 ORDER BY visitors DESC
	 LIMIT %d`, fSQL, limit)

	rows, err := nucleus.Query[ScreenStat](ctx, s.db.SQL(), q, allParams...)
	if err != nil {
		return nil, fmt.Errorf("top screens: %w", err)
	}
	return rows, nil
}

// UTMStat represents a UTM parameter breakdown entry.
type UTMStat struct {
	Value    string `json:"value" db:"value"`
	Visitors int64  `json:"visitors" db:"visitors"`
}

func (s *StatsService) TopUTM(ctx context.Context, siteID string, from, to time.Time, utmType string, limit int, filters *FilterBuilder) ([]UTMStat, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())
	if limit <= 0 {
		limit = 10
	}
	fSQL, _ := filterSQL(filters)
	allParams := baseParams(siteID, fromMs, toMs, filters)

	col := "utm_source"
	switch utmType {
	case "medium":
		col = "utm_medium"
	case "campaign":
		col = "utm_campaign"
	}

	q := fmt.Sprintf(`SELECT %s AS value,
	        COUNT(DISTINCT session_id) AS visitors
	 FROM events
	 WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT)
	   AND %s != ''%s
	 GROUP BY %s
	 ORDER BY visitors DESC
	 LIMIT %d`, col, col, fSQL, col, limit)

	rows, err := nucleus.Query[UTMStat](ctx, s.db.SQL(), q, allParams...)
	if err != nil {
		return nil, fmt.Errorf("top utm: %w", err)
	}
	return rows, nil
}

// EntryPageStat represents an entry page breakdown entry.
type EntryPageStat struct {
	Pathname string `json:"pathname" db:"pathname"`
	Visitors int64  `json:"visitors" db:"visitors"`
}

func (s *StatsService) TopEntryPages(ctx context.Context, siteID string, from, to time.Time, limit int, filters *FilterBuilder) ([]EntryPageStat, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())
	if limit <= 0 {
		limit = 10
	}
	fSQL, _ := filterSQL(filters)
	allParams := baseParams(siteID, fromMs, toMs, filters)

	q := fmt.Sprintf(`SELECT entry_url AS pathname,
	        COUNT(*) AS visitors
	 FROM sessions
	 WHERE site_id = $1 AND first_ts >= CAST($2 AS BIGINT) AND first_ts < CAST($3 AS BIGINT)
	   AND entry_url != ''%s
	 GROUP BY entry_url
	 ORDER BY visitors DESC
	 LIMIT %d`, fSQL, limit)

	rows, err := nucleus.Query[EntryPageStat](ctx, s.db.SQL(), q, allParams...)
	if err != nil {
		return nil, fmt.Errorf("top entry pages: %w", err)
	}
	return rows, nil
}

// ExitPageStat represents an exit page breakdown entry.
type ExitPageStat struct {
	Pathname string `json:"pathname" db:"pathname"`
	Visitors int64  `json:"visitors" db:"visitors"`
}

func (s *StatsService) TopExitPages(ctx context.Context, siteID string, from, to time.Time, limit int, filters *FilterBuilder) ([]ExitPageStat, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())
	if limit <= 0 {
		limit = 10
	}
	fSQL, _ := filterSQL(filters)
	allParams := baseParams(siteID, fromMs, toMs, filters)

	q := fmt.Sprintf(`SELECT exit_url AS pathname,
	        COUNT(*) AS visitors
	 FROM sessions
	 WHERE site_id = $1 AND first_ts >= CAST($2 AS BIGINT) AND first_ts < CAST($3 AS BIGINT)
	   AND exit_url != ''%s
	 GROUP BY exit_url
	 ORDER BY visitors DESC
	 LIMIT %d`, fSQL, limit)

	rows, err := nucleus.Query[ExitPageStat](ctx, s.db.SQL(), q, allParams...)
	if err != nil {
		return nil, fmt.Errorf("top exit pages: %w", err)
	}
	return rows, nil
}

// CustomEventStat represents a custom event type breakdown entry.
type CustomEventStat struct {
	EventType string `json:"event_type" db:"event_type"`
	Count     int64  `json:"count" db:"count"`
	Visitors  int64  `json:"visitors" db:"visitors"`
}

func (s *StatsService) CustomEvents(ctx context.Context, siteID string, from, to time.Time, limit int, filters *FilterBuilder) ([]CustomEventStat, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())
	if limit <= 0 {
		limit = 10
	}
	fSQL, _ := filterSQL(filters)
	allParams := baseParams(siteID, fromMs, toMs, filters)

	q := fmt.Sprintf(`SELECT event_type,
	        COUNT(*) AS count,
	        COUNT(DISTINCT session_id) AS visitors
	 FROM events
	 WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT)
	   AND event_type != 'pageview'%s
	 GROUP BY event_type
	 ORDER BY count DESC
	 LIMIT %d`, fSQL, limit)

	rows, err := nucleus.Query[CustomEventStat](ctx, s.db.SQL(), q, allParams...)
	if err != nil {
		return nil, fmt.Errorf("custom events: %w", err)
	}
	return rows, nil
}

// PropertyStat represents a single property key/value count.
type PropertyStat struct {
	Key      string `json:"key"`
	Value    string `json:"value"`
	Count    int64  `json:"count"`
	Visitors int64  `json:"visitors"`
}

// EventProperties returns property key→value breakdowns for a specific event type.
// Aggregated in Go because Nucleus doesn't support jsonb_each or similar.
func (s *StatsService) EventProperties(ctx context.Context, siteID string, from, to time.Time, eventType string, limit int) ([]PropertyStat, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())
	if limit <= 0 {
		limit = 20
	}

	type raw struct {
		Properties string `json:"properties" db:"properties"`
		SessionID  string `json:"session_id" db:"session_id"`
	}
	rows, err := nucleus.Query[raw](ctx, s.db.SQL(),
		`SELECT COALESCE(properties, '') AS properties, session_id
		 FROM events
		 WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT)
		   AND event_type = $4 AND properties != ''
		 LIMIT 5000`,
		siteID, fromMs, toMs, eventType,
	)
	if err != nil {
		return nil, fmt.Errorf("event properties: %w", err)
	}

	type key struct{ k, v string }
	counts := make(map[key]int64)
	visitors := make(map[key]map[string]bool)

	for _, r := range rows {
		if r.Properties == "" || r.Properties == "{}" {
			continue
		}
		var props map[string]any
		if err := json.Unmarshal([]byte(r.Properties), &props); err != nil {
			continue
		}
		for k, v := range props {
			vStr := fmt.Sprintf("%v", v)
			kv := key{k, vStr}
			counts[kv]++
			if visitors[kv] == nil {
				visitors[kv] = make(map[string]bool)
			}
			visitors[kv][r.SessionID] = true
		}
	}

	result := make([]PropertyStat, 0, len(counts))
	for kv, c := range counts {
		result = append(result, PropertyStat{
			Key: kv.k, Value: kv.v, Count: c, Visitors: int64(len(visitors[kv])),
		})
	}

	// Sort by count desc, truncate
	for i := 0; i < len(result); i++ {
		for j := i + 1; j < len(result); j++ {
			if result[j].Count > result[i].Count {
				result[i], result[j] = result[j], result[i]
			}
		}
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

// ============================================================================
// Session browser
// ============================================================================

// SessionSummary represents a session list entry.
type SessionSummary struct {
	SessionID string `json:"session_id" db:"session_id"`
	FirstTS   int64  `json:"first_ts" db:"first_ts"`
	LastTS    int64  `json:"last_ts" db:"last_ts"`
	Pageviews int64  `json:"pageviews" db:"pageviews"`
	EntryURL  string `json:"entry_url" db:"entry_url"`
	ExitURL   string `json:"exit_url" db:"exit_url"`
	Browser   string `json:"browser" db:"browser"`
	OS        string `json:"os" db:"os"`
	Country   string `json:"country" db:"country"`
	Device    string `json:"device" db:"device"`
	IsBounce  string `json:"is_bounce" db:"is_bounce"`
	Duration  int64  `json:"duration"`
}

func (s *StatsService) Sessions(ctx context.Context, siteID string, from, to time.Time, limit int) ([]SessionSummary, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())
	if limit <= 0 {
		limit = 20
	}

	q := fmt.Sprintf(`SELECT session_id,
	        first_ts AS first_ts,
	        last_ts AS last_ts,
	        pageviews AS pageviews,
	        entry_url, exit_url,
	        browser, os, country, device, is_bounce
	 FROM sessions
	 WHERE site_id = $1 AND first_ts >= CAST($2 AS BIGINT) AND first_ts < CAST($3 AS BIGINT)
	 ORDER BY first_ts DESC
	 LIMIT %d`, limit)

	rows, err := nucleus.Query[SessionSummary](ctx, s.db.SQL(), q, siteID, fromMs, toMs)
	if err != nil {
		return nil, fmt.Errorf("sessions: %w", err)
	}
	for i := range rows {
		rows[i].Duration = rows[i].LastTS - rows[i].FirstTS
	}
	return rows, nil
}

// SessionEvent represents a single event within a session timeline.
type SessionEvent struct {
	EventID   string `json:"event_id" db:"event_id"`
	EventType string `json:"event_type" db:"event_type"`
	URL       string `json:"url" db:"url"`
	Pathname  string `json:"pathname" db:"pathname"`
	Title     string `json:"title" db:"title"`
	Timestamp int64  `json:"timestamp" db:"timestamp"`
}

func (s *StatsService) SessionDetail(ctx context.Context, sessionID, siteID string) ([]SessionEvent, error) {
	rows, err := nucleus.Query[SessionEvent](ctx, s.db.SQL(),
		`SELECT event_id, event_type, url, pathname, title, timestamp
		 FROM events
		 WHERE session_id = $1 AND site_id = $2
		 ORDER BY timestamp ASC`,
		sessionID, siteID,
	)
	if err != nil {
		return nil, fmt.Errorf("session detail: %w", err)
	}
	return rows, nil
}

// ============================================================================
// Custom event property drill-down
// ============================================================================

// validPropertyKey matches safe alphanumeric + underscore property keys.
var validPropertyKey = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// PropertyKeyStat represents a property key with its occurrence count.
type PropertyKeyStat struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

func (s *StatsService) EventPropertyKeys(ctx context.Context, siteID, eventName string, from, to time.Time) ([]PropertyKeyStat, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	type eventRow struct {
		Properties string `db:"properties"`
	}

	rows, err := nucleus.Query[eventRow](ctx, s.db.SQL(),
		`SELECT properties FROM events
		 WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT)
		   AND event_type = $4 AND properties IS NOT NULL`,
		siteID, fromMs, toMs, eventName,
	)
	if err != nil {
		return nil, fmt.Errorf("event property keys: %w", err)
	}

	keyCounts := make(map[string]int64)
	for _, r := range rows {
		if r.Properties == "" {
			continue
		}
		var props map[string]any
		if err := json.Unmarshal([]byte(r.Properties), &props); err != nil {
			continue
		}
		for k := range props {
			keyCounts[k]++
		}
	}

	result := make([]PropertyKeyStat, 0, len(keyCounts))
	for k, c := range keyCounts {
		result = append(result, PropertyKeyStat{Key: k, Count: c})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Count > result[j].Count
	})
	return result, nil
}

// PropertyValueStat represents a property value with its occurrence count.
type PropertyValueStat struct {
	Value string `json:"value" db:"value"`
	Count int64  `json:"count" db:"count"`
}

func (s *StatsService) EventPropertyValues(ctx context.Context, siteID, eventName, propKey string, from, to time.Time) ([]PropertyValueStat, error) {
	if !validPropertyKey.MatchString(propKey) {
		return nil, fmt.Errorf("event property values: invalid property key")
	}

	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	// Inline propKey via Sprintf after validation (Nucleus SimpleProtocol
	// may not handle parameterized JSONB ->> operator positions).
	q := fmt.Sprintf(`SELECT properties ->> '%s' AS value, COUNT(*) AS count
	 FROM events
	 WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT)
	   AND event_type = $4
	   AND properties ->> '%s' IS NOT NULL
	 GROUP BY properties ->> '%s'
	 ORDER BY count DESC
	 LIMIT 20`, propKey, propKey, propKey)

	rows, err := nucleus.Query[PropertyValueStat](ctx, s.db.SQL(), q,
		siteID, fromMs, toMs, eventName,
	)
	if err != nil {
		return nil, fmt.Errorf("event property values: %w", err)
	}
	return rows, nil
}
