package query

import (
	"context"
	"errors"
	"github.com/neutron-build/neutron/go/neutron"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/cohorts"
	"github.com/useteploy/teploy-observe/internal/queryguard"
)

// O12 query admission for the expensive analytics read paths. The
// StatsService carries an optional concurrency limiter and a mandatory
// budget set; the heavy methods (funnel, funnel breakdown, retention,
// journeys, correlation) route through beginQuery and the bounded scans
// below instead of issuing unbounded range reads.

// WithQueryGuard installs the O12 admission state. A nil limiter disables
// concurrency admission (tests, embedded use); budgets are always in
// force. Zero-valued fields in b fall back to the declared defaults so a
// hand-built Budgets{} cannot disable the row/time ceilings.
func (s *StatsService) WithQueryGuard(l *queryguard.Limiter, b queryguard.Budgets) *StatsService {
	d := queryguard.DefaultBudgets()
	if b.Timeout <= 0 {
		b.Timeout = d.Timeout
	}
	if b.MaxScanRows <= 0 {
		b.MaxScanRows = d.MaxScanRows
	}
	if b.MaxWindow <= 0 {
		b.MaxWindow = d.MaxWindow
	}
	s.guard = l
	s.budgets = b
	return s
}

// beginQuery is the admission gate for one expensive query: it takes a
// concurrency slot (labeled refusal when the global or site bound is
// full) and derives the budget-bounded context whose deadline is the
// time budget. The returned release must be called exactly once on the
// success path.
func (s *StatsService) beginQuery(ctx context.Context, siteID string) (context.Context, func(), error) {
	var release func() = func() {}
	if s.guard != nil {
		rel, err := s.guard.Acquire(ctx, siteID)
		if err != nil {
			return ctx, release, err
		}
		release = rel
	}
	ctx = context.WithValue(ctx, scanWorkKey{}, &scanWork{})
	qctx, cancel := context.WithTimeout(cohorts.WithAdmission(ctx), s.budgets.Timeout)
	return qctx, func() {
		cancel()
		release()
	}, nil
}

// scanDeadlineError classifies a scan failure: a deadline hit on the
// budget context while the caller's context is still live is a labeled
// time-budget refusal; anything else (caller cancellation, engine error)
// propagates as-is.
func (s *StatsService) scanDeadlineError(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return queryguard.TimeBudgetRefusal(s.guard, s.budgets.Timeout)
	}
	return err
}

// streamEvents is the O12 bounded replacement for the unbounded
// "SELECT ... FROM events WHERE site_id AND timestamp range" reads the
// funnel, funnel-breakdown and retention paths used to run: it streams
// rows in the walk's pinned (timestamp, event_id) total order through
// pgx (O(1) rows held at a time), refuses labeled once the declared row
// budget is exceeded, and is bounded in wall time by the budget context.
//
// extraSelect, when non-empty, is appended to the column list (the
// funnel-breakdown path adds its breakdown expression) and the per-row
// callback receives the raw pgx row to scan it.
func (s *StatsService) streamEvents(ctx context.Context, siteID string, fromMs, toMs int64, extraSelect string, visit func(row pgx.Row) error) error {
	cols := funnelEntityColumns
	if extraSelect != "" {
		cols += ", " + extraSelect
	}
	// ORDER BY resolves against the select list's output names — every
	// term below is a selected output name (timestamp, event_id), and a
	// nucleus-gated test (o12_budget_nucleus_test.go) pins that the
	// engine actually delivers this total order; a silently-ignored
	// ORDER BY term would corrupt the walk, not just its performance.
	q := `SELECT ` + cols + `
		 FROM events
		 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
		 ORDER BY timestamp ASC, event_id ASC`
	rows, err := s.db.Pool().Query(ctx, q, pgx.QueryExecModeSimpleProtocol, siteID, fromMs, toMs)
	if err != nil {
		return s.scanDeadlineError(ctx, err)
	}
	defer rows.Close()

	for rows.Next() {
		if err := s.accountScan(ctx, 1); err != nil {
			// Return before visiting: the refusal replaces the result,
			// never truncates it into a misleading partial answer.
			return err
		}
		if err := visit(rows); err != nil {
			return err
		}
	}
	return s.scanDeadlineError(ctx, rows.Err())
}

