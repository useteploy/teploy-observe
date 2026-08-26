package query

import (
	"fmt"
	"time"
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
	// Exact reports whether the source covers the whole requested range.
	Exact bool `json:"exact"`
	// RangeFrom / CoveredFrom are epoch milliseconds. They are equal when Exact.
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

// Coverage tiers a range by where its `from` sits relative to each retention
// window. The boundary is the range's start, not its length: a 7-day window
// sitting a year back is just as far outside raw retention as a 12-month one,
// and tiering on duration would route it to the exact source and return almost
// nothing.
func (w RetentionWindows) Coverage(from, to, now time.Time) UniqueCoverage {
	cov := UniqueCoverage{
		Source:      SourceEvents,
		Exact:       true,
		RangeFrom:   from.UnixMilli(),
		CoveredFrom: from.UnixMilli(),
	}

	rawCut, rawBounded := cutoff(w.RawDays, now)
	if !rawBounded || !from.Before(rawCut) {
		return cov
	}

	// Past raw retention: the sessions table still holds one row per session.
	cov.Source = SourceSessions
	sessCut, sessBounded := cutoff(w.SessionsDays, now)
	if !sessBounded || !from.Before(sessCut) {
		return cov
	}

	// Past both. Report what the sessions table supports and say so.
	cov.Exact = false
	cov.CoveredFrom = sessCut.UnixMilli()
	cov.CoveredDays = w.SessionsDays
	cov.Note = fmt.Sprintf(
		"Visitor counts cover the last %d days of this range, not all of it — per-visitor data older than that has been removed by retention. Pageviews still cover the whole range.",
		w.SessionsDays)
	return cov
}

// rawOnly is the coverage for a read that has to use raw events even though the
// range reaches past raw retention, because the active filter names a column
// only the events table carries.
func (w RetentionWindows) rawOnly(from, now time.Time) UniqueCoverage {
	cov := UniqueCoverage{
		Source:      SourceEvents,
		Exact:       true,
		RangeFrom:   from.UnixMilli(),
		CoveredFrom: from.UnixMilli(),
	}
	rawCut, rawBounded := cutoff(w.RawDays, now)
	if !rawBounded || !from.Before(rawCut) {
		return cov
	}
	cov.Exact = false
	cov.CoveredFrom = rawCut.UnixMilli()
	cov.CoveredDays = w.RawDays
	cov.Note = fmt.Sprintf(
		"Visitor counts cover the last %d days of this range, not all of it — the active filter can only be applied to raw events, which retention keeps for that long.",
		w.RawDays)
	return cov
}

// WithRetention wires the running retention configuration. Returns the receiver
// for fluent setup at boot.
func (s *StatsService) WithRetention(w RetentionWindows) *StatsService {
	s.retention = w
	return s
}

// coverage picks the unique source for a range and describes what it covers.
//
// A filter on pathname / event_type / distinct_id cannot be applied to the
// sessions table at all, so such a read stays on raw events and loses the
// longer reach — which rawOnly marks rather than hides.
func (s *StatsService) coverage(from, to time.Time, filters *FilterBuilder) UniqueCoverage {
	now := time.Now().UTC()
	cov := s.retention.Coverage(from, to, now)
	if cov.Source == SourceSessions && filters.ReferencesColumnsOutside(sessionsFilterColumns) {
		return s.retention.rawOnly(from, now)
	}
	return cov
}

// UniqueCoverageFor is the coverage the dashboard reads to label its visitor
// panels. It runs no query.
func (s *StatsService) UniqueCoverageFor(from, to time.Time, filters *FilterBuilder) UniqueCoverage {
	return s.coverage(from, to, filters)
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

	if s.coverage(from, to, filters).Source == SourceSessions {
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
