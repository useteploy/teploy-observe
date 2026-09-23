package query

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"
)

// FunnelStep defines one step in a funnel (event type or pathname match).
type FunnelStep struct {
	Type  string `json:"type"`  // "event" or "page"
	Value string `json:"value"` // event_type name or pathname
}

// FunnelResult is the conversion data for each step. Entity and
// EntityLimitation are the O03 D2/D11 labels: which entity was grouped
// (visitor-estimate | visit | person) and the one-line honest limitation
// of that entity. They ride on every row so the array response stays
// backward-compatible while naming its entity.
type FunnelResult struct {
	Step             FunnelStep `json:"step"`
	Visitors         int        `json:"visitors"`
	Conversion       float64    `json:"conversion"` // % of step 1 visitors
	DropOff          float64    `json:"drop_off"`   // % dropped from previous step
	Entity           string     `json:"entity,omitempty"`
	EntityLimitation string     `json:"entity_limitation,omitempty"`
}

// FunnelOptions is the O04 semantics surface. Zero value = pre-O04
// behavior: visitor-estimate entity, unbounded conversion window (the
// query range is the only bound), no exclusions.
type FunnelOptions struct {
	// Entity is the grouped entity (entity.go). "" = visitor-estimate.
	Entity string
	// ConversionWindowMs bounds the funnel traversal: the last step's
	// event must occur within this many milliseconds of the entity's
	// FIRST step-0 event. The boundary ts == e0 + window is IN; beyond it
	// is OUT. <= 0 means unbounded (bounded only by the query range).
	ConversionWindowMs int64
	// Exclusions are disqualifying steps: after an entity has matched
	// step 0, the first event matching any exclusion (strictly after the
	// step-0 event in the pinned total order) terminates its progression
	// — steps already matched are kept, nothing further counts. Events
	// at or before the step-0 event never disqualify.
	Exclusions []FunnelStep
}

// funnelEvent is the minimal event data needed for funnel computation.
// SessionID/VisitID/DistinctID are the three entity columns (O03);
// Timestamp is the STORED timestamp, which era-1 sets to ingestion time
// (the wire protocol carries no event time — ADR D8).
type funnelEvent struct {
	EventID    string `db:"event_id"`
	SessionID  string `db:"session_id"`
	VisitID    string `db:"visit_id"`
	DistinctID string `db:"distinct_id"`
	EventType  string `db:"event_type"`
	Pathname   string `db:"pathname"`
	Timestamp  int64  `db:"timestamp"`
}

// funnelEntityColumns selects the entity columns for a funnel-shaped
// events read. All three ids ride every row so the entity dispatch is a
// Go-side grouping decision, not a query-shape change.
const funnelEntityColumns = `event_id, session_id, visit_id, COALESCE(distinct_id, '') AS distinct_id,
		event_type, COALESCE(pathname, '') AS pathname, timestamp`

// walkFunnelEvents is the deterministic funnel core over ONE entity's
// events (storage-free; pinned by funnel_semantics_test.go and the
// internal/session O04 oracle tables):
//
//   - order: events sort by (timestamp, event_id) ascending — a total
//     order, since event_id is unique within a site. This is the pinned
//     timestamp-tie handling.
//   - progression: a greedy earliest chain. stepCounts[i] increments when
//     the walk finds an event matching step i strictly after the event
//     that satisfied step i-1 in that total order. Each step consumes a
//     distinct event (repeated steps re-fire on later events), and
//     conversion keys on the FIRST progression (leftmost chain).
//   - conversion window: with windowMs > 0 the walk stops scanning once
//     ts > e0.ts + windowMs, where e0 is the matched step-0 event — the
//     boundary ts == e0 + window is inside.
//   - exclusions: after e0, the first event matching any exclusion ends
//     the walk (progression kept to that point).
func walkFunnelEvents(events []funnelEvent, steps []FunnelStep, windowMs int64, exclusions []FunnelStep) []int {
	sort.Slice(events, func(i, j int) bool {
		if events[i].Timestamp != events[j].Timestamp {
			return events[i].Timestamp < events[j].Timestamp
		}
		return events[i].EventID < events[j].EventID
	})
	stepCounts := make([]int, len(steps))
	stepIdx := 0
	var e0ts int64
	for _, e := range events {
		if stepIdx == 0 {
			if matchesStep(e, steps[0]) {
				stepCounts[0]++
				stepIdx = 1
				e0ts = e.Timestamp
				if len(steps) == 1 {
					break
				}
			}
			continue
		}
		if windowMs > 0 && e.Timestamp > e0ts+windowMs {
			break // out of window; every later event is further out
		}
		if matchesAnyStep(e, exclusions) {
			break // disqualified from further progression
		}
		if matchesStep(e, steps[stepIdx]) {
			stepCounts[stepIdx]++
			stepIdx++
			if stepIdx >= len(steps) {
				break
			}
		}
	}
	return stepCounts
}

// matchesAnyStep reports whether the event matches any exclusion step.
func matchesAnyStep(e funnelEvent, steps []FunnelStep) bool {
	for _, s := range steps {
		if matchesStep(e, s) {
			return true
		}
	}
	return false
}

// Funnel computes multi-step conversion for the given steps, grouped on
// the visitor-estimate entity (pre-O04 behavior exactly).
func (s *StatsService) Funnel(ctx context.Context, siteID string, from, to time.Time, steps []FunnelStep) ([]FunnelResult, error) {
	return s.FunnelWithOptions(ctx, siteID, from, to, steps, FunnelOptions{})
}

