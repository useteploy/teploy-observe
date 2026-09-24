package query

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
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

// funnelWalker is the incremental form of the deterministic funnel core.
// Streaming rows in the pinned (timestamp, event_id) total order and
// pushing them one at a time is EXACTLY the sorted-slice single pass of
// walkFunnelEvents — same greedy chain, same window boundary, same
// exclusions — with O(1) state per entity instead of the entity's whole
// event slice (O12: the walk no longer materializes the range).
type funnelWalker struct {
	steps      []FunnelStep
	exclusions []FunnelStep
	windowMs   int64
	counts     []int
	stepIdx    int
	done       bool
	e0ts       int64
}

func newFunnelWalker(steps []FunnelStep, windowMs int64, exclusions []FunnelStep) *funnelWalker {
	return &funnelWalker{
		steps:      steps,
		exclusions: exclusions,
		windowMs:   windowMs,
		counts:     make([]int, len(steps)),
	}
}

// push consumes one event in (timestamp, event_id) order. Once done
// (chain complete, out of window, or disqualified) further pushes are
// no-ops, matching the sorted walk's break statements.
func (w *funnelWalker) push(e funnelEvent) {
	if w.done {
		return
	}
	if w.stepIdx == 0 {
		if matchesStep(e, w.steps[0]) {
			w.counts[0]++
			w.stepIdx = 1
			w.e0ts = e.Timestamp
			if len(w.steps) == 1 {
				w.done = true
			}
		}
		return
	}
	if w.windowMs > 0 && e.Timestamp > w.e0ts+w.windowMs {
		w.done = true // out of window; every later event is further out
		return
	}
	if matchesAnyStep(e, w.exclusions) {
		w.done = true // disqualified from further progression
		return
	}
	if matchesStep(e, w.steps[w.stepIdx]) {
		w.counts[w.stepIdx]++
		w.stepIdx++
		if w.stepIdx >= len(w.steps) {
			w.done = true
		}
	}
}

// walkFunnelEvents is the deterministic funnel core over ONE entity's
// events (storage-free; pinned by funnel_semantics_test.go and the
// internal/session O04 oracle tables). Implemented over funnelWalker so
// the sorted form and the O12 streaming form cannot diverge:
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
	w := newFunnelWalker(steps, windowMs, exclusions)
	for _, e := range events {
		w.push(e)
	}
	return w.counts
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
//
// O12: the read is a budgeted stream (guard.go) in the walk's pinned
// (timestamp, event_id) order — one funnelWalker per entity, O(entities)
// memory — under the declared window clamp, row budget and time budget,
// with per-site/global concurrency admission. It replaces the unbounded
// whole-range SELECT the funnel used to run.
func (s *StatsService) FunnelWithOptions(ctx context.Context, siteID string, from, to time.Time, steps []FunnelStep, opts FunnelOptions) ([]FunnelResult, error) {
	if len(steps) == 0 {
		return nil, nil
	}
	entity := opts.Entity
	if err := ValidateEntity(entity); err != nil {
		return nil, err
	}

	from = s.clampWindow(from, to)
	qctx, finish, err := s.beginQuery(ctx, siteID)
	if err != nil {
		return nil, err
	}
	defer finish()

	walkers := make(map[string]*funnelWalker)
	err = s.streamEvents(qctx, siteID, from.UnixMilli(), to.UnixMilli(), "", func(row pgx.Row) error {
		e, err := scanFunnelEvent(row)
		if err != nil {
			return fmt.Errorf("funnel scan: %w", err)
		}
		id, ok := entityKeyOf(entity, e)
		if !ok {
			return nil
		}
		w, seen := walkers[id]
		if !seen {
			w = newFunnelWalker(steps, opts.ConversionWindowMs, opts.Exclusions)
			walkers[id] = w
		}
		w.push(e)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("funnel query: %w", err)
	}

	counts := make([]int, len(steps))
	for _, w := range walkers {
		for i, c := range w.counts {
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
//
// O12: same budgeted stream as FunnelWithOptions, with the breakdown
// expression riding each row. One determinism note versus the old
// whole-range read: an entity spanning several breakdown values keeps the
// value of its EARLIEST event in the pinned total order — the old code
// kept whichever row an unordered scan happened to deliver first.
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

	from = s.clampWindow(from, to)
	qctx, finish, err := s.beginQuery(ctx, siteID)
	if err != nil {
		return nil, err
	}
	defer finish()

	// Group walkers by (breakdown, entity); an entity can span different
	// breakdown values in theory; keep the first seen in stream order.
	type key struct{ Breakdown, Entity string }
	grouped := make(map[key]*funnelWalker)
	entityBreakdown := make(map[string]string)
	err = s.streamEvents(qctx, siteID, from.UnixMilli(), to.UnixMilli(), col+" AS breakdown", func(row pgx.Row) error {
		e, err := scanFunnelEventWithBreakdown(row)
		if err != nil {
			return fmt.Errorf("funnel breakdown scan: %w", err)
		}
		eid, ok := entityKeyOf(entity, e.funnelEvent)
		if !ok {
			return nil
		}
		if _, seen := entityBreakdown[eid]; !seen {
			entityBreakdown[eid] = e.Breakdown
		}
		k := key{Breakdown: entityBreakdown[eid], Entity: eid}
		w, seen := grouped[k]
		if !seen {
			w = newFunnelWalker(steps, opts.ConversionWindowMs, opts.Exclusions)
			grouped[k] = w
		}
		w.push(e.funnelEvent)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("funnel breakdown query: %w", err)
	}

	// Sum step counts per breakdown.
	perBreakdown := make(map[string][]int) // breakdown -> stepCounts[]
	for k, w := range grouped {
		counts, ok := perBreakdown[k.Breakdown]
		if !ok {
			counts = make([]int, len(steps))
			perBreakdown[k.Breakdown] = counts
		}
		for i, c := range w.counts {
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
