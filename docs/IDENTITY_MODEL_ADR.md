# Identity Model ADR (O03)

Status: ACCEPTED (era-1 pinned) — 2026-09-21. Programme workstream O03,
"One explicit identity and session model" (P0 for product analytics).
This record is the pre-edit obligation: it locks the entity model,
semantics targets, and salt/time policies BEFORE any identity code
changes. No production identity code changed in this slice.

The executable oracle for current behavior is
`internal/session/reference_test.go` (scenarios a-e + salt eras). A
future behavior change must bump the era, update those tables in the
same change, and record the new era here. Historical data is never
rewritten to match a new era.

## 1. Current derivation, as verified (era-1)

| ID | Derivation | Where |
| --- | --- | --- |
| `session_id` | `sha256(site + ip + rawUA + globalSalt + YYYY-MM)` formatted UUID-style; YYYY-MM is the **UTC month of the server clock at processing time** | `internal/session/session.go` `ID()` |
| `visit_id` | `sha256(session_id + ":" + hourBucket)`; hourBucket = timestamp truncated to an absolute (UTC-epoch) hour; the ingest path passes **ingestion time** | `internal/session/session.go` `VisitID()`, `internal/ingest/handler.go` `prepareEvent` |
| stored `timestamp` | ingestion time (`now.UnixMilli()`); the wire protocol has **no client event-time field** — an unknown `timestamp` key is folded into `properties` | `internal/ingest/handler.go` |
| `distinct_id` (persons) | `HMAC-SHA256(rawIdentifyValue, per-site salt)` truncated to 16 hex; per-site `raw_distinct_id` opt-out stores raw; **global-salt fallback only for unknown sites** | `internal/identity/hash.go`, `internal/ingest/handler.go` |
| replay `session_id` | client-generated random id per tracker instance (per page load), rotated client-side on logout (`reset()`); stored on `replay_sessions`; a separate identity space from analytics `session_id` | `sdk/browser/src/index.ts`, `internal/replays/replays.go` |

Boundary conditions: bot UAs (`IsBot`) are dropped before identity;
site_id is in the fingerprint, so identities never cross sites; the UA
enters the fingerprint **byte-exact** (no normalization beyond HTTP
header trimming; `ParseUA` feeds display columns only, never identity).

## 2. Entity model

| Entity | Definition | Status |
| --- | --- | --- |
| Anonymous visitor **estimate** | Today's `session_id`: the site/IP/UA/salt/month fingerprint. **This is an estimate, not people.** It merges same-browser users behind one NAT, splits one person across UA version bumps, devices, salt eras, and UTC month boundaries (see oracle scenarios b-d). | Current; kept as an explicitly-labeled estimate |
| Persistent anonymous client ID | A client-generated, cookie/localStorage-backed stable id for anonymous visitors, sent when the site enables it. | **Target entity — not implemented.** The classic tracker's random per-page-load `sessionId` is NOT this and must never be persisted as one. |
| Identified person | `distinct_id`: the server-side HMAC of the SDK's `identify(userId)` value under the per-site salt (or raw, per opt-out). The only cross-device, cross-fingerprint unifier today. | Current (persons surface) |
| Visit / session | Naming is inverted in the code: the column `session_id` is the **monthly visitor estimate**; the column `visit_id` (one clock hour of one estimate) is what every other product calls a session. UI labels follow the columns (`visitors` = DISTINCT session_id, `sessions` = DISTINCT visit_id). | Current; renaming is a UI-label-first change (D6), never a data rewrite |
| Replay session | `replay_id` (client-generated) + its client `session_id`. Correlated to persons via `distinct_id` only; never conflated with analytics `session_id`. | Current |
| Device | `ParseUA`-derived browser/OS/device attributes — a breakdown dimension, **not** an identity. | Current |

Query-visible entity per surface (pinned):

| Surface | Entity grouped on | Where |
| --- | --- | --- |
| Funnels | `session_id` (monthly estimate — funnel steps may span hours/days within a month) | `internal/query/funnel.go` |
| Retention | `session_id` (cohorts from `sessions.first_ts`, UTC day/week buckets) | `internal/query/retention.go` |
| Stats/boards/coverage | `visitors` = DISTINCT `session_id`; `sessions` = DISTINCT `visit_id` | `internal/query/stats.go`, `boards.go`, `coverage.go` |
| Persons | `distinct_id` (anonymous `''` excluded by default) | `internal/persons/persons.go` |