// FunnelWithOptions is Funnel with the O04 entity/window/exclusion
// semantics. See FunnelOptions for the pinned meaning of each field.
func (s *StatsService) FunnelWithOptions(ctx context.Context, siteID string, from, to time.Time, steps []FunnelStep, opts FunnelOptions) ([]FunnelResult, error) {
	if len(steps) == 0 {
		return nil, nil
	}
	entity := opts.Entity
	if err := ValidateEntity(entity); err != nil {
		return nil, err
	}

	fromMs := from.UnixMilli()
	toMs := to.UnixMilli()

	// Fetch all events in the time range for this site. Ordering happens
	// in the walk (the SQL order is just a scan order).
	rows, err := nucleus.Query[funnelEvent](ctx, s.db.SQL(),
		`SELECT `+funnelEntityColumns+`
		 FROM events
		 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3`,
		siteID, fromMs, toMs,
	)
	if err != nil {
		return nil, fmt.Errorf("funnel query: %w", err)
	}

	counts := make([]int, len(steps))
	for _, events := range groupFunnelByEntity(rows, entity) {
		entityCounts := walkFunnelEvents(events, steps, opts.ConversionWindowMs, opts.Exclusions)
		for i, c := range entityCounts {
			counts[i] += c
		}
	}

	results := make([]FunnelResult, len(steps))
	for i, st := range steps {
		results[i] = FunnelResult{
			Step:             st,
			Visitors:         counts[i],
			Entity:           entityLabel(entity),
			EntityLimitation: EntityLimitation(entity),
		}
		if counts[0] > 0 {
			results[i].Conversion = float64(counts[i]) / float64(counts[0]) * 100
		}
		if i > 0 && counts[i-1] > 0 {
			results[i].DropOff = float64(counts[i-1]-counts[i]) / float64(counts[i-1]) * 100
		}
	}

	return results, nil
}

// entityLabel normalizes the empty default to its honest name.
func entityLabel(entity string) string {
	if entity == "" {
		return EntityVisitorEstimate
	}
	return entity
}

// FunnelBreakdownResult groups funnel outcomes by a property dimension.
type FunnelBreakdownResult struct {
	Breakdown string         `json:"breakdown"`
	Results   []FunnelResult `json:"results"`
}

// FunnelByBreakdown computes the funnel separately for each distinct value of
// `breakdownBy` (e.g. "browser", "country", "device", "os"). Unsupported
// breakdowns return an error. Groups with fewer than minSize entities are dropped.
func (s *StatsService) FunnelByBreakdown(ctx context.Context, siteID string, from, to time.Time, steps []FunnelStep, breakdownBy string, minSize int) ([]FunnelBreakdownResult, error) {
	return s.FunnelByBreakdownWithOptions(ctx, siteID, from, to, steps, breakdownBy, minSize, FunnelOptions{})
}

// FunnelByBreakdownWithOptions is FunnelByBreakdown with the O04 entity /
// window / exclusion semantics applied per breakdown group.
func (s *StatsService) FunnelByBreakdownWithOptions(ctx context.Context, siteID string, from, to time.Time, steps []FunnelStep, breakdownBy string, minSize int, opts FunnelOptions) ([]FunnelBreakdownResult, error) {
	if len(steps) == 0 {
		return nil, nil
	}
	entity := opts.Entity
	if err := ValidateEntity(entity); err != nil {
		return nil, err
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
		funnelEvent
		Breakdown string `db:"breakdown"`
	}
	rows, err := nucleus.Query[breakdownRow](ctx, s.db.SQL(),
		fmt.Sprintf(`SELECT %s, %s AS breakdown
		 FROM events
		 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3`, funnelEntityColumns, col),
		siteID, fromMs, toMs,
	)
	if err != nil {
		return nil, fmt.Errorf("funnel breakdown query: %w", err)
	}

	// Group events by (breakdown, entity). An entity can span different
	// breakdown values in theory; keep the first seen.
	type key struct{ Breakdown, Entity string }
	grouped := make(map[key][]funnelEvent)
	entityBreakdown := make(map[string]string)
	for _, r := range rows {
		eid, ok := entityKeyOf(entity, r.funnelEvent)
		if !ok {
			continue
		}
		if _, seen := entityBreakdown[eid]; !seen {
			entityBreakdown[eid] = r.Breakdown
		}
		k := key{Breakdown: entityBreakdown[eid], Entity: eid}
		grouped[k] = append(grouped[k], r.funnelEvent)
	}

	// Walk per-breakdown.
	perBreakdown := make(map[string][]int) // breakdown -> stepCounts[]
	for k, events := range grouped {
		counts, ok := perBreakdown[k.Breakdown]
		if !ok {
			counts = make([]int, len(steps))
			perBreakdown[k.Breakdown] = counts
		}
		entityCounts := walkFunnelEvents(events, steps, opts.ConversionWindowMs, opts.Exclusions)
		for i, c := range entityCounts {
			counts[i] += c
		}
	}

	out := make([]FunnelBreakdownResult, 0, len(perBreakdown))
	for b, counts := range perBreakdown {
		if counts[0] < minSize {
			continue
		}
		res := make([]FunnelResult, len(steps))
		for i, st := range steps {
			res[i] = FunnelResult{Step: st, Visitors: counts[i], Entity: entityLabel(entity), EntityLimitation: EntityLimitation(entity)}
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
