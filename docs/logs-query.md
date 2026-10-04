# Log query language

`GET /api/v1/logs/search` accepts an optional `lq` parameter holding a small
query expression. (`q` keeps its original meaning, a single case-insensitive
message substring, and without `lq` the endpoint behaves exactly as before.)

## Syntax

| Form | Meaning |
|------|---------|
| `timeout` | message contains the word (case-insensitive) |
| `"connection reset"` | message contains the phrase |
| `level:error`, `service:api`, `trace_id:abc`, `span_id:def`, `message:x` | field equals / contains |
| `attr.<key>:value`, `resource.<key>:value` | attribute equals value |
| `attr.<key>:>=500`, `<`, `<=`, `>` | numeric comparison |
| `timestamp:>=2026-01-01` | RFC3339, `YYYY-MM-DD` or unix ms |
| `-term`, `NOT term` | negation |
| `a b`, `a AND b` | both (implicit AND) |
| `a OR b` | either |
| `( ... )` | grouping |

`AND`, `OR`, `NOT` are keywords only in upper case; quote a word to search for
it literally. Level matching tries the as-typed, lower and upper spellings.

## Limits

Query length 1024 bytes, 20 terms, parenthesis depth 5, key
`^[A-Za-z0-9_.\-/]{1,128}$`, value 256 characters. Violations and syntax
errors return HTTP 400 with the 0-based `position` in the problem body.

## Paging and verification

Results are newest first, keyset-paged on `(timestamp, log_id)`:
pass the response `next_cursor` as `cursor`. `offset` is rejected with `lq`.
`limit` is capped at 200 (default 50).

All values are bound SQL parameters and column names come from a fixed list.
`attr.*` / `resource.*` terms (and message terms containing `%`, `_` or `\`)
are not expressed in SQL; the SQL narrows to a candidate window of at most
5000 rows and Go checks the rest. In that mode the response has
`"verification": "go_window"`, and `"truncated": true` means the window was
exhausted without filling the page; follow `next_cursor` to continue scanning.
Resource attributes live in the same stored attributes object as log
attributes, so `attr.k` and `resource.k` read the same key space.

The SQL path is unit-tested only; it has not been run against live Nucleus.
