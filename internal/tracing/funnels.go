package tracing

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// FunnelStep is the per-step result of a trace funnel computation.
//
// ConversionPct is the percentage of traces that reached this step relative
// to the previous step. The first step always has ConversionPct = 100.0 when
// any traces reach it (it is the denominator for step 1). MedianGapMs and
// P95GapMs are the median / p95 gap between this step's first occurrence and
// the previous step's first occurrence within a trace, in milliseconds. Both
// are zero on the first step.
type FunnelStep struct {
	Operation     string  `json:"operation"`
	Count         int64   `json:"count"`
	ConversionPct float64 `json:"conversion_pct"`
	MedianGapMs   int64   `json:"median_gap_ms"`
	P95GapMs      int64   `json:"p95_gap_ms"`
}

// FunnelResult is the funnel computation result returned to clients.
type FunnelResult struct {
	Steps       []FunnelStep `json:"steps"`
	TotalTraces int64        `json:"total_traces"`
}

// funnelSpan is the minimal projection of a span needed for funnel
// computation. Kept separate from Span so we can cast columns explicitly
// (see scan-row pattern in query.go) and skip the JSON columns entirely.
type funnelSpan struct {
	TraceID   string `db:"trace_id"`
	OpName    string `db:"operation_name"`
	StartTime string `db:"start_time"`
}

// FunnelByOps computes a trace funnel for the given ordered operation list
// in the time window [fromMs, toMs). It returns a FunnelResult with one
// FunnelStep per input op.
//
// An op is considered to "match" a trace if any span in the trace has
// operation_name == op AND that span's start_time is strictly after the
// previously-matched op's start_time (within the same trace). The first op
// matches the earliest span with that operation_name. This mirrors how
// PostHog / Amplitude treat ordered conversion funnels.
//
// Empty ops returns an empty result without hitting the database.
func (q *QueryService) FunnelByOps(ctx context.Context, siteID string, ops []string, fromMs, toMs int64) (FunnelResult, error) {
	if len(ops) == 0 {
		return FunnelResult{Steps: []FunnelStep{}}, nil
	}
	if siteID == "" {
		return FunnelResult{}, fmt.Errorf("siteID required")
	}

	// Build an IN-list that filters by exactly the ops we care about so
	// large traces don't pull every span just to be discarded in Go. The
	// time-window predicate uses CAST(... AS BIGINT) per query.go's
	// established pgwire-text-column pattern.
	placeholders := make([]string, len(ops))
	params := []any{siteID, dbutil.IntParam(fromMs), dbutil.IntParam(toMs)}
	for i, op := range ops {
		placeholders[i] = fmt.Sprintf("$%d", i+4)
		params = append(params, op)
	}

	sqlText := fmt.Sprintf(
		`SELECT trace_id, operation_name, CAST(start_time AS TEXT) AS start_time
		 FROM spans
		 WHERE site_id = $1
		   AND start_time >= $2
		   AND start_time < $3
		   AND operation_name IN (%s)`,
		strings.Join(placeholders, ","),
	)

	rows, err := nucleus.Query[funnelSpan](ctx, q.db.SQL(), sqlText, params...)
	if err != nil {
		return FunnelResult{}, fmt.Errorf("funnel query: %w", err)
	}

	return computeFunnel(rows, ops), nil
}

// computeFunnel is the pure-Go funnel walker. Exported (lowercase) for the
// package to test without spinning up a database. Splitting the query and
// the algorithm makes each independently testable, matching the pattern in
// aggregateRollups.
func computeFunnel(rows []funnelSpan, ops []string) FunnelResult {
	if len(ops) == 0 {
		return FunnelResult{Steps: []FunnelStep{}}
	}

	// Bucket by trace_id, keep (op, startMs) pairs.
	type spanRef struct {
		op string
		ts int64
	}
	byTrace := make(map[string][]spanRef)
	for _, r := range rows {
		ts := parseInt(r.StartTime)
		byTrace[r.TraceID] = append(byTrace[r.TraceID], spanRef{op: r.OpName, ts: ts})
	}

	// Sort each trace's spans by start time so the ordered match is meaningful.
	for tid := range byTrace {
		sort.Slice(byTrace[tid], func(i, j int) bool {
			return byTrace[tid][i].ts < byTrace[tid][j].ts
		})
	}

	// stepHits[i] counts the number of distinct traces that reached step i.
	stepHits := make([]int64, len(ops))
	// gapsMs[i] collects the gap (ms) between step i's match and step i-1's
	// match across all traces that reached step i. gapsMs[0] stays empty.
	gapsMs := make([][]int64, len(ops))

	for _, spans := range byTrace {
		// Walk ops in order, advancing through the trace's span list.
		var lastTs int64
		stepIdx := 0
		matched := false
		for _, s := range spans {
			if stepIdx >= len(ops) {
				break
			}
			if s.op != ops[stepIdx] {
				continue
			}
			// First op matches the first occurrence; subsequent ops must
			// occur strictly after the previous matched op.
			if stepIdx > 0 && s.ts < lastTs {
				continue
			}
			stepHits[stepIdx]++
			if stepIdx > 0 {
				gapsMs[stepIdx] = append(gapsMs[stepIdx], s.ts-lastTs)
			}
			lastTs = s.ts
			stepIdx++
			matched = true
		}
		_ = matched
	}

	steps := make([]FunnelStep, len(ops))
	for i, op := range ops {
		var conv float64
		if i == 0 {
			if stepHits[0] > 0 {
				conv = 100.0
			}
		} else if stepHits[i-1] > 0 {
			conv = float64(stepHits[i]) / float64(stepHits[i-1]) * 100.0
		}
		median, p95 := percentilePair(gapsMs[i])
		steps[i] = FunnelStep{
			Operation:     op,
			Count:         stepHits[i],
			ConversionPct: conv,
			MedianGapMs:   median,
			P95GapMs:      p95,
		}
	}

	return FunnelResult{Steps: steps, TotalTraces: int64(len(byTrace))}
}

// percentilePair returns the median (p50) and p95 of vals in milliseconds.
// Both are zero for an empty input. Sorts in place on a copy so callers'
// slices are untouched.
func percentilePair(vals []int64) (int64, int64) {
	if len(vals) == 0 {
		return 0, 0
	}
	sorted := append([]int64(nil), vals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return percentileAt(sorted, 0.50), percentileAt(sorted, 0.95)
}

// percentileAt picks the value at the given percentile from a pre-sorted
// slice using the same nearest-rank rule as tracing.percentile (idx =
// (n-1) * p). Mirrors the existing percentile() in ingest.go to stay
// consistent across the package.
func percentileAt(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// FunnelDef is a saved funnel persisted via the views table. The JSON shape
// is what we put in saved_views.view_config so we can list / load funnels
// without a dedicated table.
type FunnelDef struct {
	Type string   `json:"type"`
	Ops  []string `json:"ops"`
}

// SinceForTimestamps is a guard for callers that want a stable default
// time window when a request omits from/to. Returns last 24 hours in ms.
func SinceForTimestamps() (int64, int64) {
	now := time.Now().UTC()
	return now.Add(-24 * time.Hour).UnixMilli(), now.UnixMilli()
}