// scanFunnelEvent scans one streamed row into a funnelEvent. Kept next to
// streamEvents so a column-list change cannot drift from the scan order.
// The COALESCE terms in funnelEntityColumns keep every target non-NULL.
func scanFunnelEvent(row pgx.Row) (funnelEvent, error) {
	var e funnelEvent
	err := row.Scan(&e.EventID, &e.SessionID, &e.VisitID, &e.DistinctID,
		&e.EventType, &e.Pathname, &e.Timestamp)
	return e, err
}

// scanFunnelEventWithBreakdown scans a streamed row that carries one
// extra trailing text column (the funnel-breakdown expression) after the
// funnelEntityColumns prefix.
func scanFunnelEventWithBreakdown(row pgx.Row) (struct {
	funnelEvent
	Breakdown string
}, error) {
	var e struct {
		funnelEvent
		Breakdown string
	}
	err := row.Scan(&e.EventID, &e.SessionID, &e.VisitID, &e.DistinctID,
		&e.EventType, &e.Pathname, &e.Timestamp, &e.Breakdown)
	return e, err
}

// boundedRangeQuery runs a whole-result read (journeys, correlation) with
// the O12 budgets without restructuring its aggregation: the SQL gains a
// LIMIT of budget+1 rows so the engine materializes at most one row past
// the ceiling, and reading past the budget converts to a labeled refusal
// instead of an unbounded slice. The LIMIT never trips for in-budget
// queries, so result sets are unchanged below the ceiling (the
// no-total-order caveat does not apply to a bound we only ever refuse
// past).
func boundedRangeQuery[T any](ctx context.Context, s *StatsService, query string, args ...any) ([]T, error) {
	bounded := query + " LIMIT " + itoa(scanLimit(s.budgets.MaxScanRows))
	rows, err := nucleus.Query[T](ctx, s.db.SQL(), bounded, args...)
	if err != nil {
		return nil, s.scanDeadlineError(ctx, err)
	}
	if err := s.accountScan(ctx, int64(len(rows))); err != nil {
		return nil, err
	}
	return rows, nil
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// A single admitted operation shares its materialized/streamed row allowance
// across all reads (notably retention sessions plus raw activity).
type scanWorkKey struct{}
type scanWork struct{ rows int64 }

func scanLimit(max int64) int64 {
	if max == math.MaxInt64 {
		return max
	}
	return max + 1
}
func (s *StatsService) accountScan(ctx context.Context, n int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w, _ := ctx.Value(scanWorkKey{}).(*scanWork)
	used := int64(0)
	if w != nil {
		used = w.rows
	}
	if n > s.budgets.MaxScanRows-used {
		return queryguard.RowBudgetRefusal(s.guard, s.budgets.MaxScanRows)
	}
	if w != nil {
		w.rows += n
	}
	return nil
}
func (s *StatsService) validateWindow(from, to time.Time) error {
	if !from.Before(to) {
		return neutron.ErrBadRequest("query requires from < to")
	}
	if to.Sub(from) > s.budgets.MaxWindow {
		return neutron.ErrBadRequest("query window exceeds supported maximum")
	}
	return nil
}

// Stable bottom-up merge sorting with cooperative cancellation in the work
// loop; checks around an uncancellable standard sort alone are insufficient.
func sortWithContext[T any](ctx context.Context, values []T, less func(T, T) bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	n := len(values)
	if n < 2 {
		return nil
	}
	scratch := make([]T, n)
	for width := 1; width < n; {
		for start := 0; start < n; start += 2 * width {
			mid := start + width
			if mid > n {
				mid = n
			}
			end := start + 2*width
			if end > n {
				end = n
			}
			i, j := start, mid
			for k := start; k < end; k++ {
				if k%64 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				if i < mid && (j >= end || !less(values[j], values[i])) {
					scratch[k] = values[i]
					i++
				} else {
					scratch[k] = values[j]
					j++
				}
			}
		}
		for start := 0; start < n; start += 64 {
			if err := ctx.Err(); err != nil {
				return err
			}
			end := start + 64
			if end > n {
				end = n
			}
			copy(values[start:end], scratch[start:end])
		}
		if width > n/2 {
			break
		}
		width *= 2
	}
	return ctx.Err()
}
