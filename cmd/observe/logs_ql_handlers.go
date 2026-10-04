package main

import (
	"context"
	"errors"
	"time"

	"github.com/neutron-build/neutron/go/neutron"

	"github.com/useteploy/teploy-observe/internal/guardmap"
	"github.com/useteploy/teploy-observe/internal/logs"
)

// logSearchQL serves GET /api/v1/logs/search when the lq parameter is
// present. The plain search (no lq) never reaches this function.
//
// Syntax and limit errors are 400s carrying the 0-based character position
// of the problem; they are produced by the parser before any query is built
// or any database call is made.
func logSearchQL(ctx context.Context, svc *logs.LogService, input logSearchInput, from, to time.Time) (any, error) {
	ast, _, err := logs.ParseQueryParam(input.LQ)
	if err != nil {
		return nil, logQueryHTTPError(err)
	}
	if input.Offset != 0 {
		return nil, neutron.ErrBadRequest("offset is not supported with lq; page with the cursor from the previous response")
	}
	res, err := svc.SearchLogsQL(ctx, input.SiteID, from, to, input.Level, input.Service, ast, input.Limit, input.Cursor)
	if err != nil {
		if errors.Is(err, logs.ErrBadCursor) {
			return nil, neutron.ErrBadRequest("invalid cursor")
		}
		return nil, guardmap.HTTPError(err)
	}
	return res, nil
}

// logQueryHTTPError maps a parse error to a 400 problem response with the
// error position in the extensions.
func logQueryHTTPError(err error) error {
	var qe *logs.QueryError
	if !errors.As(err, &qe) {
		return neutron.ErrBadRequest("invalid query")
	}
	e := neutron.ErrBadRequest(qe.Error())
	e.Meta = map[string]any{"position": qe.Pos, "message": qe.Msg}
	return e
}
