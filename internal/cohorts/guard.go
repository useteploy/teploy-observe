package cohorts

import (
	"context"
	"github.com/useteploy/teploy-observe/internal/queryguard"
	"strconv"
	"time"
)

type admissionKey struct{}
type workKey struct{}
type readWork struct{ rows int64 }

// WithAdmission marks a context whose caller already holds the shared query slot.
func WithAdmission(ctx context.Context) context.Context {
	return context.WithValue(ctx, admissionKey{}, true)
}
func (s *Service) WithQueryGuard(l *queryguard.Limiter, b queryguard.Budgets) *Service {
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
func (s *Service) beginRead(ctx context.Context, siteID string) (context.Context, func(), error) {
	if ctx.Value(workKey{}) == nil {
		ctx = context.WithValue(ctx, workKey{}, &readWork{})
	}
	if ctx.Value(admissionKey{}) == true {
		return ctx, func() {}, nil
	}
	release := func() {}
	if s.guard != nil {
		var err error
		release, err = s.guard.Acquire(ctx, siteID)
		if err != nil {
			return ctx, func() {}, err
		}
	}
	timeout := s.budgets.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	qctx, cancel := context.WithTimeout(WithAdmission(ctx), timeout)
	return qctx, func() { cancel(); release() }, nil
}

func (s *Service) leafLimitSQL() string {
	limit := int64(MaxLeafRows)
	if s.budgets.MaxScanRows > 0 && s.budgets.MaxScanRows < limit {
		limit = s.budgets.MaxScanRows
	}
	return " LIMIT " + strconv.FormatInt(limit+1, 10)
}
func (s *Service) accountRows(ctx context.Context, n int) error {
	total := int64(n)
	if w, ok := ctx.Value(workKey{}).(*readWork); ok {
		w.rows += int64(n)
		total = w.rows
	}
	if s.budgets.MaxScanRows > 0 && total > s.budgets.MaxScanRows {
		return queryguard.RowBudgetRefusal(s.guard, s.budgets.MaxScanRows)
	}
	return ctx.Err()
}
