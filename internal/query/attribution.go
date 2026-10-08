// Multi-touch UTM attribution (Wave 2 / W2.C — Umami gap #1).
//
// We don't add a schema; the existing events table already carries
// utm_source/medium/campaign/term/content per pageview. Sessions are
// reconstructed at query time by grouping events by session_id and
// walking them in timestamp order. Three attribution models are
// supported:
//
//	first  — credit goes to the FIRST non-empty utm_source seen in the session.
//	last   — credit goes to the LAST non-empty utm_source seen in the session.
//	linear — 1/N credit per unique utm_source seen in the session.
//
// v1 treats *every* session as a "conversion" (basic mode). A future
// goal_event filter can layer on top by restricting which sessions we
// credit (see ConversionRule below).
//
// Sessions with no utm_source on any event are bucketed as "(direct)".
// We use float counters because linear attribution can split credit
// fractionally across sources.

package query

import (
	"context"
	"fmt"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/queryguard"
)

// Attribution model identifiers accepted by the API.
const (
	AttributionFirstTouch = "first"
	AttributionLastTouch  = "last"
	AttributionLinear     = "linear"
)

// directBucket is the source label used for sessions that arrived
// without any utm_source tagging on any event in the session.
const directBucket = "(direct)"

// AttributionRow is a per-source aggregate result. Sessions and
// Conversions are floats because the linear model can assign
// fractional credit (1/N).
type AttributionRow struct {
	Source        string  `json:"source"`
	Sessions      float64 `json:"sessions"`
	Conversions   float64 `json:"conversions"`
	ConversionPct float64 `json:"conversion_pct"`
}

// AttributionService runs multi-touch UTM attribution queries. Owns its
// own *nucleus.Client handle so callers don't have to thread the DB
// through every signature.
type AttributionService struct {
	db         *nucleus.Client
	stats      *StatsService
	readEvents func(context.Context, string, ...any) ([]attributionEvent, error)
}

// NewAttributionService constructs an AttributionService bound to the
// given DB client.
func NewAttributionService(db *nucleus.Client) *AttributionService {
	return &AttributionService{db: db, stats: NewStatsService(db)}
}

// IsValidModel returns true if the given identifier is one of the
// supported attribution models. Used by the HTTP handler to 400 early.
func IsValidModel(model string) bool {
	switch model {
	case AttributionFirstTouch, AttributionLastTouch, AttributionLinear:
		return true
	}
	return false
}

// attributionEvent is the minimal projection we need from the events
// table. We pull only utm_source (other UTM fields aren't credited in
// v1) and timestamp for ordering.
type attributionEvent struct {
	SessionID string `db:"session_id"`
	UTMSource string `db:"utm_source"`
	Timestamp int64  `db:"timestamp"`
	EventID   string `db:"event_id"`
}

