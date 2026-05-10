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
	"sort"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
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
	db *nucleus.Client
}

// NewAttributionService constructs an AttributionService bound to the
// given DB client.
func NewAttributionService(db *nucleus.Client) *AttributionService {
	return &AttributionService{db: db}
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

	from := dbutil.IntParam(fromMs)
	to := dbutil.IntParam(toMs)

	// Pull the minimal projection. Ordered for deterministic walks.
	// Per dogfood finding #24 we scan natively (no CAST(... AS TEXT))
	// and per finding #6 the BIGINT bound is wrapped with CAST so the
	// SimpleProtocol-stringified parameter is compared as a number.
	rows, err := nucleus.Query[attributionEvent](ctx, s.db.SQL(),
		`SELECT session_id, COALESCE(utm_source, '') AS utm_source, timestamp
		   FROM events
		  WHERE site_id = $1
		    AND timestamp >= CAST($2 AS BIGINT)
		    AND timestamp <  CAST($3 AS BIGINT)
		  ORDER BY session_id, timestamp ASC`,
		siteID, from, to,
	)
	if err != nil {
		return nil, fmt.Errorf("attribution query: %w", err)
	}

	return ComputeAttribution(rows, model), nil
}

// ComputeAttribution is the pure (no-DB) attribution kernel. It accepts
// a flat slice of (session_id, utm_source, timestamp) tuples in any
// order and returns the per-source aggregate for the requested model.
//
// Exposed so unit tests can exercise the math without spinning up a DB.
func ComputeAttribution(events []attributionEvent, model string) []AttributionRow {
	if len(events) == 0 {
		return []AttributionRow{}
	}

	// Bucket by session and sort each bucket by timestamp. We sort here
	// (rather than relying on the SQL ORDER BY) so unit tests can pass
	// arbitrary input ordering.
	sessions := make(map[string][]attributionEvent)
	for _, e := range events {
		sessions[e.SessionID] = append(sessions[e.SessionID], e)
	}
	for sid := range sessions {
		bucket := sessions[sid]
		sort.SliceStable(bucket, func(i, j int) bool {
			return bucket[i].Timestamp < bucket[j].Timestamp
		})
		sessions[sid] = bucket
	}

	totals := make(map[string]float64)
	conversions := make(map[string]float64)
	totalSessions := 0

	for _, evts := range sessions {
		totalSessions++
		credits := creditsForSession(evts, model)
		for src, weight := range credits {
			totals[src] += weight
			// v1: every session is a conversion. Future: gate on a
			// ConversionRule (e.g. "fired event_type=signup").
			conversions[src] += weight
		}
	}

	out := make([]AttributionRow, 0, len(totals))
	for src, sess := range totals {
		conv := conversions[src]
		pct := 0.0
		if sess > 0 {
			pct = conv / sess * 100
		}
		out = append(out, AttributionRow{
			Source:        src,
			Sessions:      sess,
			Conversions:   conv,
			ConversionPct: pct,
		})
	}

	// Sort by sessions desc for stable presentation; tie-break by
	// source name so test output is deterministic.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sessions != out[j].Sessions {
			return out[i].Sessions > out[j].Sessions
		}
		return out[i].Source < out[j].Source
	})

	return out
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
