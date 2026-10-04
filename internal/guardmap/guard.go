package guardmap

import (
	"context"
	"errors"
	"time"

	"github.com/useteploy/teploy-observe/internal/queryguard"
)

// Guard is the O12 admission state for one read service (logs, metrics,
// and any later read path): an optional concurrency limiter plus the
// mandatory budget set. It carries the same pattern internal/query/guard.go
// implements for the stats service, packaged so each service does not
// re-derive it. The nil *Guard is valid and means "no concurrency
// admission, default budgets", so zero-value services built by tests keep
// the budgets in force.
type Guard struct {
	limiter *queryguard.Limiter
	budgets queryguard.Budgets
}

// NewGuard returns a Guard. A nil limiter disables concurrency admission;
// zero-valued budget fields fall back to the declared defaults so a
// hand-built Budgets{} cannot disable the row/time ceilings.
func NewGuard(l *queryguard.Limiter, b queryguard.Budgets) *Guard {
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
	return &Guard{limiter: l, budgets: b}
}

func (g *Guard) get() (*queryguard.Limiter, queryguard.Budgets) {
	if g == nil {
		return nil, queryguard.DefaultBudgets()
	}
	return g.limiter, g.budgets
}

// Begin is the admission gate for one expensive query: it takes a
// concurrency slot (labeled refusal when the global or site bound is full)
// and derives the context whose deadline is the time budget. The returned
// release must be called exactly once, on every path.
func (g *Guard) Begin(ctx context.Context, siteID string) (context.Context, func(), error) {
	l, b := g.get()
	release := func() {}
	if l != nil {
		rel, err := l.Acquire(ctx, siteID)
		if err != nil {
			return ctx, release, err
		}
		release = rel
	}
	qctx, cancel := context.WithTimeout(ctx, b.Timeout)
	return qctx, func() {
		cancel()
		release()
	}, nil
}

// MaxRows is the declared row ceiling.
func (g *Guard) MaxRows() int64 {
	_, b := g.get()
	return b.MaxScanRows
}

// RowRefusal builds (and counts) the labeled row-budget refusal.
func (g *Guard) RowRefusal() *queryguard.Refusal {
	l, b := g.get()
	return queryguard.RowBudgetRefusal(l, b.MaxScanRows)
}

// DeadlineError classifies a query failure: a deadline hit on the budget
// context while the caller's context is still live is a labeled
// time-budget refusal; anything else (caller cancellation, engine error)
// propagates as-is. parent is the caller's context (not the one Begin
// returned).
func (g *Guard) DeadlineError(parent context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) && parent.Err() == nil {
		l, b := g.get()
		return queryguard.TimeBudgetRefusal(l, b.Timeout)
	}
	return err
}

// Timeout is the declared wall-time budget (for tests and diagnostics).
func (g *Guard) Timeout() time.Duration {
	_, b := g.get()
	return b.Timeout
}
