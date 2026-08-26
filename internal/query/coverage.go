package query

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// Which table can answer a unique (visitor / session) count, and how much of
// the asked-for range that answer really covers.
//
// A unique count cannot be summed out of a rollup — see the note on
// uniqueVisitorsByBucket — so it has to be counted from a table that stores one
// row per thing being counted. Two such tables exist and they are pruned on
// different schedules:
//
//	events   one row per event, kept for OBSERVE_RAW_RETENTION_DAYS (default 30)
//	sessions one row per session, kept for its own retention policy (default 90)
//
// Pageviews meanwhile come from stats_hourly / stats_daily, which are kept for
// a year or indefinitely. So a range the pageview panel answers in full can be
// one that no unique-capable table covers at all, and counting uniques from
// whatever survives without saying so under-reports them silently. That silence
// is the bug; the tiers below plus an explicit marker are the fix.
type UniqueSource string

const (
	// SourceEvents counts DISTINCT session_id over raw events. Exact, and the
	// only source that can honour a filter on a column the sessions table
	// does not carry (pathname, event_type, distinct_id).
	SourceEvents UniqueSource = "events"
	// SourceSessions counts rows in the session-grain sessions table. One row
	// per session makes the count exact and cheap, and sessions outlive raw
	// events, so this extends accurate uniques past raw retention.
	SourceSessions UniqueSource = "sessions"
)

// RetentionWindows are the retention policies a unique count can be answered
// from, in days, as the running process is actually configured — not constants.
// An operator who raises OBSERVE_RAW_RETENTION_DAYS therefore widens the exact
// tier automatically. A value <= 0 means that policy prunes nothing (see
// RetentionService.RunCleanup, which skips those), i.e. an unbounded window.
type RetentionWindows struct {
	RawDays      int
	SessionsDays int
}

// DefaultRetentionWindows mirrors the shipped defaults, for callers that do not
// wire the running config (tests, embedders).
func DefaultRetentionWindows() RetentionWindows {
	return RetentionWindows{RawDays: 30, SessionsDays: 90}
}

// UniqueCoverage describes which table answered a range's unique counts and how
// much of the range that answer covers. Exact == false is the truthful marker:
// the figure is real, but it describes a shorter window than the one asked for.
type UniqueCoverage struct {
	Source UniqueSource `json:"source"`
	// Exact reports whether the source covers everything in the requested
	// range — which is not the same as covering the whole calendar range. A
	// range reaching back further than the site has ever had data is exact:
	// there is nothing older to be missing.
	Exact bool `json:"exact"`
	// RangeFrom / CoveredFrom are epoch milliseconds. CoveredFrom is the oldest
	// instant the figures actually describe. It equals RangeFrom in the normal
	// case; when a retention cutoff bit, it is that cutoff; when the site is
	// younger than the range it is the site's first data, and Exact stays true
	// because nothing was lost.
	RangeFrom   int64 `json:"range_from"`
	CoveredFrom int64 `json:"covered_from"`
	// CoveredDays is the retention window backing the answer, 0 when Exact.
	CoveredDays int `json:"covered_days"`
	// Note is empty when Exact, and otherwise the sentence to show the user.
	Note string `json:"note"`
}

// cutoff returns the oldest instant a days-long retention policy still keeps,
// and whether the policy bounds anything at all.
func cutoff(days int, now time.Time) (time.Time, bool) {
	if days <= 0 {
		return time.Time{}, false
	}
	return now.Add(-time.Duration(days) * 24 * time.Hour), true
}

// bound tiers a range by where its `from` sits relative to each retention
// window: which source answers it, and which cutoff — if any — limits how far
// back that source reaches. The boundary is the range's start, not its length:
// a 7-day window sitting a year back is just as far outside raw retention as a
// 12-month one, and tiering on duration would route it to the exact source and
// return almost nothing.
//
// forcedRaw is the case where an active filter names a column only the events
// table carries, so the read cannot fall through to sessions.
func (w RetentionWindows) bound(from, now time.Time, forcedRaw bool) (src UniqueSource, cut time.Time, days int, bounded bool) {
	rawCut, rawBounded := cutoff(w.RawDays, now)
	if !rawBounded || !from.Before(rawCut) {
		// The whole range sits inside raw retention.
		return SourceEvents, time.Time{}, 0, false
	}
	if forcedRaw {
		return SourceEvents, rawCut, w.RawDays, true
	}
	// Past raw retention: the sessions table still holds one row per session.
	sessCut, sessBounded := cutoff(w.SessionsDays, now)
	if !sessBounded || !from.Before(sessCut) {
		return SourceSessions, time.Time{}, 0, false
	}
	return SourceSessions, sessCut, w.SessionsDays, true
}

