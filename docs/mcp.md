# MCP server

Observe ships a Model Context Protocol server, so an AI client — Claude Code,
Cursor, anything that speaks MCP — can discover and call Observe's reads as
typed tools instead of being hand-told every REST route and payload shape.

The gap it closes is discovery and typing, not access. Nothing here grants an
agent anything a person with a dashboard login could not already do, and the
data boundary below is *narrower* than that login's.

## Pointing a client at it

The endpoint is `POST /api/mcp` on the dashboard address (the same host and port
as the UI, not the ingest listener). It is JSON-RPC 2.0 over streamable HTTP,
request/response only — no server push, no sessions. Every request carries a
bearer token.

Mint a token in **Settings → MCP**. Choose a name you will recognise in the
audit log; the secret is shown once and is not recoverable.

```jsonc
// .mcp.json / claude_desktop_config.json
{
  "mcpServers": {
    "observe": {
      "type": "http",
      "url": "https://observe.example.com/api/mcp",
      "headers": { "Authorization": "Bearer tpo_..." }
    }
  }
}
```

Or from the command line:

```bash
claude mcp add --transport http observe https://observe.example.com/api/mcp \
  --header "Authorization: Bearer tpo_..."
```

A quick check that it is reachable:

```bash
curl -s -X POST https://observe.example.com/api/mcp \
  -H "Authorization: Bearer tpo_..." \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

The dashboard address is usually tailnet-only, which is the intended shape: an
MCP token is a long-lived credential and it should not be reachable from the
public internet if the dashboard is not.

## Tools

All seven v1 tools are read-only.

| Tool | What it does |
|---|---|
| `observe_tables` | The complete readable surface — tables, columns, and how to read them. Call this first. |
| `observe_query` | Run a read-only SQL query against the analytics aggregates. |
| `observe_explain` | Return the plan for a statement without running it. |
| `observe_ask` | Turn a plain-language question into SQL using Observe's own query assistant. Returns the SQL; run it with `observe_query`. |
| `observe_list_incidents` | Open incidents for a site, or every incident overlapping a time range. |
| `observe_live_stats` | How many visitors are active right now. A count — there is no tool that returns who they are. |
| `observe_list_flags` | A site's feature flags: key, type, enabled state, rollout percentage. |

`observe_ask` needs the AI query assistant configured (**Settings → AI**); the
other six work out of the box.

Every tool wraps a service method the dashboard already calls. None of them
holds state of its own or reaches around the service layer, so an answer an
agent gets and an answer the dashboard shows are the same answer.

## The data boundary

Observe holds personal data. Teploy Dash's MCP server exposes deployment
metadata, where the worst case is an env var *name*; Observe's worst case is an
identified user's behaviour handed to a model and out to whatever it is plugged
into. A raw query tool over an analytics warehouse is an exfiltration primitive,
so **MCP reaches analytics aggregates only.**

That is enforced server-side, in one function, by an **allowlist** — not a
denylist, not a prompt, not a filter applied to results. The direction is the
point: when a migration adds a table, an allowlist refuses it until somebody
decides otherwise, and a denylist serves it until somebody remembers.

The rules, in `internal/mcp/allowlist.go`:

1. Every table named after `FROM` or `JOIN` — at any nesting depth, in any
   subquery, CTE or set operation — must be on the allowlist.
2. Every column must be on the allowlist for one of the tables the query reads.
   A column left off is unreachable even though its table is allowed: that is
   how `sites.session_salt`, `cron_monitors.ping_token` and
   `feature_flags.targeting` stay out.
3. A bare `*` is refused. `count(*)` is fine; `SELECT *` is not, because a
   wildcard would defeat rule 2 the moment a migration adds a column.
4. A schema-qualified table reference is refused outright, which is what keeps
   `information_schema` and `pg_catalog` unreachable.

**`observe_ask` goes through exactly the same gate.** The model is shown the
allowlist as its schema rather than the real one, and then the SQL it produces
is checked by the same function a hand-written query is checked by. A prompt is
not a control, so the generated statement is never trusted; a question that can
only be answered from personal data comes back refused, not answered.

### Readable

Traffic rollups (`stats_hourly`, `stats_daily`), service RED metrics
(`service_stats`, `service_dependencies`), error and performance issue groups
(`issues`, `performance_issues`), infrastructure and OTLP metrics
(`host_metrics`, `metric_points`), incidents, and the configuration tables —
`alert_rules`, `uptime_monitors`, `uptime_results`, `cron_monitors`,
`feature_flags`, `experiments`, `goals`, `sites`.

`observe_tables` prints the current list with columns; that output, not this
paragraph, is authoritative.

### Not readable, through any tool or any token

Raw per-visitor rows (`events`, `events_recent`, `sessions`), error and log and
span payloads (`error_events`, `logs`, `spans`), LLM prompts and completions
(`llm_traces`), session replays and heatmaps (`replay_sessions`,
`replay_events`, `click_heatmaps` — out of scope entirely: there is no analytics
question worth a session recording crossing this boundary), cohort and group
membership, per-user flag evaluations and experiment exposures, survey responses
and feedback, and every credential or account table.

### Read-only is not the same axis as PII

A token's role — viewer or editor — governs **mutation**. It does not govern
sensitivity. v1 ships **no PII-enabled token of any kind**, so there is no role,
flag or header that widens the allowlist. The two axes are enforced separately
and neither substitutes for the other.

### If a consumer genuinely needs person-level data

It gets an explicitly granted **separate scope** with its own audit trail — a
new token capability, a new tool, its own action namespace in the audit log.
Never by widening the default allowlist, and never by relaxing the rules above
for everyone in order to serve one caller. That is the reversal condition; it is
recorded here and in `internal/mcp/allowlist.go` so a future change has to argue
with it rather than around it.

## Roles

MCP tokens carry the canonical Teploy roles (see the RBAC contract):

- **viewer** — read tools only. The default.
- **editor** — may also call mutating tools. None ship in v1; the role exists so
  the second pass (create/close incident, evaluate flag) lands on a gate that is
  already enforced and already tested.

`admin` is deliberately not mintable: MCP has no configuration or credential
surface, and a token that cannot be granted admin cannot be tricked into using
it. Minting and revoking tokens is itself admin work and happens in the
dashboard, never over MCP.

A read-only token does not merely have mutating tools hidden from
`tools/list` — calling one by name is refused before the backend is reached.

## Audit

**Every MCP call is recorded** in Observe's append-only, tamper-evident audit
trail (**Settings → Audit**, or `GET /api/v1/audit`), alongside every other
admin action. One event per call, carrying:

- `actor` — the token id; `actor_type` — `agent`
- `action` — `mcp.<tool_name>` (`mcp.auth` for a rejected credential)
- `target` — the token's name
- `result` — `success`, `failure` or `denied`
- `source_ip`, `user_agent`
- `metadata` — the tool, the token id/name/role, the arguments as given
  (truncated at 2 KB), and the reason for any refusal

Denials are recorded, not just successes: a refused query, a read-only token
reaching for a mutating tool, and an invalid or revoked bearer token all leave a
record. Filter the audit log by `actor` to get one token's whole history.

Revoking a token keeps its row, so the trail can still name what acted.

## Not in scope

- Streaming (`/api/v1/logs/stream`, the live SSE feed) — MCP tools are
  request/response.
- Any change to an existing `/api/v1` route.
- Session replays and heatmaps, permanently.
