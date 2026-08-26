package query

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// CohortResolver returns the list of distinct_ids for a cohort. It's
// optional: if not wired (nil), cohort_id filters are silently dropped.
// The handler layer typically supplies cohorts.Service.MembersForFilter.
type CohortResolver func(ctx context.Context, siteID, cohortID string) ([]string, error)

// StatsService provides analytics query methods for the dashboard.
type StatsService struct {
	db             *nucleus.Client
	cohortResolver CohortResolver
	// retention decides which table can answer a unique count for a given
	// range, and whether it covers all of it. See coverage.go.
	retention RetentionWindows
}

func NewStatsService(db *nucleus.Client) *StatsService {
	return &StatsService{db: db, retention: DefaultRetentionWindows()}
}

// WithCohortResolver attaches a cohort lookup so the api layer can apply
// `?cohort_id=X` to any analytics route via StatsInput.ResolveCohort.
// Returns the receiver for fluent setup at boot.
func (s *StatsService) WithCohortResolver(r CohortResolver) *StatsService {
	s.cohortResolver = r
	return s
}

// ResolveCohort returns the distinct_ids for a cohort, or nil if no
// resolver is wired. Used by the API layer to expand cohort_id into
// a FilterBuilder.AddIn before issuing the analytics query.
func (s *StatsService) ResolveCohort(ctx context.Context, siteID, cohortID string) ([]string, error) {
	if s.cohortResolver == nil || cohortID == "" {
		return nil, nil
	}
	return s.cohortResolver(ctx, siteID, cohortID)
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

// rollupColumns is the set of filterable columns present on each pre-aggregated
// table. Columns absent here (notably distinct_id for cohort filters, and
// language) only exist on the raw events table.
var rollupColumns = map[string]map[string]bool{
	"stats_hourly": {
		"pathname": true, "event_type": true,
	},
	"stats_daily": {
		"pathname": true, "event_type": true, "referrer": true,
		"browser": true, "os": true, "country": true, "device": true,
		"utm_source": true, "utm_medium": true, "utm_campaign": true,
	},
}

// sessionsFilterColumns are the filterable columns present on the sessions
// table (no distinct_id/pathname/event_type, which only exist on events).
var sessionsFilterColumns = map[string]bool{
	"referrer": true, "browser": true, "os": true, "device": true,
	"country": true, "language": true,
	"utm_source": true, "utm_medium": true, "utm_campaign": true,
}

// tableForFilters picks the source table by time range (tableFor) but forces
// the raw events table whenever the active filters reference a column the chosen
// rollup table doesn't have. Without this, a cohort (distinct_id) or language
// filter over a range >24h hit a rollup table missing the column and produced
// empty/erroring charts.
func tableForFilters(from, to time.Time, filters *FilterBuilder) string {
	table := tableFor(from, to)
	if table == "events" || filters == nil {
		return table
	}
	if filters.ReferencesColumnsOutside(rollupColumns[table]) {
		return "events"
	}
	return table
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
// fromMs and toMs are raw int64 milliseconds. Passing int64 (not string) causes pgx
// SimpleProtocol to encode them as unquoted numeric literals, which Nucleus can compare
// against BIGINT columns without a text→int cast (which Nucleus rejects).
func baseParams(siteID string, fromMs, toMs int64, filters *FilterBuilder) []any {
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
	cutoff := time.Now().UTC().Add(-time.Duration(minutes) * time.Minute).UnixMilli()
	rows, err := nucleus.Query[RealtimeResult](ctx, s.db.SQL(),
		`SELECT COUNT(DISTINCT session_id) AS active_visitors
		 FROM events_recent
		 WHERE site_id = $1 AND timestamp >= $2`,
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
	fromMs := from.UnixMilli()
	toMs := to.UnixMilli()
	table := tableForFilters(from, to, filters)
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
		 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
		   AND event_type = 'pageview'%s
		 GROUP BY (CAST(timestamp AS BIGINT) / %d) * %d
		 ORDER BY (CAST(timestamp AS BIGINT) / %d) * %d`, bucketMs, bucketMs, fSQL, bucketMs, bucketMs, bucketMs, bucketMs)
	} else {
		// pageviews are additive across rollup rows, so they come from the
		// rollup — collapsed to the latest version per bucket key first.
		// visitors are not additive (see uniqueVisitors) and are filled in
		// from raw events below.
		where := fmt.Sprintf(`site_id = $1 AND %s >= $2 AND %s < $3 AND event_type = 'pageview'%s`, ts, ts, fSQL)
		q = fmt.Sprintf(`SELECT %s AS bucket,
		        SUM(pageviews) AS pageviews,
		        0 AS visitors
		 FROM %s AS r
		 GROUP BY %s
		 ORDER BY %s`, ts, LatestRows(table, []string{"pageviews"}, where), ts, ts)
	}

	rows, err := nucleus.Query[TimeSeriesPoint](ctx, s.db.SQL(), q, allParams...)
	if err != nil {
		return nil, fmt.Errorf("pageview timeseries: %w", err)
	}

	if table != "events" {
		bucketMs := int64(3600000)
		if table == "stats_daily" {
			bucketMs = 86400000
		}
		visitors, err := s.uniqueVisitorsByBucket(ctx, siteID, fromMs, toMs, bucketMs, filters,
			s.coverage(from, to, filters).Source)
		if err != nil {
			return nil, err
		}
		for i := range rows {
			rows[i].Visitors = visitors[rows[i].Bucket]
		}
	}
	return rows, nil
}

// uniqueVisitors and its helpers exist because a unique count cannot be
// summed out of a rollup. `visitors` in stats_hourly / stats_daily is a
// COUNT(DISTINCT session_id) taken per (bucket, pathname, event_type, …)
// group, so a session that spans two hours, or visits two pages in the same
// hour, is counted once per group. Summing those groups therefore inflates
// the number even after duplicate versions are collapsed away — on the live
// instance a window with 11 real sessions reported 41 after the collapse and
// 91 before it.
//
// There is no distinct-sketch type in Nucleus to make the column additive, so
// unique counts are taken from raw events instead. That is what umami — the
// reference implementation vendored under ref/umami — does: it keeps no
// unique column in any aggregate and always runs count(distinct session_id)
// against the raw event table (ref/umami/src/queries/sql/getWebsiteStats.ts).
// It also makes the number consistent across the range boundary, where the
// same site previously reported one figure below 24h (raw events) and a
// three-to-eight-times larger one above it (summed rollups).
//
// Raw events do not reach as far back as the rollups, so the source is tiered
// by where the range starts (see coverage.go): inside raw retention the count
// comes from events, past it from the session-grain sessions table, and past
// both it comes from whatever sessions still holds plus an explicit marker that
// the figure covers a shorter window than the range. Under-reporting silently
// is the failure this replaces.
func (s *StatsService) uniqueVisitorsByBucket(ctx context.Context, siteID string, fromMs, toMs, bucketMs int64, filters *FilterBuilder, src UniqueSource) (map[int64]int64, error) {
	var q string
	var params []any
	if src == SourceSessions {
		sessFilters := filters.Subset(sessionsFilterColumns)
		sessSQL, _ := filterSQL(sessFilters)
		params = baseParams(siteID, fromMs, toMs, sessFilters)
		// A session is attributed to the bucket it started in — the same
		// first_ts grain the entry/exit-page panels already use.
		q = fmt.Sprintf(`SELECT (CAST(first_ts AS BIGINT) / %d) * %d AS bucket,
		        COUNT(*) AS visitors
		 FROM %s AS s
		 GROUP BY (CAST(first_ts AS BIGINT) / %d) * %d`,
			bucketMs, bucketMs,
			LatestRows("sessions", []string{"first_ts"}, sessionWhere("", sessSQL)),
			bucketMs, bucketMs)
	} else {
		fSQL, _ := filterSQL(filters)
		params = baseParams(siteID, fromMs, toMs, filters)
		q = fmt.Sprintf(`SELECT (CAST(timestamp AS BIGINT) / %d) * %d AS bucket,
		        COUNT(DISTINCT session_id) AS visitors
		 FROM events
		 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
		   AND event_type = 'pageview'%s
		 GROUP BY (CAST(timestamp AS BIGINT) / %d) * %d`, bucketMs, bucketMs, fSQL, bucketMs, bucketMs)
	}

	type bucketRow struct {
		Bucket   int64 `db:"bucket"`
		Visitors int64 `db:"visitors"`
	}
	rows, err := nucleus.Query[bucketRow](ctx, s.db.SQL(), q, params...)
	if err != nil {
		return nil, fmt.Errorf("pageview timeseries visitors: %w", err)
	}
	out := make(map[int64]int64, len(rows))
	for _, r := range rows {
		out[r.Bucket] = r.Visitors
	}
	return out, nil
}

// uniqueVisitorsByPathname is uniqueVisitorsByBucket keyed on pathname.
func (s *StatsService) uniqueVisitorsByPathname(ctx context.Context, siteID string, fromMs, toMs int64, filters *FilterBuilder) (map[string]int64, error) {
	fSQL, _ := filterSQL(filters)
	q := fmt.Sprintf(`SELECT pathname,
	        COUNT(DISTINCT session_id) AS visitors
	 FROM events
	 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
	   AND event_type = 'pageview'%s
	 GROUP BY pathname`, fSQL)

	type pathRow struct {
		Pathname string `db:"pathname"`
		Visitors int64  `db:"visitors"`
	}
	rows, err := nucleus.Query[pathRow](ctx, s.db.SQL(), q, baseParams(siteID, fromMs, toMs, filters)...)
	if err != nil {
		return nil, fmt.Errorf("top pages visitors: %w", err)
	}
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.Pathname] = r.Visitors
	}
	return out, nil
}

// TopPage represents a page with its view count.
type TopPage struct {
	Pathname  string `json:"pathname" db:"pathname"`
	Pageviews int64  `json:"pageviews" db:"pageviews"`
	Visitors  int64  `json:"visitors" db:"visitors"`
}

func (s *StatsService) TopPages(ctx context.Context, siteID string, from, to time.Time, limit int, filters *FilterBuilder) ([]TopPage, error) {
	fromMs := from.UnixMilli()
	toMs := to.UnixMilli()
	if limit <= 0 {
		limit = 10
	}
	table := tableForFilters(from, to, filters)
	ts := tsColumn(table)
	fSQL, _ := filterSQL(filters)
	allParams := baseParams(siteID, fromMs, toMs, filters)

	var q string
	if table == "events" {
		q = fmt.Sprintf(`SELECT pathname,
		        COUNT(*) AS pageviews,
		        COUNT(DISTINCT session_id) AS visitors
		 FROM events
		 WHERE site_id = $1 AND %s >= $2 AND %s < $3
		   AND event_type = 'pageview'%s
		 GROUP BY pathname
		 ORDER BY pageviews DESC, pathname ASC
		 LIMIT %d`, ts, ts, fSQL, limit)
	} else {
		where := fmt.Sprintf(`site_id = $1 AND %s >= $2 AND %s < $3 AND event_type = 'pageview'%s`, ts, ts, fSQL)
		q = fmt.Sprintf(`SELECT pathname,
		        SUM(pageviews) AS pageviews,
		        0 AS visitors
		 FROM %s AS r
		 GROUP BY pathname
		 ORDER BY pageviews DESC, pathname ASC
		 LIMIT %d`, LatestRows(table, []string{"pageviews"}, where), limit)
	}

	rows, err := nucleus.Query[TopPage](ctx, s.db.SQL(), q, allParams...)
	if err != nil {
		return nil, fmt.Errorf("top pages: %w", err)
	}

	if table != "events" {
		visitors, err := s.uniqueVisitorsByPathname(ctx, siteID, fromMs, toMs, filters)
		if err != nil {
			return nil, err
		}
		for i := range rows {
			rows[i].Visitors = visitors[rows[i].Pathname]
		}
	}
	return rows, nil
}

// TopReferrer represents a referrer source with its count.
type TopReferrer struct {
	Referrer string `json:"referrer" db:"referrer"`
	Visitors int64  `json:"visitors" db:"visitors"`
}

func (s *StatsService) TopReferrers(ctx context.Context, siteID string, from, to time.Time, limit int, filters *FilterBuilder) ([]TopReferrer, error) {
	if limit <= 0 {
		limit = 10
	}
	// Never a rollup: `visitors` is a unique count, which cannot be summed out
	// of stats_daily (see uniqueVisitorsByBucket). Which of the two
	// unique-capable tables answers depends on where the range starts — see
	// coverage.go.
	q, allParams := s.breakdownSQL(siteID, from, to, breakdown{
		Expr: "referrer", Alias: "referrer", Where: "referrer != ''",
		EventsExtra: "event_type = 'pageview'", Cols: []string{"referrer"},
	}, limit, filters)

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
	if limit <= 0 {
		limit = 10
	}
	q, allParams := s.breakdownSQL(siteID, from, to, breakdown{
		Expr: "browser", Alias: "browser", Where: "browser != ''",
		Cols: []string{"browser"},
	}, limit, filters)

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
	if limit <= 0 {
		limit = 10
	}
	q, allParams := s.breakdownSQL(siteID, from, to, breakdown{
		Expr: "country", Alias: "country", Where: "country != ''",
		Cols: []string{"country"},
	}, limit, filters)

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
	if limit <= 0 {
		limit = 10
	}
	q, allParams := s.breakdownSQL(siteID, from, to, breakdown{
		Expr: "os", Alias: "os", Where: "os != ''",
		Cols: []string{"os"},
	}, limit, filters)

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
	if limit <= 0 {
		limit = 10
	}
	q, allParams := s.breakdownSQL(siteID, from, to, breakdown{
		Expr: "device", Alias: "device", Where: "device != ''",
		Cols: []string{"device"},
	}, limit, filters)

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

// rankChannels turns the per-channel tally into the ordered top-N.
//
// Ties are the normal case here — there are only six channels, and on a small
// site several of them sit on the same count — so the order has to be fully
// determined or the panel reshuffles between two identical loads. Ranging over
// a map and sorting on the count alone leaves tied entries in Go's randomised
// map order, which is exactly that. Channel name breaks the tie.
func rankChannels(counts map[string]int64, limit int) []ChannelStat {
	result := make([]ChannelStat, 0, len(counts))
	for ch, n := range counts {
		result = append(result, ChannelStat{Channel: ch, Visitors: n})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Visitors != result[j].Visitors {
			return result[i].Visitors > result[j].Visitors
		}
		return result[i].Channel < result[j].Channel
	})
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result
}

func (s *StatsService) TopChannels(ctx context.Context, siteID string, from, to time.Time, limit int, filters *FilterBuilder) ([]ChannelStat, error) {
	fromMs := from.UnixMilli()
	toMs := to.UnixMilli()
	var q string
	var allParams []any
	if s.coverage(from, to, filters).Source == SourceSessions {
		// Past raw retention. The sessions table carries referrer and the UTM
		// pair already collapsed to one row per session, which is exactly the
		// grain this classification wants.
		sessFilters := filters.Subset(sessionsFilterColumns)
		sessSQL, _ := filterSQL(sessFilters)
		allParams = baseParams(siteID, fromMs, toMs, sessFilters)
		q = fmt.Sprintf(`SELECT session_id,
		        COALESCE(referrer, '') AS referrer,
		        COALESCE(utm_source, '') AS utm_source,
		        COALESCE(utm_medium, '') AS utm_medium
		 FROM %s AS s`, LatestRows("sessions",
			[]string{"referrer", "utm_source", "utm_medium"},
			sessionWhere("", sessSQL)))
	} else {
		fSQL, _ := filterSQL(filters)
		allParams = baseParams(siteID, fromMs, toMs, filters)
		q = fmt.Sprintf(`SELECT DISTINCT session_id,
			        COALESCE(referrer, '') AS referrer,
			        COALESCE(utm_source, '') AS utm_source,
			        COALESCE(utm_medium, '') AS utm_medium
			 FROM events
			 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3%s`, fSQL)
	}

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

	return rankChannels(counts, limit), nil
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
	fromMs := from.UnixMilli()
	toMs := to.UnixMilli()
	table := tableForFilters(from, to, filters)
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
		 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
		   AND event_type = 'pageview'%s`, fSQL)
	} else {
		// pageviews only: visitors and sessions are unique counts and are
		// taken from raw events below (see uniqueVisitorsByBucket).
		where := fmt.Sprintf(`site_id = $1 AND %s >= $2 AND %s < $3 AND event_type = 'pageview'%s`, ts, ts, fSQL)
		q = fmt.Sprintf(`SELECT SUM(pageviews) AS pageviews,
		        0 AS visitors,
		        0 AS sessions
		 FROM %s AS r`, LatestRows(table, []string{"pageviews"}, where))
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

	if table != "events" {
		uniqQ := fmt.Sprintf(`SELECT 0 AS pageviews,
		        COUNT(DISTINCT session_id) AS visitors,
		        COUNT(DISTINCT visit_id) AS sessions
		 FROM events
		 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
		   AND event_type = 'pageview'%s`, fSQL)
		uniqParams := allParams
		if s.coverage(from, to, filters).Source == SourceSessions {
			// Past raw retention. One row per session makes the count exact.
			// The visit-grain figure raw events give for `sessions`
			// (COUNT(DISTINCT visit_id), a visit being one clock hour) has no
			// session-grain equivalent, so both tiles report the session grain
			// the bounce rate and duration already use.
			sessFilters := filters.Subset(sessionsFilterColumns)
			sessSQL, _ := filterSQL(sessFilters)
			uniqParams = baseParams(siteID, fromMs, toMs, sessFilters)
			uniqQ = fmt.Sprintf(`SELECT 0 AS pageviews,
			        COUNT(*) AS visitors,
			        COUNT(*) AS sessions
			 FROM %s AS s`, LatestRows("sessions", nil, sessionWhere("", sessSQL)))
		}
		uniqRows, err := nucleus.Query[rawStats](ctx, s.db.SQL(), uniqQ, uniqParams...)
		if err != nil {
			return OverviewStats{}, fmt.Errorf("overview uniques: %w", err)
		}
		if len(uniqRows) > 0 {
			result.Visitors = uniqRows[0].Visitors
			result.Sessions = uniqRows[0].Sessions
		}
	}

	// Query sessions table for bounce rate and average duration. The sessions
	// table has only a subset of filterable columns (no distinct_id/pathname/
	// event_type), so apply only the compatible filters — otherwise a cohort or
	// pathname filter referenced a missing column and the query errored, which
	// was then silently swallowed and bounce/duration showed as 0.
	sessFilters := filters.Subset(sessionsFilterColumns)
	sessSQL, _ := filterSQL(sessFilters)
	sessParams := baseParams(siteID, fromMs, toMs, sessFilters)
	sessQ := fmt.Sprintf(`SELECT
		        SUM(CASE WHEN is_bounce = 'true' THEN 1 ELSE 0 END) AS bounces,
		        COUNT(*) AS total_sessions,
		        SUM(CAST(last_ts AS BIGINT) - CAST(first_ts AS BIGINT)) AS duration_sum
		 FROM %s AS s`, LatestRows("sessions",
		[]string{"is_bounce", "first_ts", "last_ts"},
		fmt.Sprintf(`site_id = $1 AND first_ts >= $2 AND first_ts < $3%s`, sessSQL)))

	sessRows, err := nucleus.Query[sessionStats](ctx, s.db.SQL(), sessQ, sessParams...)
	if err != nil {
		// Non-fatal, but log it rather than silently returning zero bounce/duration.
		slog.Warn("overview: sessions sub-query failed", "site", siteID, "err", err)
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
	if limit <= 0 {
		limit = 10
	}
	q, allParams := s.breakdownSQL(siteID, from, to, breakdown{
		Expr: "language", Alias: "language", Where: "language != ''",
		Cols: []string{"language"},
	}, limit, filters)

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
	if limit <= 0 {
		limit = 10
	}
	q, allParams := s.breakdownSQL(siteID, from, to, breakdown{
		Expr:  "CAST(screen_width AS TEXT) || 'x' || CAST(screen_height AS TEXT)",
		Alias: "screen",
		Where: "CAST(screen_width AS INTEGER) > 0",
		Cols:  []string{"screen_width", "screen_height"},
	}, limit, filters)

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
	if limit <= 0 {
		limit = 10
	}

	col := "utm_source"
	switch utmType {
	case "medium":
		col = "utm_medium"
	case "campaign":
		col = "utm_campaign"
	}

	q, allParams := s.breakdownSQL(siteID, from, to, breakdown{
		Expr: col, Alias: "value", Where: col + " != ''",
		Cols: []string{col},
	}, limit, filters)

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
	fromMs := from.UnixMilli()
	toMs := to.UnixMilli()
	if limit <= 0 {
		limit = 10
	}
	fSQL, _ := filterSQL(filters)
	allParams := baseParams(siteID, fromMs, toMs, filters)

	q := fmt.Sprintf(`SELECT entry_url AS pathname,
	        COUNT(*) AS visitors
	 FROM %s AS s
	 GROUP BY entry_url
	 ORDER BY visitors DESC, pathname ASC
	 LIMIT %d`, LatestRows("sessions", []string{"entry_url"},
		fmt.Sprintf(`site_id = $1 AND first_ts >= $2 AND first_ts < $3 AND entry_url != ''%s`, fSQL)), limit)

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
	fromMs := from.UnixMilli()
	toMs := to.UnixMilli()
	if limit <= 0 {
		limit = 10
	}
	fSQL, _ := filterSQL(filters)
	allParams := baseParams(siteID, fromMs, toMs, filters)

	q := fmt.Sprintf(`SELECT exit_url AS pathname,
	        COUNT(*) AS visitors
	 FROM %s AS s
	 GROUP BY exit_url
	 ORDER BY visitors DESC, pathname ASC
	 LIMIT %d`, LatestRows("sessions", []string{"exit_url"},
		fmt.Sprintf(`site_id = $1 AND first_ts >= $2 AND first_ts < $3 AND exit_url != ''%s`, fSQL)), limit)

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
	fromMs := from.UnixMilli()
	toMs := to.UnixMilli()
	if limit <= 0 {
		limit = 10
	}
	fSQL, _ := filterSQL(filters)
	allParams := baseParams(siteID, fromMs, toMs, filters)

	q := fmt.Sprintf(`SELECT event_type,
	        COUNT(*) AS count,
	        COUNT(DISTINCT session_id) AS visitors
	 FROM events
	 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
	   AND event_type != 'pageview'%s
	 GROUP BY event_type
	 ORDER BY count DESC, event_type ASC
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
	fromMs := from.UnixMilli()
	toMs := to.UnixMilli()
	if limit <= 0 {
		limit = 20
	}

	type raw struct {
		Properties string `json:"properties" db:"properties"`
		SessionID  string `json:"session_id" db:"session_id"`
	}
	rows, err := nucleus.Query[raw](ctx, s.db.SQL(),
		// ORDER BY timestamp DESC makes the 5000-row cap deterministic (the most
		// recent events) rather than an arbitrary slice, so breakdowns are stable
		// across calls.
		`SELECT COALESCE(properties, '') AS properties, session_id
		 FROM events
		 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
		   AND event_type = $4 AND properties != ''
		 ORDER BY timestamp DESC
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
	fromMs := from.UnixMilli()
	toMs := to.UnixMilli()
	if limit <= 0 {
		limit = 20
	}

	q := fmt.Sprintf(`SELECT session_id,
	        first_ts AS first_ts,
	        last_ts AS last_ts,
	        pageviews AS pageviews,
	        entry_url, exit_url,
	        browser, os, country, device, is_bounce
	 FROM %s AS s
	 ORDER BY first_ts DESC
	 LIMIT %d`, LatestRows("sessions",
		[]string{"first_ts", "last_ts", "pageviews", "entry_url", "exit_url",
			"browser", "os", "country", "device", "is_bounce"},
		`site_id = $1 AND first_ts >= $2 AND first_ts < $3`), limit)

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
	fromMs := from.UnixMilli()
	toMs := to.UnixMilli()

	type eventRow struct {
		Properties string `db:"properties"`
	}

	rows, err := nucleus.Query[eventRow](ctx, s.db.SQL(),
		`SELECT properties FROM events
		 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
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

	fromMs := from.UnixMilli()
	toMs := to.UnixMilli()

	// Inline propKey via Sprintf after validation (Nucleus SimpleProtocol
	// may not handle parameterized JSONB ->> operator positions).
	q := fmt.Sprintf(`SELECT properties ->> '%s' AS value, COUNT(*) AS count
	 FROM events
	 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
	   AND event_type = $4
	   AND properties ->> '%s' IS NOT NULL
	 GROUP BY properties ->> '%s'
	 ORDER BY count DESC, value ASC
	 LIMIT 20`, propKey, propKey, propKey)

	rows, err := nucleus.Query[PropertyValueStat](ctx, s.db.SQL(), q,
		siteID, fromMs, toMs, eventName,
	)
	if err != nil {
		return nil, fmt.Errorf("event property values: %w", err)
	}
	return rows, nil
}
