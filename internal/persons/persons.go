// Package persons aggregates events.distinct_id into a per-user view.
//
// C2 (Wave 4, 2026-05-10) — PostHog parity. The schema columns the SDK
// identify() contract needs already landed in migration 018; this
// package is the read layer that turns the column into a "Persons" UI.
//
// Design choices:
//   - No table of its own. Persons are an aggregate over events; the
//     refresh is implicit on every read. Materialization is a phase-2
//     concern (cohort_members table + periodic refresh per the design
//     doc); v1 keeps the contract simple.
//   - Anonymous rows (distinct_id = ”) are excluded by default. The
//     caller can opt them in with a flag — useful for ops who haven't
//     wired identify() yet and want to see traffic shape.
//   - All aggregates scan as native int64 in Go (per nucleus dogfood
//     finding #24). BIGINT comparisons wrap both sides in CAST AS
//     BIGINT (per finding #6).
package persons

import (
	"context"
	"fmt"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// Service exposes person aggregation queries.
type Service struct {
	db *nucleus.Client
}

// NewService constructs a persons.Service backed by the shared nucleus client.
func NewService(db *nucleus.Client) *Service {
	return &Service{db: db}
}

// Person is the aggregate returned by ListPersons. Counts are int64 so
// callers can format them without a parse step. *Ms timestamps are
// epoch-milliseconds to match every other Observe API.
type Person struct {
	DistinctID   string `json:"distinct_id"   db:"distinct_id"`
	FirstSeenMs  int64  `json:"first_seen_ms" db:"first_seen_ms"`
	LastSeenMs   int64  `json:"last_seen_ms"  db:"last_seen_ms"`
	EventCount   int64  `json:"event_count"   db:"event_count"`
	SessionCount int64  `json:"session_count" db:"session_count"`
	TopCountry   string `json:"top_country"   db:"top_country"`
	TopBrowser   string `json:"top_browser"   db:"top_browser"`
}

// PersonEvent is a single timeline row for the detail view.
type PersonEvent struct {
	EventID   string `json:"event_id"   db:"event_id"`
	EventType string `json:"event_type" db:"event_type"`
	URL       string `json:"url"        db:"url"`
	Pathname  string `json:"pathname"   db:"pathname"`
	Timestamp int64  `json:"timestamp"  db:"timestamp"`
}

// PersonDetail bundles a person's aggregate row with its event timeline.
type PersonDetail struct {
	Person   Person        `json:"person"`
	Timeline []PersonEvent `json:"timeline"`
}

// ListPersons returns one row per distinct_id observed in the time
// window for siteID, ordered by last activity descending. Anonymous
// (empty distinct_id) rows are excluded unless includeAnonymous=true —
// most operators want the identified-users view, but a fresh install
// without identify() wired needs the all-traffic view to verify the
// page is even rendering.
func (s *Service) ListPersons(ctx context.Context, siteID string, fromMs, toMs int64, limit, offset int, includeAnonymous bool) ([]Person, error) {
	if siteID == "" {
		return []Person{}, nil
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}

	from := dbutil.IntParam(fromMs)
	to := dbutil.IntParam(toMs)

	// Inner aggregate computes the per-distinct_id summary. The outer
	// scan is plain so we can sort + paginate in SQL. argMax(country,
	// timestamp) picks the most recent country/browser per person —
	// nucleus doesn't expose argMax yet, so we approximate with MAX
	// over a tuple-style hack: pick by timestamp using a subquery.
	// Simpler portable form: use a window-style MAX(country) and
	// accept that for ties we get the last grouped value, which is
	// good enough for a "where they're connecting from" hint.
	anonClause := "AND distinct_id != ''"
	if includeAnonymous {
		anonClause = ""
	}

	q := fmt.Sprintf(`SELECT distinct_id,
	        MIN(CAST(timestamp AS BIGINT)) AS first_seen_ms,
	        MAX(CAST(timestamp AS BIGINT)) AS last_seen_ms,
	        COUNT(*) AS event_count,
	        COUNT(DISTINCT session_id) AS session_count,
	        argMax(country, CAST(timestamp AS BIGINT)) AS top_country,
	        argMax(browser, CAST(timestamp AS BIGINT)) AS top_browser
	 FROM events
	 WHERE site_id = $1
	   AND timestamp >= $2
	   AND timestamp < $3
	   %s
	 GROUP BY distinct_id
	 ORDER BY last_seen_ms DESC
	 LIMIT %d OFFSET %d`, anonClause, limit, offset)

	rows, err := nucleus.Query[Person](ctx, s.db.SQL(), q, siteID, from, to)
	if err != nil {
		return nil, fmt.Errorf("list persons: %w", err)
	}
	if rows == nil {
		rows = []Person{}
	}
	return rows, nil
}

// PersonDetail returns the aggregate row for distinctID plus the most
// recent 100 events as a vertical timeline. distinctID = ” returns an
// empty result on purpose — anonymous-aggregated detail makes no sense.
func (s *Service) PersonDetail(ctx context.Context, siteID, distinctID string) (PersonDetail, error) {
	if siteID == "" || distinctID == "" {
		return PersonDetail{}, nil
	}

	// Aggregate over the full history of this distinct_id (no time
	// window — the detail view always shows the lifetime summary).
	aggQ := `SELECT distinct_id,
	        MIN(CAST(timestamp AS BIGINT)) AS first_seen_ms,
	        MAX(CAST(timestamp AS BIGINT)) AS last_seen_ms,
	        COUNT(*) AS event_count,
	        COUNT(DISTINCT session_id) AS session_count,
	        argMax(country, CAST(timestamp AS BIGINT)) AS top_country,
	        argMax(browser, CAST(timestamp AS BIGINT)) AS top_browser
	 FROM events
	 WHERE site_id = $1 AND distinct_id = $2
	 GROUP BY distinct_id`

	aggRows, err := nucleus.Query[Person](ctx, s.db.SQL(), aggQ, siteID, distinctID)
	if err != nil {
		return PersonDetail{}, fmt.Errorf("person aggregate: %w", err)
	}

	var p Person
	if len(aggRows) > 0 {
		p = aggRows[0]
	} else {
		p = Person{DistinctID: distinctID}
	}

	tlQ := `SELECT event_id, event_type,
	        COALESCE(url, '') AS url,
	        COALESCE(pathname, '') AS pathname,
	        CAST(timestamp AS BIGINT) AS timestamp
	 FROM events
	 WHERE site_id = $1 AND distinct_id = $2
	 ORDER BY CAST(timestamp AS BIGINT) DESC
	 LIMIT 100`

	tl, err := nucleus.Query[PersonEvent](ctx, s.db.SQL(), tlQ, siteID, distinctID)
	if err != nil {
		// Non-fatal: aggregate is the load-bearing part. Return what we have.
		return PersonDetail{Person: p, Timeline: []PersonEvent{}}, nil
	}
	if tl == nil {
		tl = []PersonEvent{}
	}
	return PersonDetail{Person: p, Timeline: tl}, nil
}

// CountPersons returns the total distinct_id count in the window.
// Cheap helper used by the UI to render pagination totals without
// pulling a second page.
func (s *Service) CountPersons(ctx context.Context, siteID string, fromMs, toMs int64, includeAnonymous bool) (int64, error) {
	if siteID == "" {
		return 0, nil
	}
	from := dbutil.IntParam(fromMs)
	to := dbutil.IntParam(toMs)

	anonClause := "AND distinct_id != ''"
	if includeAnonymous {
		anonClause = ""
	}

	type countRow struct {
		Total int64 `db:"total"`
	}
	q := fmt.Sprintf(`SELECT COUNT(DISTINCT distinct_id) AS total
	 FROM events
	 WHERE site_id = $1
	   AND timestamp >= $2
	   AND timestamp < $3
	   %s`, anonClause)

	rows, err := nucleus.Query[countRow](ctx, s.db.SQL(), q, siteID, from, to)
	if err != nil {
		return 0, fmt.Errorf("count persons: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rows[0].Total, nil
}

// DefaultWindow returns the default 30-day query window when the caller
// omits from / to. Centralised so the handler and tests agree on the
// same default.
func DefaultWindow() (int64, int64) {
	now := time.Now().UTC()
	return now.Add(-30 * 24 * time.Hour).UnixMilli(), now.UnixMilli()
}
