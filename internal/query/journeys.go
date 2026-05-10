package query

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
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
	Timestamp int64  `db:"timestamp"`
}

// Journeys computes page-to-page transitions and top paths for the given time range.
func (s *StatsService) Journeys(ctx context.Context, siteID string, from, to time.Time, limit int) (*JourneyResult, error) {
	if limit <= 0 {
		limit = 10
	}
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	rows, err := nucleus.Query[journeyEvent](ctx, s.db.SQL(),
		`SELECT session_id, COALESCE(pathname, '/') AS pathname, timestamp
		 FROM events
		 WHERE site_id = $1 AND CAST(timestamp AS BIGINT) >= CAST($2 AS BIGINT) AND CAST(timestamp AS BIGINT) < CAST($3 AS BIGINT)
		   AND event_type = 'pageview' AND pathname != ''
		 ORDER BY session_id, timestamp ASC`,
		siteID, fromMs, toMs,
	)
	if err != nil {
		return nil, fmt.Errorf("journeys query: %w", err)
	}

	// Group by session and build transitions + paths
	transitions := make(map[string]int) // "from->to" -> count
	pathCounts := make(map[string]int)  // serialized path -> count

	var currentSession string
	var sessionPages []string

	flushSession := func() {
		if len(sessionPages) < 2 {
			return
		}
		// Record transitions
		for i := 0; i < len(sessionPages)-1; i++ {
			key := sessionPages[i] + "->" + sessionPages[i+1]
			transitions[key]++
		}
		// Record full path (cap at 5 pages for grouping)
		path := sessionPages
		if len(path) > 5 {
			path = path[:5]
		}
		pathKey := ""
		for i, p := range path {
			if i > 0 {
				pathKey += " > "
			}
			pathKey += p
		}
		pathCounts[pathKey]++
	}

	for _, e := range rows {
		if e.SessionID != currentSession {
			flushSession()
			currentSession = e.SessionID
			sessionPages = nil
		}
		// Deduplicate consecutive same-page visits
		if len(sessionPages) == 0 || sessionPages[len(sessionPages)-1] != e.Pathname {
			sessionPages = append(sessionPages, e.Pathname)
		}
	}
	flushSession()

	// Build transition list
	var steps []JourneyStep
	for key, count := range transitions {
		for i, c := range key {
			if c == '-' && i+1 < len(key) && key[i+1] == '>' {
				steps = append(steps, JourneyStep{
					From:  key[:i],
					To:    key[i+2:],
					Count: count,
				})
				break
			}
		}
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].Count > steps[j].Count })
	if len(steps) > limit*2 {
		steps = steps[:limit*2]
	}

	// Build top paths list
	var paths []JourneyPath
	for pathKey, count := range pathCounts {
		var pages []string
		current := ""
		for i := 0; i < len(pathKey); i++ {
			if i+2 < len(pathKey) && pathKey[i:i+3] == " > " {
				pages = append(pages, current)
				current = ""
				i += 2
			} else {
				current += string(pathKey[i])
			}
		}
		if current != "" {
			pages = append(pages, current)
		}
		paths = append(paths, JourneyPath{Path: pages, Count: count})
	}
	sort.Slice(paths, func(i, j int) bool { return paths[i].Count > paths[j].Count })
	if len(paths) > limit {
		paths = paths[:limit]
	}

	return &JourneyResult{
		Transitions: steps,
		TopPaths:    paths,
		TotalPaths:  len(pathCounts),
	}, nil
}
