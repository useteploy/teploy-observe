# Trace search: attribute filters and orphan traces

`GET /api/v1/traces/search-advanced` (JWT) extends `GET /api/v1/traces/search`,
which is unchanged (root spans only, array response, ignores the parameters
below). Everything here is **UNVERIFIED against a live Nucleus**: it is covered
by unit tests with stubbed queries only.

## Parameters

| Param | Meaning |
|---|---|
| `site_id`, `from`, `to`, `service`, `operation`, `status`, `min_duration`, `max_duration`, `limit`, `offset` | As `/search`. `limit` is capped at 200. |
| `attr=key:op[:value]` | Repeatable, at most 5 in total. `op` is `eq`, `neq`, `exists` or `contains`. Keys match `^[A-Za-z0-9_.\-/]{1,128}$`; values are at most 256 bytes. |
| `http_status_code=500` | Shortcut for `attr=http.status_code:eq:500` (counts toward the 5). |
| `http_method=GET` | Shortcut for `attr=http.method:eq:GET` (counts toward the 5). |
| `include_orphans=true` | Also list traces with no root span (default off). |

`neq` means "present with a different value" (an absent key does not match).
`contains` is case-sensitive. Filters apply to the listed span: the root span,
or for an orphan trace its earliest span. A bad key, op, value, flag or more
than five filters is a 400.

The response is an envelope, not an array:

```json
{"traces": [...], "truncated": false, "truncated_reason": "", "candidate_cap": 2000}
```

`root_missing: true` on a trace means it has no root span and the row is its
earliest span (span count is the spans found; duration is the trace extent).

## How attribute filtering works

`spans.attributes` is a JSONB column (migration 003) holding a string map. The
repo has no verified JSON extraction over it (`internal/metrics/attrs.go`
records that Nucleus has no JSONB extract; `internal/query/goals.go` uses `->>`
only on `events.properties`), so no key is ever interpolated into SQL. Search
is two-stage:

1. SQL narrows by the existing predicates plus, per filter,
   `CAST(attributes AS TEXT) LIKE $n` with a bound pattern `%"key"%` and, for
   `eq`/`contains`, `%value%`. Patterns are built to only ever over-select:
   `_` stays a one-character wildcard (a superset of itself), and a value
   containing `%`, `\`, a quote, a control byte or non-ASCII skips the value
   pattern entirely, so the result never depends on how the engine escapes or
   re-serialises JSON. Values and keys are always bound parameters.
2. Go decodes the attributes of up to 2000 candidate root spans and checks
   every filter exactly.

If the candidate set hits 2000 the response says `truncated: true` with a
reason; narrow the time range or filters. It is never silent.

## Orphan mode

With `include_orphans=true` the newest 2000 non-root spans in the window are
inspected (at most 200 distinct traces, 5000 spans loaded). A trace is
reported when it has no span with an empty parent and its earliest span's
parent is not in the trace. This is best effort on a busy site and will
usually be `truncated`; the cost of an exact answer would be a window-wide
anti-join that this repo has no precedent for.

## Query admission (O12)

Service list, operation list, trace search, trace fetch and the dependency
graph now take a slot from the shared limiter and run under the time budget
(see `capacity.md`). A full limiter is a labeled 429 and a blown budget a 504,
with the refusal code in the problem body. The per-page span count is now
restricted to the page's trace ids instead of grouping the whole window.
