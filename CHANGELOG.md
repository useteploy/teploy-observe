# Changelog

All notable changes to Observe are recorded here.

## [Unreleased]

### Added

- **The selected date range now survives navigation and reload.** It lived only
  in `useFilters`' in-memory state, so routing back to the dashboard from Errors
  or Traces snapped it back to "Last 24 hours" and any longer window had to be
  re-picked every time.

  It persists in `localStorage` under `observe.range`, following the pattern the
  site selection already uses, rather than going in the URL: the range applies
  across every route and belongs to the operator, not to a link, and a URL that
  pinned `from`/`to` would freeze a rolling window into a shared link. What is
  stored is the **label**, not the instants, for anything rolling — "Last 7
  days" names a window relative to now, so restoring frozen timestamps would
  quietly pin the dashboard to whenever it was first picked and it would stop
  showing today. Only a genuinely fixed selection (a hand-picked Custom range,
  or one the arrows stepped off its preset) restores its instants verbatim.

  The stored value is treated as hostile, because an older build or another tab
  can leave anything there: blocked storage, unparseable JSON, a missing field,
  an unparseable instant and a reversed range all fall back to the default
  range, and a **rolling label this build no longer offers** — four presets were
  removed in the entry above — falls back too rather than rendering a blank
  range. A *pinned* removed label keeps its dates and is relabelled Custom,
  since the dates are still real. `ui/src/utils/ranges.test.ts` covers each
  case. Bare routes (share links, embeds) deliberately do not adopt it.

### Changed

