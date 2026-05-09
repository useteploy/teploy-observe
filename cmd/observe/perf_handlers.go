package main

// Performance issue API — separated from main.go to minimise merge conflicts
// with W4.A (heatmaps) and W4.B (replay→issue linking) which are both
// currently editing main.go. The single integration point is
// RegisterPerformanceRoutes, called from main.go's route-registration block.

import (
	"context"
	"strconv"

	"github.com/neutron-dev/neutron-go/neutron"

	"github.com/useteploy/teploy-observe/internal/tracing"
)

// RegisterPerformanceRoutes wires the performance issue HTTP endpoints onto
// the given router. The router is expected to already have JWT middleware
// applied (typically a r.Group("/api/v1/performance", jwtMW) handed in).
func RegisterPerformanceRoutes(r *neutron.Router, svc *tracing.QueryService) {
	neutron.Get(r, "/issues", listPerformanceIssuesHandler(svc),
		neutron.WithTags("performance"),
		neutron.WithSummary("List detected performance issues"),
	)
}

type listPerfIssuesInput struct {
	SiteID string `query:"site_id"`
	From   string `query:"from"`
	To     string `query:"to"`
}

func listPerformanceIssuesHandler(svc *tracing.QueryService) neutron.HandlerFunc[listPerfIssuesInput, []tracing.PerformanceIssue] {
	return func(ctx context.Context, input listPerfIssuesInput) ([]tracing.PerformanceIssue, error) {
		if input.SiteID == "" {
			return nil, neutron.ErrBadRequest("site_id required")
		}
		from, to := parseTimeRange(input.From, input.To)
		fromMs := from.UnixMilli()
		toMs := to.UnixMilli()
		// Defensive: a caller passing 0/0 would otherwise return the entire
		// table. The default in parseTimeRange is last-24h, so this is
		// belt-and-braces for parse failures.
		if fromMs == 0 && toMs == 0 {
			fromMs, _ = strconv.ParseInt("0", 10, 64)
			toMs = 1<<62 - 1
		}
		out, err := svc.ListPerformanceIssues(ctx, input.SiteID, fromMs, toMs)
		if err != nil {
			return nil, err
		}
		if out == nil {
			out = []tracing.PerformanceIssue{}
		}
		return out, nil
	}
}
