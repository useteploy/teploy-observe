package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/useteploy/teploy-observe/internal/aiquery"
	"github.com/useteploy/teploy-observe/internal/explorer"
	"github.com/useteploy/teploy-observe/internal/flags"
	"github.com/useteploy/teploy-observe/internal/incidents"
	"github.com/useteploy/teploy-observe/internal/mcp"
	"github.com/useteploy/teploy-observe/internal/query"
)

// mcpBackend adapts Observe's existing services to the MCP tool surface.
//
// Every method is a thin formatter over a service call the dashboard already
// makes. Nothing here queries the database directly, holds state, or
// reimplements a service — that is the invariant ported from Dash's MCP, and it
// is what keeps the answer an agent gets identical to the answer the dashboard
// shows.
type mcpBackend struct {
	explorer  *explorer.ExplorerService
	ai        *aiquery.Service
	incidents *incidents.Service
	stats     *query.StatsService
	flags     *flags.FlagService
}

func (b *mcpBackend) ListTables(ctx context.Context) ([]string, error) {
	return b.explorer.ListTables(ctx)
}

// Query and Explain receive SQL the allowlist has ALREADY passed — the gate
// runs in internal/mcp, before the backend is reached, so there is exactly one
// place the boundary is enforced and no way to call the service around it.
func (b *mcpBackend) Query(ctx context.Context, sql string) (string, error) {
	res, err := b.explorer.Execute(ctx, sql)
	if err != nil {
		return "", err
	}
	return encodeResult(res)
}

func (b *mcpBackend) Explain(ctx context.Context, sql string) (string, error) {
	res, err := b.explorer.Explain(ctx, sql)
	if err != nil {
		return "", err
	}
	return encodeResult(res)
}

// encodeResult surfaces the explorer's in-band error as a real error, so a
// rejected query reads as a failed tool call rather than a successful one whose
// payload happens to contain an error string.
func encodeResult(res *explorer.QueryResult) (string, error) {
	if res == nil {
		return "", fmt.Errorf("no result")
	}
	if res.Error != "" {
		return "", fmt.Errorf("%s", res.Error)
	}
	return encodeJSON(res)
}

func (b *mcpBackend) GenerateSQL(ctx context.Context, question, schemaCard string) (string, error) {
	res, err := b.ai.Generate(ctx, question, schemaCard)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(res.SQL) == "" {
		return "", fmt.Errorf("the query assistant returned nothing")
	}
	return res.SQL, nil
}

func (b *mcpBackend) ActiveIncidents(ctx context.Context, siteID string) (string, error) {
	list, err := b.incidents.Active(ctx, siteID)
	if err != nil {
		return "", err
	}
	return encodeJSON(list)
}

func (b *mcpBackend) IncidentsInRange(ctx context.Context, siteID string, from, to int64) (string, error) {
	list, err := b.incidents.InRange(ctx, siteID, from, to)
	if err != nil {
		return "", err
	}
	return encodeJSON(list)
}

func (b *mcpBackend) LiveStats(ctx context.Context, siteID string, minutes int) (string, error) {
	count, err := b.stats.RealtimeVisitors(ctx, siteID, minutes)
	if err != nil {
		return "", err
	}
	return encodeJSON(map[string]any{
		"site_id":         siteID,
		"window_minutes":  minutes,
		"active_visitors": count,
	})
}

// mcpFlag is the flag shape MCP returns. It is the service DTO minus
// `targeting`, whose rules can name individual users — the same column the SQL
// allowlist withholds. The two paths agree on purpose: a boundary that holds
// for `observe_query` and leaks through `observe_list_flags` is not a boundary.
type mcpFlag struct {
	FlagKey     string `json:"flag_key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	FlagType    string `json:"flag_type"`
	Enabled     bool   `json:"enabled"`
	RolloutPct  int    `json:"rollout_pct"`
	Variants    string `json:"variants"`
}

func (b *mcpBackend) ListFlags(ctx context.Context, siteID string) (string, error) {
	list, err := b.flags.List(ctx, siteID)
	if err != nil {
		return "", err
	}
	out := make([]mcpFlag, 0, len(list))
	for _, f := range list {
		out = append(out, mcpFlag{
			FlagKey:     f.FlagKey,
			Name:        f.Name,
			Description: f.Description,
			FlagType:    f.FlagType,
			Enabled:     f.Enabled,
			RolloutPct:  f.RolloutPct,
			Variants:    f.Variants,
		})
	}
	return encodeJSON(out)
}

func encodeJSON(v any) (string, error) {
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(buf), nil
}

// --- Token management API (admin only) ---
//
// Minting and revoking a credential is admin work per the RBAC contract
// ("account/credential/config -> admin"). These routes live under /api/v1 so
// the audit middleware records the mutations alongside every other admin
// action; the MCP endpoint itself lives at /api/mcp and audits per call.

func mcpTokensListHandler(store *mcp.TokenStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		list, err := store.List(r.Context())
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "listing MCP tokens failed"})
			return
		}
		if list == nil {
			list = []mcp.Token{}
		}
		json.NewEncoder(w).Encode(list)
	}
}

func mcpTokensCreateHandler(store *mcp.TokenStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Name string `json:"name"`
			Role string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		plaintext, tok, err := store.Create(r.Context(), input.Name, input.Role)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// The plaintext crosses the wire exactly once, here. Nothing stores it
		// and no read path returns it.
		json.NewEncoder(w).Encode(map[string]any{"token": plaintext, "record": tok})
	}
}

func mcpTokensRevokeHandler(store *mcp.TokenStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("token_id")
		if id == "" {
			http.Error(w, `{"error":"token_id required"}`, http.StatusBadRequest)
			return
		}
		if err := store.Revoke(r.Context(), id); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}
}