// coverage turns a tier plus the site's earliest data into what can honestly be
// claimed for the range.
//
// The calendar alone is not evidence of loss. A range that starts before a
// retention cutoff is only short-changed if data older than that cutoff ever
// existed — otherwise the cutoff pruned nothing and the count is complete.
// Deciding on the cutoff alone told a two-month-old install that a 12-month
// range had "been removed by retention", and told a brand-new one the same on
// its first day. So `earliest` is the deciding evidence: the oldest instant the
// site has any data for, taken from the pageview rollup, which outlives every
// unique-capable table. A zero `earliest` means unknown, or no data at all;
// both must fail toward claiming nothing was removed.
func (w RetentionWindows) coverage(from, now time.Time, forcedRaw bool, earliest time.Time) UniqueCoverage {
	src, cut, days, bounded := w.bound(from, now, forcedRaw)
	cov := UniqueCoverage{
		Source:      src,
		Exact:       true,
		RangeFrom:   from.UnixMilli(),
		CoveredFrom: from.UnixMilli(),
	}
	if !bounded {
		return cov
	}
	if earliest.IsZero() || !earliest.Before(cut) {
		// Nothing older than the covered window was ever here, so nothing is
		// missing. Report the window the figures really describe: the site's
		// own start, when that is later than the range's.
		if !earliest.IsZero() && earliest.After(from) {
			cov.CoveredFrom = earliest.UnixMilli()
		}
		return cov
	}

	// Data genuinely predates what the source still holds. Report what survives
	// and say so.
	cov.Exact = false
	cov.CoveredFrom = cut.UnixMilli()
	cov.CoveredDays = days
	if forcedRaw {
		cov.Note = fmt.Sprintf(
			"Visitor counts cover the last %d days of this range, not all of it — the active filter can only be applied to raw events, which retention keeps for that long.",
			days)
	} else {
		cov.Note = fmt.Sprintf(
			"Visitor counts cover the last %d days of this range, not all of it — per-visitor data older than that has been removed by retention. Pageviews still cover the whole range.",
			days)
	}
	return cov
}

// Coverage is the coverage for a range answered by whichever table retention
// leaves able to answer it. `earliest` is the oldest instant the site has data
// for; the zero time means unknown or empty, which never warns.
func (w RetentionWindows) Coverage(from, to, now, earliest time.Time) UniqueCoverage {
	return w.coverage(from, now, false, earliest)
}

// rawOnly is the coverage for a read that has to use raw events even though the
// range reaches past raw retention, because the active filter names a column
// only the events table carries.
func (w RetentionWindows) rawOnly(from, now, earliest time.Time) UniqueCoverage {
	return w.coverage(from, now, true, earliest)
}

// WithRetention wires the running retention configuration. Returns the receiver
// for fluent setup at boot.
func (s *StatsService) WithRetention(w RetentionWindows) *StatsService {
	s.retention = w
	return s
}

// forcedRaw reports whether a read has to stay on raw events even though the
// range reaches past raw retention: a filter on pathname / event_type /
// distinct_id cannot be applied to the sessions table at all.
func (s *StatsService) forcedRaw(from time.Time, now time.Time, filters *FilterBuilder) bool {
	src, _, _, _ := s.retention.bound(from, now, false)
	return src == SourceSessions && filters.ReferencesColumnsOutside(sessionsFilterColumns)
}

// sourceFor picks the table a range's unique counts are read from. Retention
// alone decides it, and it needs no evidence about the site: which table still
// holds rows for the range's start does not depend on how much data the site
// has. Only the honesty of the answer — Exact / Note — depends on that, and
// that lives in UniqueCoverageFor.
func (s *StatsService) sourceFor(from time.Time, filters *FilterBuilder) UniqueSource {
	now := time.Now().UTC()
	if s.forcedRaw(from, now, filters) {
		return SourceEvents
	}
	src, _, _, _ := s.retention.bound(from, now, false)
	return src
}