// AttributionByModel computes per-source attribution credit for the
// given site/window using the named model. fromMs/toMs are inclusive of
// from and exclusive of to (matches the rest of the stats API).
//
// Returns rows sorted by Sessions desc. Empty result on no traffic; no
// error on empty windows.
func (s *AttributionService) AttributionByModel(ctx context.Context, siteID, model string, fromMs, toMs int64) ([]AttributionRow, error) {
	if !IsValidModel(model) {
		return nil, fmt.Errorf("attribution: invalid model %q", model)
	}

	qctx, finish, err := s.stats.beginQuery(ctx, siteID)
	if err != nil {
		return nil, err
	}
	defer finish()
	if err := s.stats.validateWindow(time.UnixMilli(fromMs), time.UnixMilli(toMs)); err != nil {
		return nil, err
	}
	from := fromMs
	to := toMs

	// Pull the minimal projection. Ordered for deterministic walks.
	// Per dogfood finding #24 we scan natively (no CAST(... AS TEXT))
	// and per finding #6 the BIGINT bound is wrapped with CAST so the
	// SimpleProtocol-stringified parameter is compared as a number.
	q := `SELECT event_id, session_id, COALESCE(utm_source, '') AS utm_source, timestamp
 FROM events WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
 ORDER BY session_id ASC, timestamp ASC, event_id ASC`
	var rows []attributionEvent
	if s.readEvents != nil {
		rows, err = s.readEvents(qctx, q, siteID, from, to)
		if err == nil {
			err = s.stats.accountScan(qctx, int64(len(rows)))
		}
	} else {
		rows, err = boundedRangeQuery[attributionEvent](qctx, s.stats, q, siteID, from, to)
	}
	if err != nil {
		return nil, fmt.Errorf("attribution query: %w", err)
	}

	result, err := computeAttribution(qctx, rows, model, s.stats.checkPostRead)
	if err != nil {
		return nil, err
	}
	if err := qctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// ComputeAttribution is the pure (no-DB) attribution kernel. It accepts
// a flat slice of (session_id, utm_source, timestamp) tuples in any
// order and returns the per-source aggregate for the requested model.
//
// Exposed so unit tests can exercise the math without spinning up a DB.
func ComputeAttribution(events []attributionEvent, model string) []AttributionRow {
	result, _ := computeAttribution(context.Background(), events, model, nil)
	return result
}

// Production and the pure oracle share the same stable, cancellable kernel.
// Each map/slice has at most the admitted event cardinality; work is O(n log n).
func computeAttribution(ctx context.Context, events []attributionEvent, model string, checkpoint func(context.Context, string) error) ([]AttributionRow, error) {
	check := func(phase string) error {
		if checkpoint != nil {
			return checkpoint(ctx, phase)
		}
		return ctx.Err()
	}
	sessions := make(map[string][]attributionEvent)
	for _, e := range events {
		if err := check("attribution-copy"); err != nil {
			return nil, err
		}
		sessions[e.SessionID] = append(sessions[e.SessionID], e)
	}
	// Session order also pins floating-point accumulation independently of map iteration.
	ids := make([]string, 0, len(sessions))
	for id := range sessions {
		if err := check("attribution-sessions"); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := sortWithContext(ctx, ids, func(a, b string) bool { return a < b }); err != nil {
		return nil, err
	}
	totals := make(map[string]float64)
	for _, id := range ids {
		bucket := sessions[id]
		if err := check("attribution-sort"); err != nil {
			return nil, err
		}
		var canceled error
		err := sortWithContext(ctx, bucket, func(a, b attributionEvent) bool {
			if canceled == nil {
				canceled = check("attribution-sort-compare")
			}
			if a.Timestamp != b.Timestamp {
				return a.Timestamp < b.Timestamp
			}
			return a.EventID < b.EventID
		})
		if err != nil {
			return nil, err
		}
		if canceled != nil {
			return nil, canceled
		}
		uniq := make(map[string]struct{})
		first, last := "", ""
		for _, e := range bucket {
			if err := check("attribution-credit"); err != nil {
				return nil, err
			}
			if e.UTMSource == "" {
				continue
			}
			if first == "" {
				first = e.UTMSource
			}
			last = e.UTMSource
			uniq[e.UTMSource] = struct{}{}
		}
		if first == "" {
			totals[directBucket]++
			continue
		}
		switch model {
		case AttributionFirstTouch:
			totals[first]++
		case AttributionLastTouch:
			totals[last]++
		case AttributionLinear:
			for src := range uniq {
				if err := check("attribution-accumulate"); err != nil {
					return nil, err
				}
				totals[src] += 1 / float64(len(uniq))
			}
		}
	}
	out := make([]AttributionRow, 0, len(totals))
	for src, n := range totals {
		if err := check("attribution-output"); err != nil {
			return nil, err
		}
		out = append(out, AttributionRow{Source: src, Sessions: n, Conversions: n, ConversionPct: 100})
	}
	var canceled error
	err := sortWithContext(ctx, out, func(a, b AttributionRow) bool {
		if canceled == nil {
			canceled = check("attribution-final-sort")
		}
		if a.Sessions != b.Sessions {
			return a.Sessions > b.Sessions
		}
		return a.Source < b.Source
	})
	if err != nil {
		return nil, err
	}
	if canceled != nil {
		return nil, canceled
	}
	return out, check("attribution-complete")
}

// creditsForSession returns map[source] = credit for one session under
// the given model. Sessions with no utm_source on any event get full
// credit attributed to "(direct)".
//
// Caller must pass events already sorted by timestamp ascending.
func creditsForSession(evts []attributionEvent, model string) map[string]float64 {
	credits := make(map[string]float64)
	if len(evts) == 0 {
		return credits
	}

	// Collect non-empty utm_source values in chronological order, plus
	// the unique set for linear attribution.
	var ordered []string
	uniq := make(map[string]struct{})
	for _, e := range evts {
		if e.UTMSource == "" {
			continue
		}
		ordered = append(ordered, e.UTMSource)
		uniq[e.UTMSource] = struct{}{}
	}

	if len(ordered) == 0 {
		credits[directBucket] = 1.0
		return credits
	}

	switch model {
	case AttributionFirstTouch:
		credits[ordered[0]] = 1.0
	case AttributionLastTouch:
		credits[ordered[len(ordered)-1]] = 1.0
	case AttributionLinear:
		share := 1.0 / float64(len(uniq))
		for src := range uniq {
			credits[src] = share
		}
	}
	return credits
}

// TimeRangeMs is a small helper that mirrors the rest of the query
// package's window-defaulting behavior (last 24h on zero values). Used
// by the attribution HTTP handler.
func TimeRangeMs(from, to time.Time) (int64, int64) {
	if from.IsZero() {
		from = time.Now().UTC().Add(-24 * time.Hour)
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}
	return from.UnixMilli(), to.UnixMilli()
}

func (s *AttributionService) WithQueryGuard(l *queryguard.Limiter, b queryguard.Budgets) *AttributionService {
	s.stats.WithQueryGuard(l, b)
	return s
}
