package main

// Trace funnel API — separated from main.go to keep merge surface minimal.
// The single integration point in main.go is RegisterFunnelRoutes inside
// the route-registration block. Saved funnels are stored in the existing
// saved_views table with view_config JSON of shape {"type":"trace_funnel",
// "ops":["GET /a","POST /b",...]} so we don't need a new schema.

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/neutron-dev/neutron-go/neutron"

	"github.com/useteploy/teploy-observe/internal/tracing"
	"github.com/useteploy/teploy-observe/internal/views"
)

// funnelViewType is the discriminator stored in view_config.type so funnel
// definitions can coexist with other future view types in saved_views.
const funnelViewType = "trace_funnel"

// RegisterFunnelRoutes wires the trace funnel HTTP endpoints onto the
// given router. The router is expected to already have JWT middleware
// applied. requireEditor must be passed in so the writes (save / delete)
// are gated to admin/editor while preview + list stay readable for any
// authenticated user.
func RegisterFunnelRoutes(
	r *neutron.Router,
	q *tracing.QueryService,
	viewSvc *views.ViewService,
	requireEditor neutron.Middleware,
) {
	editor := r.Group("", requireEditor)

	neutron.Post(editor, "/preview", funnelPreviewHandler(q),
		neutron.WithTags("tracing", "funnels"),
		neutron.WithSummary("Compute a trace funnel ad-hoc"),
	)
	neutron.Get(r, "/saved", listSavedFunnelsHandler(viewSvc),
		neutron.WithTags("tracing", "funnels"),
		neutron.WithSummary("List saved trace funnels"),
	)
	neutron.Post(editor, "/saved", saveFunnelHandler(viewSvc),
		neutron.WithTags("tracing", "funnels"),
		neutron.WithSummary("Save a trace funnel definition"),
	)
	neutron.Delete(editor, "/saved/{view_id}", deleteSavedFunnelHandler(viewSvc),
		neutron.WithTags("tracing", "funnels"),
		neutron.WithSummary("Delete a saved trace funnel"),
	)
}

// --- Preview ---

type funnelPreviewInput struct {
	SiteID string   `json:"site_id"`
	Ops    []string `json:"ops"`
	From   int64    `json:"from"`
	To     int64    `json:"to"`
}

func funnelPreviewHandler(q *tracing.QueryService) neutron.HandlerFunc[funnelPreviewInput, tracing.FunnelResult] {
	return func(ctx context.Context, input funnelPreviewInput) (tracing.FunnelResult, error) {
		if input.SiteID == "" {
			return tracing.FunnelResult{}, neutron.ErrBadRequest("site_id required")
		}
		ops := normalizeFunnelOps(input.Ops)
		if len(ops) < 2 {
			return tracing.FunnelResult{}, neutron.ErrBadRequest("at least 2 ops required")
		}
		fromMs, toMs := input.From, input.To
		if fromMs == 0 || toMs == 0 {
			fromMs, toMs = tracing.SinceForTimestamps()
		}
		if toMs <= fromMs {
			return tracing.FunnelResult{}, neutron.ErrBadRequest("to must be > from")
		}
		res, err := q.FunnelByOps(ctx, input.SiteID, ops, fromMs, toMs)
		if err != nil {
			return tracing.FunnelResult{}, err
		}
		// Ensure non-nil slice for JSON consumers (matches emptyOnNil
		// pattern used elsewhere in this package).
		if res.Steps == nil {
			res.Steps = []tracing.FunnelStep{}
		}
		return res, nil
	}
}

// --- Saved funnels ---

// SavedFunnel is the JSON shape returned from list/saved. Mirrors the
// underlying SavedView but parses view_config into ops so the UI can
// consume it directly.
type SavedFunnel struct {
	ViewID    string   `json:"view_id"`
	SiteID    string   `json:"site_id"`
	Name      string   `json:"name"`
	Ops       []string `json:"ops"`
	CreatedBy string   `json:"created_by"`
	CreatedAt string   `json:"created_at"`
}

type listSavedFunnelsInput struct {
	SiteID string `query:"site_id"`
}

func listSavedFunnelsHandler(svc *views.ViewService) neutron.HandlerFunc[listSavedFunnelsInput, []SavedFunnel] {
	return func(ctx context.Context, input listSavedFunnelsInput) ([]SavedFunnel, error) {
		if input.SiteID == "" {
			return nil, neutron.ErrBadRequest("site_id required")
		}
		all, err := svc.List(ctx, input.SiteID)
		if err != nil {
			return nil, err
		}
		out := make([]SavedFunnel, 0)
		for _, v := range all {
			// Skip views with empty config. views.Delete hard-deletes now, but
			// installs that ran the old INSERT-tombstone delete still carry
			// blank rows on disk.
			if v.Name == "" || v.ViewConfig == "" {
				continue
			}
			var def tracing.FunnelDef
			if err := json.Unmarshal([]byte(v.ViewConfig), &def); err != nil {
				continue
			}
			if def.Type != funnelViewType {
				continue
			}
			out = append(out, SavedFunnel{
				ViewID:    v.ViewID,
				SiteID:    v.SiteID,
				Name:      v.Name,
				Ops:       def.Ops,
				CreatedBy: v.CreatedBy,
				CreatedAt: v.CreatedAt,
			})
		}
		return out, nil
	}
}

type saveFunnelInput struct {
	SiteID string   `json:"site_id"`
	Name   string   `json:"name"`
	Ops    []string `json:"ops"`
}

func saveFunnelHandler(svc *views.ViewService) neutron.HandlerFunc[saveFunnelInput, SavedFunnel] {
	return func(ctx context.Context, input saveFunnelInput) (SavedFunnel, error) {
		if input.SiteID == "" || input.Name == "" {
			return SavedFunnel{}, neutron.ErrBadRequest("site_id and name required")
		}
		ops := normalizeFunnelOps(input.Ops)
		if len(ops) < 2 {
			return SavedFunnel{}, neutron.ErrBadRequest("at least 2 ops required")
		}
		def := tracing.FunnelDef{Type: funnelViewType, Ops: ops}
		cfg, err := json.Marshal(def)
		if err != nil {
			return SavedFunnel{}, err
		}
		v, err := svc.Create(ctx, input.SiteID, input.Name, string(cfg), "")
		if err != nil {
			return SavedFunnel{}, err
		}
		return SavedFunnel{
			ViewID:    v.ViewID,
			SiteID:    v.SiteID,
			Name:      v.Name,
			Ops:       ops,
			CreatedBy: v.CreatedBy,
			CreatedAt: v.CreatedAt,
		}, nil
	}
}

type deleteSavedFunnelInput struct {
	ViewID string `path:"view_id"`
}

func deleteSavedFunnelHandler(svc *views.ViewService) neutron.HandlerFunc[deleteSavedFunnelInput, neutron.Empty] {
	return func(ctx context.Context, input deleteSavedFunnelInput) (neutron.Empty, error) {
		if input.ViewID == "" {
			return neutron.Empty{}, neutron.ErrBadRequest("view_id required")
		}
		return neutron.Empty{}, svc.Delete(ctx, input.ViewID)
	}
}

// normalizeFunnelOps trims whitespace and drops empty entries so the UI
// can submit "+ Add step" rows without forcing the user to clean up.
func normalizeFunnelOps(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}