// UniqueCoverageFor is the coverage the dashboard reads to label its visitor
// panels.
//
// It runs at most one query, and only when it could change the answer: a range
// that sits entirely inside retention cannot be missing anything however old
// the site is, so the common case still costs nothing. When retention could
// have pruned something, the site's earliest data decides whether it actually
// did — and that lookup is cached per site (see earliestData).
func (s *StatsService) UniqueCoverageFor(ctx context.Context, siteID string, from, to time.Time, filters *FilterBuilder) UniqueCoverage {
	now := time.Now().UTC()
	forced := s.forcedRaw(from, now, filters)
	if _, _, _, bounded := s.retention.bound(from, now, forced); !bounded {
		return s.retention.coverage(from, now, forced, time.Time{})
	}
	return s.retention.coverage(from, now, forced, s.earliestData(ctx, siteID))
}

// How long a per-site earliest-data instant is reused. The value only ever
// moves forward, and only when a site is deleted or the pageview rollup is
// pruned — neither is minute-to-minute news — so a few stale minutes cost
// nothing and keep a dashboard-load route off the table. A failed lookup is
// cached for much less: it resolves to "no warning", and a transient failure
// must not silence a real one for ten minutes.
const (
	earliestDataTTL      = 10 * time.Minute
	earliestDataErrTTL   = time.Minute
	earliestDataCacheMax = 1024
)

type earliestEntry struct {
	// at is the zero time when the site has no data, or when the lookup failed.
	at    time.Time
	until time.Time
}

// earliestCache memoises the per-site earliest-data instant. Zero value ready.
type earliestCache struct {
	mu      sync.Mutex
	entries map[string]earliestEntry
}

// earliestData is the oldest instant this site has any data for, or the zero
// time when that cannot be determined — no data yet, no database wired, or a
// failed query. Every one of those resolves to "do not claim retention removed
// anything", which is the safe direction: an empty site must not be told its
// history was pruned.
func (s *StatsService) earliestData(ctx context.Context, siteID string) time.Time {
	if s == nil || s.db == nil || siteID == "" {
		return time.Time{}
	}
	now := time.Now()

	s.earliest.mu.Lock()
	e, ok := s.earliest.entries[siteID]
	s.earliest.mu.Unlock()
	if ok && now.Before(e.until) {
		return e.at
	}

	at, err := s.queryEarliestData(ctx, siteID)
	ttl := earliestDataTTL
	if err != nil {
		// Do not swallow this. An unknown earliest instant suppresses the
		// coverage note, so a silent failure is indistinguishable from a site
		// that genuinely lost nothing.
		slog.Warn("earliest-data lookup failed; unique coverage will not claim retention removed anything",
			"site", siteID, "err", err)
		at, ttl = time.Time{}, earliestDataErrTTL
	}

	s.earliest.mu.Lock()
	if s.earliest.entries == nil {
		s.earliest.entries = make(map[string]earliestEntry, 8)
	}
	if len(s.earliest.entries) >= earliestDataCacheMax {
		for k, v := range s.earliest.entries {
			if !now.Before(v.until) {
				delete(s.earliest.entries, k)
			}
		}
	}
	s.earliest.entries[siteID] = earliestEntry{at: at, until: now.Add(ttl)}
	s.earliest.mu.Unlock()
	return at
}

// queryEarliestData reads the site's first pageview bucket.
//
// stats_daily is the right table for it: it carries no retention policy at all
// (jobs.DefaultPolicies lists events, stats_hourly and sessions, not it), so it
// reaches back further than anything a unique count can be taken from, which is
// exactly the comparison the coverage note needs.
//
// The shape is a single-column aggregate, cast to text and parsed here. That is
// the one form that reliably streams on a large Nucleus table instead of
// materialising it — see docs/operations/issue-duplicate-collapse.md, where
// every row-returning form over a big table was rejected by the memory limiter
// and the aggregates were not. The cast also avoids scanning a NULL (an empty
// table's MIN) into an integer.
func (s *StatsService) queryEarliestData(ctx context.Context, siteID string) (time.Time, error) {
	type earliestRow struct {
		Earliest string `db:"earliest"`
	}
	rows, err := nucleus.Query[earliestRow](ctx, s.db.SQL(),
		`SELECT CAST(COALESCE(MIN(ts_bucket), 0) AS TEXT) AS earliest
		 FROM stats_daily WHERE site_id = $1`, siteID)
	if err != nil {
		return time.Time{}, err
	}
	if len(rows) == 0 {
		return time.Time{}, nil
	}
	ms, err := strconv.ParseInt(rows[0].Earliest, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("earliest bucket %q: %w", rows[0].Earliest, err)
	}
	if ms <= 0 {
		// No rows for this site: MIN was NULL and COALESCE made it 0.
		return time.Time{}, nil
	}
	return time.UnixMilli(ms).UTC(), nil
}