- **The date range menu carried three pairs of buttons that meant the same
  thing.** "This week" sat next to "Last 7 days", "This month" next to "Last 30
  days", "This year" next to "Last 12 months" — eleven entries asking for a
  choice that barely changed the answer. It is now the rolling-window set
  Plausible and Fathom settled on: Today, Last 24 hours, Last 7 days, Last 30
  days, Last 90 days, Last 12 months, All time, plus **Custom**, which is what
  actually covers a to-date or arbitrary window and is why the calendar-to-date
  entries could go. "Last 6 months" went with them — 90 days and 12 months
  bracket it. The label *is* persisted now (see "The selected date range now
  survives navigation" below), but only in one browser's localStorage and with
  an explicit fallback for a label this build no longer offers — no saved view,
  board or scheduled report stores one. The boards page keeps its own
  window list because its keys are persisted per board; only its "Last 24h"
  label was aligned to read "Last 24 hours".

### Fixed

- **Ten cron monitors filled the analytics chart with a solid block of orange.**
  `incidents` held 12,398 rows — 6,192 closed incidents at two rows each plus 14
  open, every one severity `warning` and source `cron`, from ten distinct
  monitors. Charts draw one translucent band per incident, so thousands of them
  compose into a wash of colour with the series invisible underneath.

  The detector measured a monitor against its **grace period alone** and never
  read its `schedule`. A cron that legitimately runs hourly with a five-minute
  grace is therefore missed for fifty-five minutes out of every hour: an
  incident opens, the next hourly ping closes it, and the cycle repeats — one
  incident per cron run, forever. It was never a dedup failure; every cycle's
  incident was genuinely new. `CheckMissed` now allows `last check-in +
  the schedule's period + the grace`, reading the period from the `@`-shorthands,
  `@every`, and 5- or 6-field cron expressions. A schedule it cannot read
  contributes zero, which is exactly the old behaviour, so nothing silently
  stops alerting.

  Two things made it worse. The dedup guard was `if active, _ :=
  ActiveByRule(...)`, which reads a *failed* query as "nothing is open" and
  declares another incident — on a 45s tick, one per tick for as long as the
  query keeps failing. And `ActiveByRule` read every row for the rule and
  collapsed them in Go, so it got slower as the table grew, on a table that was
  growing because of this. `incidents.EnsureOpen` replaces the pattern at both
  auto-declare call sites: it reuses the rule's open incident and returns lookup
  errors instead of swallowing them. The collapse to one row per incident now
  runs in the database (`argMax(col, updated_at) GROUP BY incident_id`), so the
  read returns one row instead of thousands, and `InRange` is capped so the
  marker overlay cannot become a megabyte of JSON.

- **A cron heartbeat client hanging up left its incident open forever.** The log
  carried `cron incident auto-resolve failed ... context canceled` on repeat.
  The hook that resolves a monitor's incident when it checks back in ran on the
  **check-in request's own context**, so `curl -m 5` timing out, or a cron job
  killed mid-ping, cancelled the close. It was a race the growing table kept
  losing. The hook now runs detached from the request with its own 15s deadline.

- **The chart now degrades legibly when the marker data is pathological.**
  Markers are clamped to the plot window, merged when they overlap or sit within
  4px of each other (per severity, so a merged band keeps a colour that means
  something), and capped at 30 bands; the count of incidents not drawn appears
  beside the legend. A window holding 6,206 incidents renders as a readable
  chart rather than a solid block, whatever the API returns.

  Migration **035** repairs the rows already on disk: rename aside, recreate,
  and copy back one row per monitor for cron incidents (closed) plus every
  non-cron incident unchanged. Unlike 034 it is small — measured complete inside
  a 48 MB query budget, 128 times smaller than the live accessory's, so nothing
  needs raising. `docs/operations/cron-incident-flood.md` has the analysis, the
  measured costs and the verification queries.

- **Export on the Incidents page threw on render.** It read a `resolved`
  variable that does not exist in that component; the state is called `recent`.
  A `tsc --noEmit` over the app surfaces it, which is worth doing before a
  release — the bundler does not typecheck.

- **The sort control on the breakdown panels did nothing on a small site, and
  the ten rows it showed were not the top ten.** Nothing decided the order of
  rows that tie, and on a low-traffic site nearly every browser, country and
  referrer sits at one or two visitors.

  Nucleus emits `GROUP BY` results in hash order, so `ORDER BY visitors DESC`
  by itself left the tied group unordered — verified on v0.1.8 with six paths
  tied at one view each, where `LIMIT 3` returned zebra/pear/fig and `LIMIT 10`
  returned pear/apple/fig/kiwi/zebra/mango. The truncated read was therefore an
  arbitrary three of the six rather than a prefix of the full list, which is
  what made "View all" reshuffle the rows already on screen instead of adding
  to them. Every breakdown now carries a label tie-break.

  One trap inside that fix, now in CLAUDE.md: Nucleus resolves `ORDER BY`
  against the select list's *output* names and silently ignores a term it
  cannot resolve. Screens, UTM and entry/exit pages all project their label
  under a different name than the source column, so the obvious tie-break there
  parses, runs, returns rows and does not sort.

  `TopChannels` had the same problem in Go: it built its result by ranging over
  a map — randomised order — and sorted on the count alone, so tied channels
  came back differently on every request. It also ignored the `limit` argument
  entirely. Both fixed, and the handler now passes the limit through.

  The dashboard half: the panel rendered the server's array verbatim for the
  default descending direction and only sorted on the ascending click. `Array.
  sort` is stable, so once every value tied, the ascending pass reproduced the
  order it was handed and the arrow appeared to do nothing at all. It now sorts
  both directions on the same value-then-label order, and the button says which
  metric it ranks on — the panels rank on pageviews or on visitors depending on
  which one they are, and an unlabelled arrow gave no way to tell.

  **Not caused by the replacing-rollup work below** — the tie order was
  undefined before it and the sort control had never sorted its default
  direction. The separate gap that work left is closed in the entry below.

- **Visitor figures were silently under-reported on any range longer than raw
  retention.** Moving unique counts off the non-additive rollup column and onto
  raw `events` fixed a 3-8x over-count but left the panels reading different
  windows: pageviews come from `stats_hourly` / `stats_daily`
  (`OBSERVE_HOURLY_RETENTION_DAYS`, 365, then daily indefinitely), uniques came
  from `events` (`OBSERVE_RAW_RETENTION_DAYS`, 30), and entry/exit pages from
  `sessions` (90). The date picker offers Last 90 days, Last 12 months and All
  time, so picking any of them put a full-range pageview number beside visitor
  numbers covering only the last 30 days, with nothing saying so.

  The source is now tiered by what can answer the range **exactly**, and the
  tier is decided by the range's `from`, not its length — a one-week window
  sitting a year back is outside raw retention just as much as a twelve-month
  one.

  1. Inside raw retention: `COUNT(DISTINCT session_id)` over `events`, as
     before.
  2. Past raw retention, inside session retention: the session-grain `sessions`
     table, which holds one row per session and therefore counts exactly and
     cheaply. This extends accurate uniques to 90 days at no cost. It covers
     the overview tiles, the visitor series, and the referrer, channel,
     browser, OS, device, screen, country, language and UTM breakdowns; entry
     and exit pages already read `sessions` and now agree with the rest.
  3. Past both: the figure the surviving data supports, plus an explicit marker
     that it covers a shorter window than the range. Returning the smaller
     number in silence was the bug.

  Both windows are read from the running retention policy rather than a
  constant, so raising `OBSERVE_RAW_RETENTION_DAYS` widens the exact tier with
  no code change, and a policy of 0 days (prunes nothing) never downgrades
  anything. `GET /api/v1/stats/unique-coverage` returns the tier, the window
  actually covered and the sentence to show; it runs no query. The dashboard
  renders it as a "Partial" note above the tiles whenever the answer is not
  exact.

  Two consequences worth knowing. A filter on `pathname`, `event_type` or a
  cohort's `distinct_id` names a column `sessions` does not carry, so such a
  read stays on raw events past raw retention — and gets the same marker
  naming the 30-day window rather than a quietly smaller number. And in the
  sessions tier the Sessions tile reports the session grain the bounce rate and
  average duration already use; the visit grain raw events give it
  (`COUNT(DISTINCT visit_id)`, a visit being one clock hour) has no
  session-grain equivalent.

- **Migration 034 could not run at all — on a fresh install or on the live
  upgrade path — because of a comment.** Nucleus v0.1.8's SQL lexer panics on a
  non-ASCII character followed by a number inside a `--` comment and drops the
  connection; the migrator reports it as `unexpected EOF` with nothing pointing
  at prose. `034`'s header said `1, 2, 4, 8 … 4096`, so every `schema.Apply`
  failed at 34 and the next deploy of the live instance would have crash-looped.
  Now ASCII. `TestMigrationsAvoidNucleusLexerPanic` rejects the shape without
  needing a database, and `TestMigrationsApplyToFreshDatabase` runs the whole
  chain against a scratch engine. The same character followed by a word is
  harmless, which is why the em dashes in seventeen other migrations are fine.

  034's note also claimed a failure rolls the whole migration back. It does not:
  DDL is not transactional in Nucleus — `BEGIN; ALTER TABLE t RENAME TO t2;
  ROLLBACK;` leaves the table renamed — so a failed copy leaves `issues` empty
  and the data in `issues_pre034`. The corrected note carries the recovery.

- **`backup` silently skipped four tables that do not exist, and 31 that do.**
  `Tables` named `monitors`, `crons`, `share_tokens` and a second copy of the
  report schedules as `reports`. None of those is a table, and a name that is
  not a table dumps zero rows and is recorded as a *successful, empty* table —
  so uptime monitors, cron monitors and share links had never been backed up.
  In the other direction the list simply omitted 31 real tables, `stats_daily`
  (the one rollup with no retention policy) among them. Neither failure produced
  any output. The list is now the schema's, and `TestTablesMatchSchema` compares
  it against `internal/schema/migrations` in both directions, so a table added by
  a future migration fails the build unless it is backed up or explicitly
  excluded with a reason. `restore` derives its allowlist from the same list, so
  it was refusing to restore the tables it was never given.

- **Eight more `INSERT … SELECT FROM <same table>` writers were doubling their
  table on every call** — the `issues` shape, in `experiments` (Start/Stop),
  `sso_configs` (Enable), `dashboards` (Delete), `dashboard_panels`
  (DeletePanel), `saved_views` (Delete), `users` (UpdateRole) and
  `scheduled_exports` (Delete). The five replacing tables now read through the
  argMax collapse before inserting, and their reads collapse too: a completed
  experiment could report as running, an enabled SSO provider as disabled, and
  `List` returned one entry per surviving version. The two plain mergetrees have
  no version column and so no collapse to read through, and are fixed on their
  own terms below.

  Two sites reported as doubling do **not** double and were left alone:
  `incidents.Close` and `exports.recordRun` bound their SELECT with `ORDER BY …
  LIMIT 1`, and Nucleus honours both inside an `INSERT … SELECT` (verified on
  v0.1.8; the live `incidents` table holds exactly 2 rows per closed incident,
  not 2^n). Tests now pin that, because the LIMIT reads like formatting and is
  the only thing standing between those two and the `issues` outcome.

- **A demoted admin could keep reading as an admin.** `users` is a plain
  mergetree with no version column, and `UpdateRole` appended a new row while
  `List` and `Get` read the raw table with no dedup at all — so after a demotion
  both the `admin` row and the `viewer` row existed and whichever came back
  first won. It now does a real replace (delete then insert, in one
  transaction), which leaves exactly one row per user, collapses any duplicates
  an earlier call wrote, and stops overwriting `created_at` with the edit time.

- **A deleted saved view came back as a nameless entry.** `Delete` appended a
  blank tombstone that `List` still returned, so the view could not be removed
  from the UI and the row stayed on disk forever. It hard-deletes now, matching
  `boards.DeleteBoard` and `DeleteGoal`; `List` still filters the tombstones an
  older install left behind. `scheduled_exports.Delete` likewise hard-deletes
  rather than appending an `enabled='false'` row, which also clears the run
  history that nothing could read once the export was gone.

- **The `issues` table was doubling in size on every error batch, and had
  reached 16,847,389 rows on the live instance.** `bumpIssue` and
  `UpdateStatus` were both

      INSERT INTO issues (...) SELECT ... FROM issues WHERE issue_id = $1

  which writes one row per row the SELECT returns. That is only harmless if
  the engine collapses a ReplacingMergeTree's superseded versions, and Nucleus
  does not: its read-time dedup is a process-global registry that CREATE TABLE
  populates and a restart repopulates only for the tables listed in the data
  directory's `engines.json` — and every table older than that file is absent
  from it. The live instance's `engines.json` lists exactly one table, so
  every read there returns every version, and each bump inserted as many rows
  as already existed. Measured on a scratch Nucleus v0.1.8: 1, 2, 4, 8 … 4096
  over twelve bumps. Both writers now read through the latest-version collapse
  before inserting, so exactly one row is written however many versions exist.

- **Every read of `issues` returned an arbitrary version.** An issue marked
  resolved could keep reading as open, the grouphash lookup that
  `ResolveIssue` uses on a cache miss could return a stale event count, and
  `ORDER BY last_seen` sorted on whichever `last_seen` came back. The issue
  list, the single-issue read, the grouphash lookup and the full-text search
  hydration all collapse by `argMax(col, version)` now — and `status`, which
  changes between versions, is filtered *after* the collapse rather than
  inside it.

- **Deleted alert rules, webhooks, uptime monitors, cron monitors,
  integrations, report schedules and log pipelines kept running.** All seven
  soft-delete by writing a new version with `enabled = 'false'`, and all seven
  read `WHERE enabled = 'true'` against the raw table — which still matches
  the pre-delete version. So a deleted alert rule kept evaluating and firing,
  a deleted webhook kept receiving payloads, a deleted uptime monitor kept
  issuing HTTP requests, a deleted cron monitor kept alerting on missed runs,
  a deleted integration kept being delivered to, a deleted report schedule
  kept emailing, and a deleted log pipeline kept processing (and dropping)
  log lines. Each now collapses to the latest version per key and filters
  `enabled` after the collapse; each soft-delete reads through the same
  collapse so it writes one row rather than one per version.

  **Security-relevant:** a cron monitor's ping token is the credential that
  authorizes a heartbeat, and deleting the monitor is the only way to revoke
  it. `RecordCheckinByToken` filtered `enabled` against the raw table, so a
  deleted monitor's token kept authenticating indefinitely.

  Report schedules had a second symptom from the same cause: a live schedule
  was read once per surviving version, so one send cycle mailed every
  recipient several times.

- **Feature flags and surveys resolved an arbitrary version to the SDK.**
  `Evaluate` took `rows[0]` of an uncollapsed read, so a flag turned off could
  keep evaluating true for every SDK caller (and one turned on could stay
  false). Surveys had the same shape on `GetActive` and — the one that
  matters — on `SubmitResponse`, whose site-ownership and status check is the
  only gate on a public, unauthenticated endpoint.

- **`DeleteGoal` did not delete.** It was a literal `return nil` with a
  comment claiming a ReplacingMergeTree cannot delete. It can: `DELETE`
  removes the physical rows, which is what the rollup jobs already depend on.
  It now deletes, scoped by `site_id` so a guessed goal id cannot reach across
  sites.

- **Backups captured every superseded version, and restore re-inserted them
  all.** A backup carried the same duplicates the read path has to work
  around, and restoring one could bring a soft-deleted webhook back alive.
  `observe backup` now dumps a ReplacingMergeTree collapsed to one row per
  key, discovering the column list at runtime so a column added by a later
  migration (`cron_monitors.ping_token`, `sessions.release_tag`) is still
  carried.

- **Migration 034 collapses the duplicate `issues` rows already on disk.**
  Same shape as 033: rename aside, recreate, copy the highest-version row per
  key across, `issues_pre034` left in place as a recovery artifact. It runs on
  the next start, before the listeners bind. **On an instance the size of the
  live one this needs more memory than Nucleus allows a query by default —
  see `docs/operations/issue-duplicate-collapse.md` before deploying.**

- **The dashboard reported several times more traffic than it had.** On the
  live instance a window whose raw events prove 72 pageviews and 11 sessions
  was shown as 158 pageviews and 91 visitors. Two independent causes, both
  fixed:

  *Duplicate rollup rows were being summed.* `stats_hourly`, `stats_daily`
  and `sessions` are ReplacingMergeTree tables and their rollup jobs
  deliberately recompute an overlapping window on every tick, relying on the
  engine to collapse the repeats down to the highest version. Nucleus does
  not do that reliably — it collapses within a memtable but leaves rows
  written into separate segments in place, has no OPTIMIZE or merge-now
  command, and `FINAL` parses but is silently ignored. The live `stats_hourly`
  was carrying 740 duplicated bucket keys out of 1956 rows, the oldest two
  months old, and every read summed them. Reads now collapse by version with
  `argMax` over the table's declared ORDER BY key, and each rollup job clears
  the window it is about to rewrite so no duplicate key is written in the
  first place.

  *Unique counts were being summed.* `visitors` in the rollups is a
  COUNT(DISTINCT session_id) taken per bucket and per pathname, so a session
  spanning two hours or visiting two pages was counted once per group.
  Summing them inflated the number even after the duplicates were gone —
  91 became 41 rather than 11. Unique visitor and session counts now always
  come from raw `events`, which is what the reference implementation umami
  does and what ranges under 24h already did; the same site no longer reports
  one figure below 24h and a three-to-eight-times larger one above it. The
  cost is that unique counts are now bounded by raw-event retention
  (`OBSERVE_RAW_RETENTION_DAYS`, default 30) rather than by rollup retention.
  Pageview counts keep the rollups' longer reach.

  Affected everywhere the two tables are read: the overview tiles, the
  pageview time series, top pages, top referrers/browsers/countries/OS/
  devices, entry and exit pages, the session list, bounce rate and average
  duration, retention cohorts, release health and crash-free percentage, and
  the sessions CSV/JSON export.

- **Migration 033 collapses the duplicate rows already on disk.**
  Non-destructive in the style of 027/028: rename aside, recreate, copy the
  highest-version row per key across. `stats_hourly_pre033`,
  `stats_daily_pre033` and `sessions_pre033` are left in place as recovery
  artifacts — drop them by hand once the copy is confirmed. `stats_daily` has
  no retention policy, so nothing else would ever have removed its copies.

- **The replay integration tests are repeatable.** They used fixed site and
  replay ids, so a second `go test ./...` against the same engine counted the
  previous run's rows and failed.

- **Every custom event property sent by the npm browser SDK was stored as
  `{}`.** `track()` spread the caller's props across the top level of the
  ingest payload, and the server reads properties only from a nested
  `properties` object — so `track("signup", { plan: "pro" })` stored an event
  with no properties at all. The SDK now nests custom props under
  `properties` and keeps only the keys the server actually reads as fields
  (`url`, `referrer`, `title`, `language`, `screen`, `distinct_id`,
  `release`) at the top level. The naive version of this fix — nesting
  everything — would have broken pageview attribution, because `pageview()`
  routes `url`/`referrer`/`title` through the same call; a test pins that.

  `identify(userId, traits)` was the same bug: traits went top-level and were
  discarded. They are now properties. The embedded tracker
  (`/observe.js`, what a `<script>` install loads) always sent the correct
  shape and was never affected, nor were the Go, Python or Sentry-shim SDKs —
  none of them post analytics events.

- **The ingest endpoint now collects unknown top-level keys into
  `properties`.** Payloads in the old flat shape are already deployed and
  cannot be recalled, and the `from-posthog` migration recipe taught the same
  flat shape, so their data was being dropped on arrival. An explicitly
  nested `properties` entry wins over a same-named flat key, and collected
  keys are capped at the existing 50-property limit in sorted order rather
  than pushing an otherwise-valid event over it and getting the whole event
  rejected.

## v0.1.8 — 2026-08-21

Thirty-five commits since v0.1.7 on 2026-07-15. The three entries already
written up below are the large ones; the rest of what shipped is recorded here.

### Added

- **A share token can read a service's RED metrics.** `GET
  /api/v1/traces/services` now accepts a share token (`X-Share-Token`) as well
  as a session JWT. This exists so a machine can read: a user JWT expires in 24
  hours and belongs to a person, and the ingest API keys are write-scoped, so
  before this there was no credential a worker could hold to read anything.
  teploy-ship uses it to put a service's measured before/after on a pull
  request.

  Deliberately narrow. It is GET-only, pinned server-side to the token's own
  site, long-lived and revocable — and it is the **only** trace route opened
  this way. Every other one returns payloads (waterfalls, span attributes,
  search) and still requires a session. Verified against a live engine rather
  than a mock: site pinning, non-GET refused, and revocation honoured.

- **Live stats accept a share token too**, for the same reason, and country
  resolution now handles IPv6 rather than leaving those visitors unattributed.

### Fixed

- **Browser ingest was rejected** because the tracker sent no site-scoped API
  key.
- **The container health probe timed out on its own database connect.**
  `/healthz` runs a real `SELECT 1`, and a cold pool connect measured ~7.8 s
  against a 3 s timeout, so the container flapped between healthy and not. The
  shape is worth remembering: a health endpoint that touches a dependency needs
  a timeout sized for a cold connection, not a warm one.
- **The traces service cache was keyed on window *length* rather than window
  *bounds***, so two different windows of the same duration shared an answer.
- **OTLP log exports over the batch cap were lost** instead of being split.
- Source-map retention could not run when an upload failed.
- Bootstrap, session, backup, explorer and worker-lifecycle security gaps.

### Changed

- **Ingest, span, log and metric inserts are batched** and retention deletes
  are chunked; traces and metrics no longer drag whole tables into Go.
- The nucleus engine is pinned by tag and its memory capped in the deploy
  config, rather than floating on `:latest`.

### Internal

- **Integration tests now skip unless the engine is actually Nucleus.** Every
  one of them resolved its DSN to `postgres://postgres@localhost:5432` by
  default and only checked that a connection *succeeded* — so on any machine
  running PostgreSQL the whole integration suite silently ran against the wrong
  engine and reported failures that look like regressions ("syntax error at or
  near ORDER" for MergeTree DDL, columns "missing" that Nucleus resolves to
  NULL). The guard now consults the client's own `IsNucleus()`.

### Added

- **OTLP protobuf ingest, and the logs signal.** `/v1/traces` and `/v1/metrics`
  previously accepted only `application/json` and answered protobuf with a 415,
  and there was no `/v1/logs` at all — so a stock OpenTelemetry SDK or Collector
  pointed at Observe exported nothing unless it was reconfigured with
  `OTEL_EXPORTER_OTLP_PROTOCOL=http/json`, and dropped logs entirely. All three
  signals now accept both `application/x-protobuf` (the exporters' default) and
  JSON. Protobuf is translated into the same request the JSON path builds, so
  ingest, validation and storage stay single-sourced. OTLP logs land in the same
  store as `/api/v1/logs`, so pipelines, search and the UI work on them with no
  extra wiring; severity maps to Observe's levels, and `service.name` plus the
  resource attributes are carried onto every record. Verified end to end against
  the real OpenTelemetry JS exporters. gRPC remains unsupported — use HTTP
  transport or a Collector.

- **Cross-product dashboard switcher.** A top-left dropdown (in the sidebar) lets you jump between the deployed Teploy dashboards — Dash, Observe, and Ship. Configure the sibling URLs with `TEPLOY_NAV_DASH_URL` and `TEPLOY_NAV_SHIP_URL` (same env convention across all three products); the switcher only appears once at least one sibling URL is set. Exposed via `/api/v1/config`.
- **Single sign-on (OIDC).** Observe can act as an OpenID Connect relying party: delegate login to your own identity provider (Okta, Azure AD/Entra, Google Workspace, Keycloak, Authentik — "generic OIDC") or to Teploy Platform acting as the IdP for Cloud. The IdP authenticates the user; Observe verifies the signed ID token (authorization-code flow with PKCE, state, and nonce) and maps a claim to the same admin/editor/viewer roles it already uses — a `teploy_role` claim wins, otherwise a group claim is matched to configured admin/editor/viewer groups, otherwise a configurable default (viewer). It then mints Observe's normal JWT, so the SPA and every downstream check treat an SSO session identically; the role is re-read from the token on every login, keeping the IdP authoritative. Password login stays available as the break-glass path. Enable with `OBSERVE_OIDC_ISSUER` + `OBSERVE_OIDC_CLIENT_ID` (see README for the full variable list). When SSO is configured, the first-run open-access grace period is disabled (authentication becomes required). The login page shows an SSO button when it's enabled.

## v0.1.7 — 2026-07-15

Versions v0.1.1 through v0.1.6 shipped without a changelog entry each — not reconstructed here. This entry covers everything since v0.1.6.

### Added

- **Compliance/audit trail subsystem**: an append-only, admin-only audit log (`GET/POST /api/v1/audit`) covering CLI, dash, and Ship as well as Observe's own admin actions. Every mutating admin API call is recorded automatically via middleware (actor, action, target, result, source IP/UA — denied 401/403 attempts included), not just login. Each record carries a keyed HMAC hash chain (`prev_hash`/`hash`) so tampering (modification, deletion, insertion) is detectable via `GET /api/v1/audit/verify`; the signing key lives outside the database. A compliance control-status view (`GET /api/v1/compliance`) reports which controls Observe can verify (audit logging, tamper-evidence, auth, RBAC, write protection). New admin Audit UI view.
- **Signed self-downloading upgrade**: `observe upgrade` fetches, SHA256-verifies, and Ed25519-signature-verifies a release before a health-gated swap; refuses downgrades and rolls back on a failed post-upgrade health check. Systemd-owned lifecycle where installed; Homebrew/container-managed binaries point at the right channel instead of attempting an in-place upgrade. **Load-bearing: this release must be the one downloaded by any instance still on ≤ v0.1.6, since only this and later releases carry the `checksums.txt.sig` asset the new verifier requires.**
- **MinIO accessory** as a self-hosted backup + restore-test target — no external S3 dependency required.
- `/api/v1/logs/batch` — batched log ingest (mirrors the existing `/events/batch` shape, capped at 200 entries, per-entry failure isolation) for real structured-logging clients that flush in batches rather than one request per line.
- Exact trace↔error correlation via `trace_id`/`span_id` (previously a timestamp-window heuristic that could cross-contaminate overlapping traces). Go SDK gains `WithSpan()`; browser and Python SDKs gain passthrough fields.
- Site-scoped error full-text search (each error indexed with a `site_id` facet, search scoped via Nucleus's faceted FTS) so a busy site's results no longer crowd out other sites' search hits.
- `getSessionId()` on the browser SDK, so a separately-loaded script (e.g. the replay tracker) can correlate its recording with the analytics session instead of defaulting to an orphaned empty session ID.
- **Source maps are now retained per release instead of forever.** Every upload
  was stored with no expiry and no delete path anywhere in the service — one
  entry per site x release x file, up to 10 MB each. A production instance
  accumulated 4.8 GB of them, which was enough to push its database past its
  memory limit and make it reject writes for 32 hours. Observe now keeps the
  10 most recently uploaded releases per site (`OBSERVE_SOURCEMAP_KEEP_RELEASES`),
  ordered by last upload so a release stays current while it is still being
  published to. Existing installations reclaim their space on the next upload:
  releases predating the age index sort oldest and prune first. Not implemented
  as a TTL on purpose — Nucleus keeps entries carrying an expiry resident in
  memory, so a TTL would have pinned every source map in RAM until it expired.
- Sourcemap upload now also accepts the site-scoped ingest API key (not just an editor JWT), so CI can upload build-time sourcemaps without a stored interactive-login credential.

### Fixed

- **Browser SDK events were silently dropped entirely**: Go 1.22's stricter method routing rejected the CORS preflight `OPTIONS` request before the CORS middleware ever ran, so every browser `fetch()` with an API key 405'd. Explicit `OPTIONS` handler added ahead of the routing change.
- **Session replay could never actually send data on any site with an API key configured** (the normal state past initial setup) — `sendBeacon` can't carry custom headers and the XHR fallback never set one either, so every replay batch and rage-click report 401'd silently. Now uses `fetch(..., {keepalive:true})` with the key attached when one is configured.
- **OTLP ingest rejected real exporters.** Tracing and metrics ingest didn't decompress gzip bodies (`@vercel/otel`, `OTEL_EXPORTER_OTLP_COMPRESSION=gzip` both 400'd) and didn't accept bare-JSON-number `intValue` attributes (only quoted-string, per a stricter spec reading than what `@vercel/otel`/Next.js actually emit) — real-world exporters couldn't land a single span or metric. Both fixed with size-bounded gzip decompression and a lenient int type.
- **Several JSONB ingest paths silently dropped rows.** Nucleus discards an `INSERT` outright when a JSONB column receives an empty string rather than `null` or a real value — the request reported success (and rollups still wrote) but the underlying row never persisted. Fixed across event properties, errors/llm/logs/replays/surveys ingest, and span attributes/resource/events; audited and fixed at every other JSONB write site in the codebase (view/dashboard config, flags, surveys, SSO, log pipelines) so a lenient-era `''` value can no longer reach the database again. A companion backup-restore fix lands lenient-era `''` values already in old archives as `NULL` on restore against current Nucleus, which rejects them outright.
- `events`/`events_recent` tables dropped inserts from new connections after their post-creation `ALTER TABLE ADD COLUMN` migrations, due to a Nucleus MergeTree engine bug — recreated as plain OLTP tables (data loss accepted; the rows were never actually persisting).
- Timestamp range filters (`WHERE timestamp >= $n`) across stats/goals/funnel/retention/correlation/journeys always returned zero rows — a Go string timestamp was encoding as a quoted text literal that Nucleus can't implicitly cast to `BIGINT`. Now passed as raw `int64`.
- Performance-issue detection counts no longer collapse to 1 on every write — persistence now accumulates via an atomic KV counter instead of relying on read-time dedup of a `replacing_mergetree`, which was overwriting the running count on every detection.
- Cron monitor creation via the HTTP API never set the monitor's `slug` (the input struct was missing the field), so no heartbeat could ever match a monitor created that way — likely never exercised end-to-end before now.

### Security

- `/api/v1/llm/ingest` had no auth middleware at all despite a comment claiming otherwise — anyone could POST arbitrary usage/cost data into any site. The already-existing, correctly-authenticated handler was dead code, never wired to a route; it's wired in now.
- A `teploy.deploy-test.yml` with real secrets inline (session salt, JWT secret, admin password) was sitting untracked in this publicly-mirrored repo. Removed, and `teploy.*.yml` (except the base file) is now gitignored.

### Docs

- `teploy.yml` now documents the persistent-secret pattern (`teploy secret set`) for deploy env instead of the shell-export pattern, which silently ran with empty JWT secret / session salt / admin password whenever nobody remembered to re-export the vars.

## v0.1.0-rc1 — 2026-05-10

First public release candidate.

### Product surface

- **Analytics** — pageviews, sessions, visitors, bounce, duration, top
  pages/referrers/UTM, channel classification, browser/OS/device/country
  breakdowns, custom events with property drill-down, funnels, retention
  cohorts, user journeys, goals, real-time active visitors. Cookie-free.
- **Error tracking** — automatic grouping (MD5 of type + in-app frames),
  stack viewer + source maps, BM25 full-text search, status workflow
  (open / resolved / ignored), release health, breadcrumbs,
  error-to-session correlation.
- **APM / distributed tracing** — OTLP HTTP/JSON ingest, service list +
  RED metrics, waterfall + flame graphs, dependency map, p50/p95/p99
  latency, performance-issue detectors (N+1, slow DB, consecutive DB,
  slow HTTP).
- **Logs** — OTLP HTTP ingest, structured search, level/service/host
  filters, log → trace correlation.
- **Session replay** — rrweb capture, privacy masking, replay viewer
  with network/console/breadcrumb tracks.
- **Heatmaps** — click + scroll + dead-click tracking with overlay UI.
- **Monitoring** — uptime checks (HTTP/TCP), alert rules
  (event count, error rate, p95 latency, custom SQL), Slack/email/webhook
  routing, incident markers overlaid on every time-series chart.
- **Feature flags + experiments** — boolean / multivariate, sticky
  bucketing, multi-touch attribution (first / last / linear).
- **Surveys + LLM tracing + custom dashboards + insights builder.**
- **Persons + cohorts** — aggregate events by `distinct_id`, build
  rule-based cohorts, filter analytics by cohort.
- **Multi-site / multi-tenant** — site switcher with per-site rate
  limits, RBAC, and `session_salt` HMAC for distinct-id privacy.

### Differentiators (no competitor ships these together)

- **AI query assistant** on the SQL explorer. English in, SQL out (your
  LLM key, your cost). Every call logged back to the LLM-tracing table.
- **Incident-mode markers** overlay translucent vertical bands on every
  time-series chart for the alert window.
- **Scheduled SQL exports** to any S3-compatible bucket (S3, R2, MinIO).

### Hardening (Phase 1)

- Disk-backed ingest WAL (`internal/ingest/queue.go`) — events survive
  `SIGKILL` and replay on restart.
- Per-site rate limiting with per-site caps (`sites.ratelimit_per_second`).
- Role-based access control: JWT carries a `role` claim; writes require
  `admin` or `editor`.  Viewer is reads-only.
- Tokenising lexer replaces the read-only-guard regex on the SQL
  explorer; comment and stacked-statement bypasses are rejected.
- `?token=` query-string auth accepted only on `GET` and only on the
  streaming / download route allowlist.
- Ingest batch writes use parameterized multi-row `INSERT` via Nucleus'
  SimpleProtocol; `escapeSQL` and the string-concatenated path removed.

### SDKs (MIT, separately versioned)

- `sdk/go`, `sdk/browser` (npm), `sdk/sentry-shim` (npm, drop-in
  `@sentry/node` replacement), `sdk/python` (PyPI). Counter / Gauge /
  Histogram + OTLP traces + slog handler in the Go SDK.

### Distribution

- Homebrew tap (`useteploy/tap/observe`) and Scoop bucket
  (`useteploy/scoop-bucket`) built by goreleaser on every `v*` tag.
- One-line installer at `scripts/install.sh` (POSIX shell — Alpine/
  Debian/macOS).
- Container image at `ghcr.io/useteploy/teploy-observe`.

### Known limitations

- Cross-connection `UPDATE` visibility lag of ~1s due to Nucleus's
  eventual-consistency model (Nucleus #2).
- `CAST(<aggregate> AS TEXT)` and `GROUP BY ... HAVING COUNT(*) >= N`
  have Observe-side workarounds pending Nucleus #24 / #28 fixes.
- Public demo instance not yet hosted; self-host via Docker compose
  or `scripts/install.sh`.
