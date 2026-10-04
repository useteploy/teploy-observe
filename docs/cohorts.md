# Cohorts

A cohort is a saved set of identified users (`events.distinct_id`). Anonymous
events (`distinct_id = ''`) never belong to a cohort. Three kinds share the
`cohorts` table; the kind is in the stored `rule` JSON.

## Rule cohorts

```json
{"op":"and","rules":[{"type":"event","name":"purchase","min_count":2,"window":"30d"},
                     {"type":"property","key":"country","operator":"=","value":"US"}]}
```

That flat-AND form (v1) is still read and written. A rule can also be a tree:

```json
{"op":"and","children":[
  {"leaf":{"type":"event","name":"signup"}},
  {"op":"not","children":[{"leaf":{"type":"property","key":"country","value":"US"}}]},
  {"op":"or","children":[{"leaf":{"type":"event","name":"purchase"}},
                         {"leaf":{"type":"event","name":"trial_start"}}]}]}
```

- `op` is `and`, `or` (one or more children) or `not` (exactly one child).
  A node is a group (`op` + `children`) or a leaf (`leaf` only). `rules` (flat)
  and `children` cannot be mixed in one node.
- Limits, enforced on write with 422: depth at most 4 (the root group is level
  1, so a flat rule is level 2), at most 30 leaves. Leaves are validated:
  event name required, property key from the allow-list, operator `=` or `!=`,
  window like `24h` / `7d` / `90m`.
- `!=` on a property means "no event of the user has this value".
  `not` is the complement against the site's identified users.
- Evaluation: each leaf is one bounded query (at most 200,000 rows; over that
  the evaluation fails with 422, never a truncated answer); the tree is
  combined in Go with set intersection, union and difference. No SQL
  subqueries are involved.

## Static cohorts

An explicit list of ids in `cohort_members` (migration 060); the cohort's rule
is `{"op":"static"}`. At most 100,000 distinct ids per cohort and per request
(ids are trimmed, de-duplicated, at most 256 bytes, no control characters).
Request bodies are capped at 8 MB.

| Route | Body |
|---|---|
| `POST /api/v1/cohorts/static` | `{site_id,name,description,ids[]}` create |
| `POST /api/v1/cohorts/import?site_id=&name=` | CSV body, create |
| `POST /api/v1/cohorts/{id}/members` | `{site_id,ids[]}` add |
| `POST /api/v1/cohorts/{id}/members/remove` | `{site_id,ids[]}` remove |
| `POST /api/v1/cohorts/{id}/import?site_id=` | CSV body, add |

CSV: the first column is the id; a leading header row (`id`, `entity_id`,
`distinct_id`, `user_id`) and blank rows are skipped. Rule cohorts reject member
edits (422); a static cohort rejects a rule.

## Using a cohort as a filter

`cohort_id` is accepted by the stats routes (SQL `distinct_id IN (...)`) and,
since C2 depth, by `POST /stats/funnel`, `POST /stats/funnel/breakdown` (JSON
field) and `GET /stats/retention` (query param). The funnel and retention paths
stream raw events and test each event's `distinct_id` against the resolved
membership, so only identified activity counts and sessions with no identified
event drop out.

Failure behaviour is the same everywhere and never falls back to unfiltered:

| Condition | Status |
|---|---|
| cohort missing for this site, or belongs to another site | 404 |
| more than 30,000 members (`cohorts.MaxFilterMembers`) | 422 |
| query admission refused (concurrency / time budget) | 429 / 504 |
| store failure, unreadable stored rule | 503 |

Cohort resolution runs inside the query admission slot (`beginQuery`) of the
funnel / retention request, and takes its own slot on the stats routes.