## 3. Semantics targets (identify / alias / merge / logout)

- **identify** — today: the SDK stores the id client-side and attaches
  `distinct_id` (HMAC'd server-side) to subsequent events, plus a
  `$identify` event. No association between the pre-identify anonymous
  estimate and the person exists or is reconstructed. Target: unchanged
  at the boundary; internal association becomes explicit and queryable
  (first-seen-anonymous linkage), with history left as-is.
- **alias** — SUPPORTED as an explicit editor action since C3 (see
  section 8). Before C3: two distinct_ids are two persons
  forever. Target: an alias mapping (one canonical person id + alias
  table) resolved at query time; historical rows are never rewritten —
  queries resolve aliases forward from the mapping (D7-era rule).
- **merge** — SUPPORTED explicitly since C3 (section 8; `from` becomes
  an alias of `into`, resolved at read time; no auto-merge on identify).
  Original target text: merge is a versioned person-graph
  operation (two canonical ids become one going forward; prior rows keep
  their ids and the merge table unifies them at read time). Silent
  history rewriting is prohibited.
- **logout** — today: purely client-side (`reset()` clears the id and
  rotates the client replay session id); the server is never told and
  the estimate/visit derivations are unaffected (oracle scenario a).
  Target: a `$logout` event marks the end of the identified period;
  post-logout anonymous events are not attributed to the person.

## 4. Salt persistence and rotation policy (D7)

Facts today (pinned by `TestReference_SaltEras`):

- **Unset `OBSERVE_SESSION_SALT`** -> the server generates a random
  per-process salt (startup warning in `cmd/observe/main.go`): every
  restart silently begins a new session/visit era. Running this way in
  production is a defect condition, not a mode.
- **Fixed salt** -> all derivations are pure functions of their inputs;
  IDs are stable across restarts by construction (no process state).
- **Global-salt rotation** -> every `session_id` and `visit_id` changes
  (new era). `distinct_id` on a site-backed install does NOT change
  (per-site salt lives in the `sites` table and is independent); in the
  unknown-site fallback the global salt keys the HMAC, so persons
  re-key there.

Policy: rotation creates a **salt era**. Nothing happens to historical
rows — they are versioned by the era they were derived under and are
never rewritten; new rows use the new era's derivations; queries that
span eras report **mixed coverage** (one human can appear as multiple
estimates/persons, exactly like the month-boundary split) and surfaces
that care must label era boundaries rather than deduplicate them by
guesswork. Era bookkeeping (stamping rows/events with an era marker)
is O04+ implementation work; this ADR fixes the semantics.

## 5. Event-time vs ingestion-time (D8)

Current code uses **ingestion time exclusively**: the visit hour bucket,
the month key, and the stored `timestamp` are all the server's
processing clock; the wire protocol carries no client timestamp at all.
Delayed delivery therefore lands an old event in the ingestion-hour
visit, the ingestion-month estimate, and the ingestion-time series
position (oracle scenario e).

The model **requires client event time**: bucketing, funnel ordering,
and retention cohorts must key on when the event happened, with
ingestion time stored alongside for delay diagnostics. Acceptance of
client timestamps must bound clock skew (clamp/drop policy decided in
the implementing slice). Until then, the oracle pins era-1's
ingestion-time behavior as a known limitation, not a decision.

## 6. Decision list

- **D1 — One vocabulary.** The entities above are the only identity
  vocabulary; code/docs/UI must use their names. "Visitor" in
  session-keyed surfaces always means the monthly estimate.
- **D2 — Estimate, not people.** The anonymous fingerprint is labeled an
  estimate everywhere it surfaces. Its documented failure modes (NAT
  merge, UA-version split, month split, salt-era split) are product
  documentation, not bugs to hotfix silently.
- **D3 — Persistent anonymous client ID** is the target replacement for
  the estimate where accuracy matters; opt-in per site, never presumed.
- **D4 — Identified persons are the cross-device entity.** Session-keyed
  surfaces (funnels/retention) eventually gain person-level grouping as
  an explicit mode; the default entity per surface stays as pinned today
  until that ships.
- **D5 — No silent history rewrites.** identify/alias/merge/logout and
  salt/time changes add versioned behavior and coverage; they never
  mutate stored IDs. Historical data lacks information new IDs cannot
  recreate — the tables in the oracle are the record of exactly what
  each era knew.
- **D6 — Naming.** `session_id`/`visit_id` column names stay (schema
  stability); UI labels move to "visitors (estimate)" / "sessions"
  language in the O03 UI slice.
- **D7 — Salt eras** per section 4; fixed salt is the required
  production posture; unset salt warns (existing behavior).
- **D8 — Event-time required** per section 5; era-1 ingestion-time
  behavior is pinned as a limitation.
- **D9 — Replay sessions stay a separate identity space**, joined only
  via `distinct_id`/`replay_id`, never via the analytics fingerprint.
- **D10 — Device is an attribute**, not an identity.
- **D11 — Recommended default (programme decision table):** anonymous
  and identified are both supported explicitly, and the query entity is
  visible per surface (surface labels always name whether the grouped
  entity is estimate/session/person).

## 8. C3 additions: properties, alias/merge, erasure (migration 061)

Status: IMPLEMENTED (2026-10-03), compile+unit tested; SQL UNVERIFIED
against a live Nucleus. Persons remain an aggregate over
`events.distinct_id`; three side tables keyed by the same `person_key`
(the HMAC'd id, or raw under the site's `raw_distinct_id` opt-in) add data
without touching events.

Supported now:

- **Person properties** (`person_properties`). `POST /api/v1/persons/properties`
  (telemetry API key; site bound by the key, a disagreeing body `site_id` is
  403) takes the RAW identify value, hashes it exactly like event ingest
  (per-site salt, or raw on opt-in) and stores traits under the hashed key.
  The raw id is never stored. Bounds: <= 50 keys, key `^[A-Za-z_$][A-Za-z0-9_.$-]{0,63}$`,
  scalar values only (string/number/bool; null removes in merge mode),
  <= 1 KiB per encoded value, `user_id`/`distinct_id` keys rejected so the
  raw id cannot be echoed back. There is NO PII denylist: what a customer
  puts in a trait (including email) is the customer's choice and their
  responsibility. The browser SDK's `identify()` still sends traits only as
  `$identify` event properties (with its own email/user_id filter); wiring
  it to this endpoint is a separate SDK change. Caveat: a browser-embedded
  key lets any holder write properties for a person whose raw id they can
  guess - the same trust level as posting events with a chosen distinct_id.
  Person detail returns `properties`.
- **Alias/merge** (`person_aliases`). `POST /api/v1/persons/merge`
  (editor+; body `site_id`, `from_key`, `into_key`). `from` becomes an alias
  of the canonical root of `into`. Refused: self-merge, merging a key that is
  already an alias, merges that close a cycle, tombstoned keys, keys with no
  events in THIS site (404, so a key from another site cannot be linked),
  more than 20 aliases per person or 10000 per site. List and detail resolve
  aliases forward in Go over the bounded alias set; canonical properties win
  over alias properties. Events are never rewritten (D5). Merged
  `session_count` is an upper bound (a shared monthly estimate counts
  twice). The merged list scans at most 5000 persons per window and reports
  `truncated` beyond that. There is no un-merge endpoint yet (the table
  supports it: `active='false'`).
- **Erasure** (`person_tombstones`). `POST /api/v1/persons/erase` (admin
  only; key in the body so a raw id never lands in the audit path). Writes a
  tombstone for the key and every alias merged into it, deactivates those
  alias edges and blanks their stored properties (as new versions; no
  DELETE). Tombstoned persons disappear from list and detail, cannot be
  merged, and refuse property writes.
- All mutating persons routes are recorded by the audit middleware
  (actor, `persons.<verb>.create`, result).

NOT supported / honest limits:

- **Events are not deleted.** The repo has no per-key DELETE on `events`
  (retention deletes by time column only) and per-person DELETE semantics on
  Nucleus are unproven. Erasure therefore hides and de-identifies the person
  at the persons surface only; the hashed `distinct_id` still sits on event
  rows, and in session/funnel/retention aggregates, until retention expires
  them. A new event with the same identify value re-creates a visible
  person, because ingest does not consult tombstones (only property writes
  do). A true erasure that removes event rows needs an upstream-verified
  per-key DELETE plus ingest-side tombstone checks; neither is built.
- Auto-merge on identify, pre-identify anonymous linkage, un-merge, and
  person-level funnels/retention remain residual scope (section 7).

## 7. Residual O03 scope (next slices)

The identity model implementation itself (client ID + era stamping +
event-time acceptance), alias/merge query resolution, person-level
funnel/retention modes, UI entity labeling (D6/D11), and the schema work
they need. None of it is started here; the oracle tables are what those
slices will evolve deliberately.
