package query

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

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

	fromMs := from.UnixMilli()
	toMs := to.UnixMilli()

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

// FunnelBreakdownResult groups funnel outcomes by a property dimension.
type FunnelBreakdownResult struct {
	Breakdown string         `json:"breakdown"`
	Results   []FunnelResult `json:"results"`
}

// FunnelByBreakdown computes the funnel separately for each distinct value of
// `breakdownBy` (e.g. "browser", "country", "device", "os"). Unsupported
// breakdowns return an error. Groups with fewer than minSize sessions are dropped.
func (s *StatsService) FunnelByBreakdown(ctx context.Context, siteID string, from, to time.Time, steps []FunnelStep, breakdownBy string, minSize int) ([]FunnelBreakdownResult, error) {
	if len(steps) == 0 {
		return nil, nil
	}
	col, ok := map[string]string{
		"browser": "COALESCE(browser, 'unknown')",
		"country": "COALESCE(country, 'unknown')",
		"device":  "COALESCE(device, 'unknown')",
		"os":      "COALESCE(os, 'unknown')",
	}[breakdownBy]
	if !ok {
		return nil, fmt.Errorf("unsupported breakdown: %s", breakdownBy)
	}

	fromMs := from.UnixMilli()
	toMs := to.UnixMilli()

	type breakdownRow struct {
		SessionID string `db:"session_id"`
		EventType string `db:"event_type"`
		Pathname  string `db:"pathname"`
		Timestamp int64  `db:"timestamp"`
		Breakdown string `db:"breakdown"`
	}
	rows, err := nucleus.Query[breakdownRow](ctx, s.db.SQL(),
		fmt.Sprintf(`SELECT session_id, event_type, COALESCE(pathname, '') AS pathname, timestamp, %s AS breakdown
		 FROM events
		 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
		 ORDER BY session_id, timestamp ASC`, col),
		siteID, fromMs, toMs,
	)
	if err != nil {
		return nil, fmt.Errorf("funnel breakdown query: %w", err)
	}

	// Group events by (breakdown, session).
	type key struct{ Breakdown, Session string }
	grouped := make(map[key][]funnelEvent)
	sessionBreakdown := make(map[string]string)
	for _, r := range rows {
		// A session can span different breakdown values in theory; keep the first seen.
		if _, seen := sessionBreakdown[r.SessionID]; !seen {
			sessionBreakdown[r.SessionID] = r.Breakdown
		}
		k := key{Breakdown: sessionBreakdown[r.SessionID], Session: r.SessionID}
		grouped[k] = append(grouped[k], funnelEvent{
			SessionID: r.SessionID,
			EventType: r.EventType,
			Pathname:  r.Pathname,
			Timestamp: r.Timestamp,
		})
	}

	// Walk per-breakdown.
	perBreakdown := make(map[string][]int) // breakdown -> stepCounts[]
	for k, events := range grouped {
		sort.Slice(events, func(i, j int) bool { return events[i].Timestamp < events[j].Timestamp })
		counts, ok := perBreakdown[k.Breakdown]
		if !ok {
			counts = make([]int, len(steps))
			perBreakdown[k.Breakdown] = counts
		}
		stepIdx := 0
		for _, e := range events {
			if stepIdx >= len(steps) {
				break
			}
			if matchesStep(e, steps[stepIdx]) {
				counts[stepIdx]++
				stepIdx++
			}
		}
	}

	out := make([]FunnelBreakdownResult, 0, len(perBreakdown))
	for b, counts := range perBreakdown {
		if counts[0] < minSize {
			continue
		}
		res := make([]FunnelResult, len(steps))
		for i, step := range steps {
			res[i] = FunnelResult{Step: step, Visitors: counts[i]}
			if counts[0] > 0 {
				res[i].Conversion = float64(counts[i]) / float64(counts[0]) * 100
			}
			if i > 0 && counts[i-1] > 0 {
				res[i].DropOff = float64(counts[i-1]-counts[i]) / float64(counts[i-1]) * 100
			}
		}
		out = append(out, FunnelBreakdownResult{Breakdown: b, Results: res})
	}
	// Stable order: highest starting cohort first.
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Results) == 0 {
			return false
		}
		if len(out[j].Results) == 0 {
			return true
		}
		return out[i].Results[0].Visitors > out[j].Results[0].Visitors
	})
	return out, nil
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
