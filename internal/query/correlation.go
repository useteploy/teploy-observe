package query

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

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
	SessionID  string `db:"session_id"`
	EventType  string `db:"event_type"`
	Properties string `db:"properties"`
}

// CorrelationAnalysis finds properties correlated with a target event.
// targetEvent: the event type to correlate with (e.g., "signup")
// Returns properties whose presence significantly increases the rate of the target event.
func (s *StatsService) CorrelationAnalysis(ctx context.Context, siteID, targetEvent string, from, to time.Time) ([]Correlation, error) {
	fromMs := from.UnixMilli()
	toMs := to.UnixMilli()

	// Get all events with properties in the time range
	rows, err := nucleus.Query[correlationEvent](ctx, s.db.SQL(),
		`SELECT session_id, event_type, COALESCE(properties, '') AS properties
		 FROM events
		 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
		 ORDER BY session_id`,
		siteID, fromMs, toMs,
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
				for k, v := range props {
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
		for k, v := range sd.properties {
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
		if pv.occurrences < 5 {
			continue // need minimum sample
		}
		rate := float64(pv.conversions) / float64(pv.occurrences)
		uplift := 0.0
		if baselineRate > 0 {
			uplift = ((rate - baselineRate) / baselineRate) * 100
		}

		// Simple significance: z-test for proportions
		significant := false
		if pv.occurrences >= 10 && totalSessions >= 20 {
			pooledP := float64(totalConverted) / float64(totalSessions)
			se := math.Sqrt(pooledP * (1 - pooledP) * (1.0/float64(pv.occurrences) + 1.0/float64(totalSessions-pv.occurrences)))
			if se > 0 {
				z := math.Abs(rate-baselineRate) / se
				significant = z > 1.96 // p < 0.05
			}
		}

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
	sort.Slice(correlations, func(i, j int) bool {
		return math.Abs(correlations[i].Uplift) > math.Abs(correlations[j].Uplift)
	})

	if len(correlations) > 20 {
		correlations = correlations[:20]
	}

	return correlations, nil
}
