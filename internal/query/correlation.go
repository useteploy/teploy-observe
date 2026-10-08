package query

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// Correlation represents a property value correlated with a target event.
type Correlation struct {
	Property     string  `json:"property"`
	Value        string  `json:"value"`
	Uplift       float64 `json:"uplift"`      // % increase vs baseline
	Occurrences  int     `json:"occurrences"` // how many times this property+value appeared
	Conversions  int     `json:"conversions"` // how many converted
	Rate         float64 `json:"rate"`        // conversion rate for this segment
	BaselineRate float64 `json:"baseline_rate"`
	Significant  bool    `json:"significant"`
}

type correlationEvent struct {
	EventID    string `db:"event_id"`
	Timestamp  int64  `db:"timestamp"`
	SessionID  string `db:"session_id"`
	EventType  string `db:"event_type"`
	Properties string `db:"properties"`
}

// CorrelationAnalysis finds properties correlated with a target event.
// targetEvent: the event type to correlate with (e.g., "signup")
// Returns properties whose presence significantly increases the rate of the target event.
//
// O12: the read is admission-gated and bounded — the SQL carries a LIMIT
// of budget+1 rows and reading past the declared row budget converts to a
// labeled refusal instead of an unbounded slice (guard.go
// boundedRangeQuery). Below the ceiling the result set is unchanged.
func (s *StatsService) CorrelationAnalysis(ctx context.Context, siteID, targetEvent string, from, to time.Time, active ...*FilterBuilder) ([]Correlation, error) {
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
	// Get all events with properties in the time range
	rows, err := boundedRangeQuery[correlationEvent](qctx, s,
		`SELECT event_id, timestamp, session_id, event_type, COALESCE(properties, '') AS properties
		 FROM events
		 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
		 `+fSQL+` ORDER BY session_id ASC, timestamp ASC, event_id ASC`,
		baseParams(siteID, fromMs, toMs, filters)...,
	)
	if err != nil {
		return nil, fmt.Errorf("correlation query: %w", err)
	}

	// Build session-level data
	type sessionData struct {
		properties map[string]string
		converted  bool
	}
	sessions := make(map[string]*sessionData)

	for _, e := range rows {
		if err := qctx.Err(); err != nil {
			return nil, err
		}
		sd, ok := sessions[e.SessionID]
		if !ok {
			sd = &sessionData{properties: make(map[string]string)}
			sessions[e.SessionID] = sd
		}

		if e.EventType == targetEvent {
			sd.converted = true
		}

		if e.Properties != "" {
			var props map[string]any
			if json.Unmarshal([]byte(e.Properties), &props) == nil {
				if len(props) > 50 {
					return nil, fmt.Errorf("correlation properties exceed supported per-event work bound")
				}
				for k, v := range props {
					if err := qctx.Err(); err != nil {
						return nil, err
					}
					sd.properties[k] = fmt.Sprintf("%v", v)
				}
			}
		}
	}

	totalSessions := len(sessions)
	if totalSessions == 0 {
		return nil, nil
	}

	// Compute baseline conversion rate
	totalConverted := 0
	for _, sd := range sessions {
		if err := qctx.Err(); err != nil {
			return nil, err
		}
		if sd.converted {
			totalConverted++
		}
	}
	baselineRate := float64(totalConverted) / float64(totalSessions)

	// For each property+value, compute conversion rate
	type pvKey struct{ prop, val string }
	type pvStats struct {
		occurrences int
		conversions int
	}
	pvMap := make(map[pvKey]*pvStats)

	for _, sd := range sessions {
		if err := qctx.Err(); err != nil {
			return nil, err
		}
		for k, v := range sd.properties {
			if err := qctx.Err(); err != nil {
				return nil, err
			}
			key := pvKey{k, v}
			pv, ok := pvMap[key]
			if !ok {
				pv = &pvStats{}
				pvMap[key] = pv
			}
			pv.occurrences++
			if sd.converted {
				pv.conversions++
			}
		}
	}

	// Build correlations
	var correlations []Correlation
	for key, pv := range pvMap {
		if err := qctx.Err(); err != nil {
			return nil, err
		}
		if pv.occurrences < 5 {
			continue // need minimum sample
		}
		rate := float64(pv.conversions) / float64(pv.occurrences)
		uplift := 0.0
		if baselineRate > 0 {
			uplift = ((rate - baselineRate) / baselineRate) * 100
		}

		significant := correlationSignificant(pv.occurrences, pv.conversions, totalSessions, totalConverted)

		correlations = append(correlations, Correlation{
			Property:     key.prop,
			Value:        key.val,
			Uplift:       math.Round(uplift*10) / 10,
			Occurrences:  pv.occurrences,
			Conversions:  pv.conversions,
			Rate:         math.Round(rate*1000) / 10,
			BaselineRate: math.Round(baselineRate*1000) / 10,
			Significant:  significant,
		})
	}

	// Sort by absolute uplift descending
	if err := sortWithContext(qctx, correlations, func(a, b Correlation) bool {
		if math.Abs(a.Uplift) != math.Abs(b.Uplift) {
			return math.Abs(a.Uplift) > math.Abs(b.Uplift)
		}
		if a.Property != b.Property {
			return a.Property < b.Property
		}
		return a.Value < b.Value
	}); err != nil {
		return nil, err
	}

	if len(correlations) > 20 {
		correlations = correlations[:20]
	}

	if err := qctx.Err(); err != nil {
		return nil, err
	}
	return correlations, nil
}

func correlationSignificant(segment, converted, total, totalConverted int) bool {
	if segment < 10 || total < 20 || total <= segment {
		return false
	}
	p := float64(totalConverted) / float64(total)
	se := math.Sqrt(p * (1 - p) * (1/float64(segment) + 1/float64(total-segment)))
	if se <= 0 {
		return false
	}
	difference := float64(converted)/float64(segment) - float64(totalConverted-converted)/float64(total-segment)
	return math.Abs(difference)/se > 1.96
}
