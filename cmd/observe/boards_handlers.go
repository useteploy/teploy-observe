package main

// Boards API (W2.B) — multi-site aggregate dashboards. Routes live here
// so the integration into main.go is a single RegisterBoardsRoutes call,
// keeping the merge-conflict surface with W2.A (funnels) and W2.C
// (attribution) at zero.

import (
	"context"
	"strings"

	"github.com/neutron-dev/neutron-go/neutron"

	"github.com/useteploy/teploy-observe/internal/query"
)

// RegisterBoardsRoutes wires the boards endpoints onto the given router.
// The caller supplies jwtMW and requireEditor as neutron.Middleware so
// the auth policy is owned by main.go (single source of truth) and this
// file only knows the route shapes.
func RegisterBoardsRoutes(
	r *neutron.Router,
	jwtMW neutron.Middleware,
	requireEditor neutron.Middleware,
	svc *query.BoardService,
) {
	read := r.Group("/api/v1/boards", jwtMW)
	write := read.Group("", requireEditor)

	neutron.Get(read, "/summary", boardsSummaryHandler(svc),
		neutron.WithTags("boards"),
		neutron.WithSummary("Aggregate stats across multiple sites"),
	)
	neutron.Get(read, "/saved", listSavedBoardsHandler(svc),
		neutron.WithTags("boards"),
		neutron.WithSummary("List saved boards"),
	)
	neutron.Post(write, "/saved", createBoardHandler(svc),
		neutron.WithTags("boards"),
		neutron.WithSummary("Save a board definition"),
	)
	neutron.Delete(write, "/saved/{board_id}", deleteBoardHandler(svc),
		neutron.WithTags("boards"),
		neutron.WithSummary("Delete a saved board"),
	)
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

type boardsSummaryInput struct {
	SiteIDs string `query:"site_ids"`
	From    string `query:"from"`
	To      string `query:"to"`
}

func boardsSummaryHandler(svc *query.BoardService) neutron.HandlerFunc[boardsSummaryInput, []query.SiteRow] {
	return func(ctx context.Context, in boardsSummaryInput) ([]query.SiteRow, error) {
		if in.SiteIDs == "" {
			return nil, neutron.ErrBadRequest("site_ids required (comma-separated)")
		}
		ids := splitCSV(in.SiteIDs)
		if len(ids) == 0 {
			return []query.SiteRow{}, nil
		}
		from, to := parseTimeRange(in.From, in.To)
		rows, err := svc.BoardSummary(ctx, ids, from.UnixMilli(), to.UnixMilli())
		if err != nil {
			return nil, err
		}
		if rows == nil {
			rows = []query.SiteRow{}
		}
		return rows, nil
	}
}

func listSavedBoardsHandler(svc *query.BoardService) neutron.HandlerFunc[neutron.Empty, []query.SavedBoard] {
	return func(ctx context.Context, _ neutron.Empty) ([]query.SavedBoard, error) {
		return emptyOnNil(svc.ListBoards(ctx))
	}
}

type createBoardInput struct {
	Name    string             `json:"name"`
	Payload query.BoardPayload `json:"payload"`
}

func createBoardHandler(svc *query.BoardService) neutron.HandlerFunc[createBoardInput, query.SavedBoard] {
	return func(ctx context.Context, in createBoardInput) (query.SavedBoard, error) {
		if strings.TrimSpace(in.Name) == "" {
			return query.SavedBoard{}, neutron.ErrBadRequest("name required")
		}
		if len(in.Payload.SiteIDs) == 0 {
			return query.SavedBoard{}, neutron.ErrBadRequest("payload.site_ids required (at least one site)")
		}
		// Default window if the UI omits it. Keeps the saved entity
		// self-describing without forcing the UI to always set it.
		if in.Payload.Window == "" {
			in.Payload.Window = "24h"
		}
		// Bound the number of sites per board. 200 is well above our
		// 50-site agency target and keeps the fan-out sane.
		if len(in.Payload.SiteIDs) > 200 {
			return query.SavedBoard{}, neutron.ErrBadRequest("payload.site_ids capped at 200")
		}
		// createdBy is left blank for now — the auth middleware stashes
		// only the role in the request context, not the username. A
		// future token-claim refactor (the existing JWT already has
		// "username") can wire this through; today's contract is
		// "audited via JWT, not via DB column".
		b, err := svc.CreateBoard(ctx, in.Name, in.Payload, "")
		if err != nil {
			return query.SavedBoard{}, err
		}
		return *b, nil
	}
}

type deleteBoardInput struct {
	BoardID string `path:"board_id"`
}

func deleteBoardHandler(svc *query.BoardService) neutron.HandlerFunc[deleteBoardInput, neutron.Empty] {
	return func(ctx context.Context, in deleteBoardInput) (neutron.Empty, error) {
		if in.BoardID == "" {
			return neutron.Empty{}, neutron.ErrBadRequest("board_id required")
		}
		if err := svc.DeleteBoard(ctx, in.BoardID); err != nil {
			return neutron.Empty{}, err
		}
		return neutron.Empty{}, nil
	}
}

// splitCSV trims and drops empty entries so a trailing comma or stray
// whitespace doesn't fan out an empty site_id query.
func splitCSV(in string) []string {
	parts := strings.Split(in, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
