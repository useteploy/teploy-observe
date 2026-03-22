package query

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/teploy/observe/internal/dbutil"
)

// FunnelStep defines one step in a funnel (event type or pathname match).
type FunnelStep struct {
	Type  string `json:"type"`  // "event" or "page"
	Value string `json:"value"` // event_type name or pathname
}

// FunnelResult is the conversion data for each step.
type FunnelResult struct {
	Step       FunnelStep `json:"step"`
	Visitors   int        `json:"visitors"`
	Conversion float64    `json:"conversion"` // % of step 1 visitors
	DropOff    float64    `json:"drop_off"`   // % dropped from previous step
}

// funnelEvent is the minimal event data needed for funnel computation.
type funnelEvent struct {
	SessionID string `db:"session_id"`
	EventType string `db:"event_type"`
	Pathname  string `db:"pathname"`
	Timestamp int64  `db:"timestamp"`
}

// Funnel computes multi-step conversion for the given steps.
// For each session, checks if events match steps in order (by timestamp).
func (s *StatsService) Funnel(ctx context.Context, siteID string, from, to time.Time, steps []FunnelStep) ([]FunnelResult, error) {
	if len(steps) == 0 {
		return nil, nil
	}

	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	// Fetch all events in the time range for this site
	rows, err := nucleus.Query[funnelEvent](ctx, s.db.SQL(),
		`SELECT session_id, event_type, COALESCE(pathname, '') AS pathname, timestamp
		 FROM events
		 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
		 ORDER BY session_id, timestamp ASC`,
		siteID, fromMs, toMs,
	)
	if err != nil {
		return nil, fmt.Errorf("funnel query: %w", err)
	}

	// Group events by session
	sessions := make(map[string][]funnelEvent)
	for _, e := range rows {
		sessions[e.SessionID] = append(sessions[e.SessionID], e)
	}

	// For each session, walk through events and check step completion
	stepCounts := make([]int, len(steps))
	for _, events := range sessions {
		sort.Slice(events, func(i, j int) bool { return events[i].Timestamp < events[j].Timestamp })
		stepIdx := 0
		for _, e := range events {
			if stepIdx >= len(steps) {
				break
			}
			if matchesStep(e, steps[stepIdx]) {
				stepCounts[stepIdx]++
				stepIdx++
			}
		}
	}

	// Build results with conversion percentages
	results := make([]FunnelResult, len(steps))
	for i, step := range steps {
		results[i] = FunnelResult{
			Step:     step,
			Visitors: stepCounts[i],
		}
		if stepCounts[0] > 0 {
			results[i].Conversion = float64(stepCounts[i]) / float64(stepCounts[0]) * 100
		}
		if i > 0 && stepCounts[i-1] > 0 {
			results[i].DropOff = float64(stepCounts[i-1]-stepCounts[i]) / float64(stepCounts[i-1]) * 100
		}
	}

	return results, nil
}

func matchesStep(e funnelEvent, step FunnelStep) bool {
	switch step.Type {
	case "page":
		return e.EventType == "pageview" && e.Pathname == step.Value
	case "event":
		return e.EventType == step.Value
	default:
		return e.EventType == step.Value || e.Pathname == step.Value
	}
}
