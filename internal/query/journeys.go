package query

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// JourneyStep represents a page-to-page transition with its count.
type JourneyStep struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Count int    `json:"count"`
}

// JourneyPath represents a full user path through the site.
type JourneyPath struct {
	Path  []string `json:"path"`
	Count int      `json:"count"`
}

// JourneyResult contains both transition edges and top full paths.
type JourneyResult struct {
	Transitions []JourneyStep `json:"transitions"`
	TopPaths    []JourneyPath `json:"top_paths"`
	TotalPaths  int           `json:"total_paths"`
}

type journeyEvent struct {
	SessionID string `db:"session_id"`
	Pathname  string `db:"pathname"`
	EventID   string `db:"event_id"`
	Timestamp int64  `db:"timestamp"`
}

// Journeys computes page-to-page transitions and top paths for the given
// time range.
//
// O12: the read is admission-gated and bounded — the SQL carries a LIMIT
// of budget+1 rows and reading past the declared row budget converts to a
// labeled refusal instead of an unbounded slice (guard.go
// boundedRangeQuery). Below the ceiling the result set is unchanged.
func (s *StatsService) Journeys(ctx context.Context, siteID string, from, to time.Time, limit int, active ...*FilterBuilder) (*JourneyResult, error) {
	if limit == 0 {
		limit = 10
	}
	if limit < 0 || limit > 1000 {
		return nil, fmt.Errorf("journey limit must be between 1 and 1000")
	}
	if err := s.validateWindow(from, to); err != nil {
		return nil, err
	}
	fromMs := from.UnixMilli()
	toMs := to.UnixMilli()

	qctx, finish, err := s.beginQuery(ctx, siteID)
	if err != nil {
		return nil, err
	}
	defer finish()

	var filters *FilterBuilder
	if len(active) > 0 {
		filters = active[0]
	}
	fSQL, _ := filterSQL(filters)
	rows, err := boundedRangeQuery[journeyEvent](qctx, s,
		`SELECT event_id, session_id, COALESCE(pathname, '/') AS pathname, timestamp
		 FROM events
		 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
		   AND event_type = 'pageview' AND pathname != ''
		 `+fSQL+` ORDER BY session_id ASC, timestamp ASC, event_id ASC`,
		baseParams(siteID, fromMs, toMs, filters)...,
	)
	if err != nil {
		return nil, fmt.Errorf("journeys query: %w", err)
	}

	return computeJourneys(qctx, rows, limit)
}

func computeJourneys(qctx context.Context, rows []journeyEvent, limit int) (*JourneyResult, error) {
	// Group by session and build transitions + paths
	type edge struct{ from, to string }
	transitions := make(map[edge]int)  // tuple identity
	pathCounts := make(map[string]int) // serialized path -> count

	var currentSession string
	var sessionPages []string

	flushSession := func() error {
		if len(sessionPages) < 2 {
			return qctx.Err()
		}
		// Record transitions
		for i := 0; i < len(sessionPages)-1; i++ {
			if err := qctx.Err(); err != nil {
				return err
			}
			key := edge{sessionPages[i], sessionPages[i+1]}
			transitions[key]++
		}
		// Record full path (cap at 5 pages for grouping)
		path := sessionPages
		if len(path) > 5 {
			path = path[:5]
		}
		encoded, _ := json.Marshal(path)
		pathCounts[string(encoded)]++
		return qctx.Err()
	}

	for _, e := range rows {
		if err := qctx.Err(); err != nil {
			return nil, err
		}
		if e.SessionID != currentSession {
			if err := flushSession(); err != nil {
				return nil, err
			}
			currentSession = e.SessionID
			sessionPages = nil
		}
		// Deduplicate consecutive same-page visits
		if len(sessionPages) == 0 || sessionPages[len(sessionPages)-1] != e.Pathname {
			sessionPages = append(sessionPages, e.Pathname)
		}
	}
	if err := flushSession(); err != nil {
		return nil, err
	}

	// Build transition list
	var steps []JourneyStep
	for key, count := range transitions {
		if err := qctx.Err(); err != nil {
			return nil, err
		}
		steps = append(steps, JourneyStep{From: key.from, To: key.to, Count: count})
	}
	if err := sortWithContext(qctx, steps, func(a, b JourneyStep) bool {
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.To < b.To
	}); err != nil {
		return nil, err
	}

	if len(steps) > limit*2 {
		steps = steps[:limit*2]
	}

	// Build top paths list
	var paths []JourneyPath
	for pathKey, count := range pathCounts {
		if err := qctx.Err(); err != nil {
			return nil, err
		}
		var pages []string
		if err := json.Unmarshal([]byte(pathKey), &pages); err != nil {
			return nil, err
		}
		paths = append(paths, JourneyPath{Path: pages, Count: count})
	}
	if err := sortWithContext(qctx, paths, func(a, b JourneyPath) bool {
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		x, _ := json.Marshal(a.Path)
		y, _ := json.Marshal(b.Path)
		return string(x) < string(y)
	}); err != nil {
		return nil, err
	}
	if err := qctx.Err(); err != nil {
		return nil, err
	}

	if len(paths) > limit {
		paths = paths[:limit]
	}

	return &JourneyResult{
		Transitions: steps,
		TopPaths:    paths,
		TotalPaths:  len(pathCounts),
	}, nil
}