// sessionUniqueSQL renders the session-grain equivalent of a raw-events
// `COUNT(DISTINCT session_id)` breakdown: the sessions table collapsed to its
// latest version per session, one row per session, counted by dimension.
//
// dimGroup is the expression grouped on, dimAlias the output name. The ORDER BY
// names the alias deliberately: Nucleus resolves ORDER BY against the select
// list's output names only and silently ignores a term it cannot resolve, so
// ordering on the underlying column would leave the rows in hash order and make
// the LIMIT an arbitrary slice.
//
// cols must name every sessions column the dimension or the WHERE clause needs.
func sessionUniqueSQL(dimGroup, dimAlias string, cols []string, where string, limit int) string {
	q := fmt.Sprintf(`SELECT %s AS %s,
	        COUNT(*) AS visitors
	 FROM %s AS s
	 GROUP BY %s
	 ORDER BY visitors DESC, %s ASC`,
		dimGroup, dimAlias, LatestRows("sessions", cols, where), dimGroup, dimAlias)
	if limit > 0 {
		q += fmt.Sprintf("\n LIMIT %d", limit)
	}
	return q
}

// breakdown describes one visitor-breakdown dimension in the two forms it has
// to be asked for: a COUNT(DISTINCT session_id) over raw events, and a COUNT of
// rows over the session-grain sessions table.
type breakdown struct {
	// Expr is the expression grouped on. It is the same in both tables — every
	// dimension the dashboard breaks down by (referrer, browser, os, device,
	// country, language, screen, utm_*) exists on sessions as well as events.
	Expr string
	// Alias is the output name. ORDER BY has to name it: Nucleus resolves
	// ORDER BY against output names only and drops a term it cannot resolve.
	Alias string
	// Where is the dimension's own predicate, e.g. `browser != ''`.
	Where string
	// EventsExtra is appended to Where on the events branch only — the
	// sessions table has no event_type column to filter on.
	EventsExtra string
	// Cols names the sessions columns Expr and Where need.
	Cols []string
}

// breakdownSQL renders the dimension against whichever table can answer the
// range, and returns the matching parameters. The two branches carry different
// parameter lists because the sessions table can only honour a subset of the
// filters.
func (s *StatsService) breakdownSQL(siteID string, from, to time.Time, b breakdown, limit int, filters *FilterBuilder) (string, []any) {
	fromMs, toMs := from.UnixMilli(), to.UnixMilli()

	if s.sourceFor(from, filters) == SourceSessions {
		sessFilters := filters.Subset(sessionsFilterColumns)
		sessSQL, _ := filterSQL(sessFilters)
		return sessionUniqueSQL(b.Expr, b.Alias, b.Cols, sessionWhere(b.Where, sessSQL), limit),
			baseParams(siteID, fromMs, toMs, sessFilters)
	}

	fSQL, _ := filterSQL(filters)
	where := b.Where
	if b.EventsExtra != "" {
		where += " AND " + b.EventsExtra
	}
	q := fmt.Sprintf(`SELECT %s AS %s,
	        COUNT(DISTINCT session_id) AS visitors
	 FROM events
	 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
	   AND %s%s
	 GROUP BY %s
	 ORDER BY visitors DESC, %s ASC
	 LIMIT %d`, b.Expr, b.Alias, where, fSQL, b.Expr, b.Alias, limit)
	return q, baseParams(siteID, fromMs, toMs, filters)
}

// sessionWhere is the standard sessions-table predicate: the site, the range
// against first_ts (the grain the entry/exit-page panels and the bounce-rate
// sub-query already use), plus any filters the table can actually answer.
func sessionWhere(extra, filterSQL string) string {
	w := `site_id = $1 AND first_ts >= $2 AND first_ts < $3`
	if extra != "" {
		w += " AND " + extra
	}
	return w + filterSQL
}
