package main

// Multi-touch UTM attribution HTTP routes (W2.C).
//
// Lives in its own file to minimise merge surface with the two parallel
// Wave 2 agents — funnels (W2.A) and boards (W2.B). Wired into main.go
// with one line under "// --- Attribution API ---".

import (
	"context"
	"time"

	"github.com/neutron-dev/neutron-go/neutron"

	"github.com/useteploy/teploy-observe/internal/query"
)

// attributionInput is the GET query for /api/v1/attribution.
//
//	site_id  required.
//	model    one of "first" | "last" | "linear" — 400 on anything else.
//	from/to  RFC3339; defaults to last 24h via parseTimeRange.
type attributionInput struct {
	SiteID string `query:"site_id"`
	Model  string `query:"model"`
	From   string `query:"from"`
	To     string `query:"to"`
}

// RegisterAttributionRoutes wires the attribution HTTP endpoints onto
// the given router. The router is expected to already have JWT
// middleware applied (caller passes r.Group("/api/v1", jwtMW) or
// equivalent).
func RegisterAttributionRoutes(r *neutron.Router, svc *query.AttributionService) {
	neutron.Get(r, "/api/v1/attribution", attributionHandler(svc),
		neutron.WithTags("attribution"),
		neutron.WithSummary("Multi-touch UTM attribution (first / last / linear)"),
	)
}

func attributionHandler(svc *query.AttributionService) neutron.HandlerFunc[attributionInput, []query.AttributionRow] {
	return func(ctx context.Context, input attributionInput) ([]query.AttributionRow, error) {
		if input.SiteID == "" {
			return nil, neutron.ErrBadRequest("site_id required")
		}
		// Default model to first-touch when omitted — matches Umami's
		// default and keeps the URL short for the common case.
		model := input.Model
		if model == "" {
			model = query.AttributionFirstTouch
		}
		if !query.IsValidModel(model) {
			return nil, neutron.ErrBadRequest("model must be one of: first, last, linear")
		}

		from, _ := time.Parse(time.RFC3339, input.From)
		to, _ := time.Parse(time.RFC3339, input.To)
		fromMs, toMs := query.TimeRangeMs(from, to)

		rows, err := svc.AttributionByModel(ctx, input.SiteID, model, fromMs, toMs)
		if err != nil {
			return nil, err
		}
		// Always serialize as [] not null so the TS client's
		// AttributionRow[] type holds. Mirrors emptyOnNil in main.go
		// but inlined to keep this file standalone (the helper isn't
		// exported).
		if rows == nil {
			return []query.AttributionRow{}, nil
		}
		return rows, nil
	}
}
