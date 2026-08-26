# The cron incident flood (migration 035)

## What it looked like

The analytics chart was almost entirely filled with translucent orange, striped
with vertical lines. Every time-series chart draws one shaded band per incident
overlapping the window; the bands are translucent, so N overlapping bands
compose N times the alpha and the series underneath disappears.

Read-only against the live instance on 2026-08-25:

| | |
| --- | --- |
| rows in `incidents` | 12,398 |
| distinct titles | 10 |
| severity | `warning`, all of them |
| source | `cron`, all of them |
| rows with `ended_at = 0` | 6,206 |

Those numbers resolve exactly: `incidents` is a plain mergetree with no version
column, `Create` writes one row and `Close` appends a second, so
`2C + O = 12,398` and `C + O = 6,206` give **6,192 closed incidents and 14 open
ones** — 6,206 distinct incidents from ten monitors.

The app log also carried, repeatedly:

```
cron incident auto-resolve failed ... err="nucleus: rows: context canceled"
```

## Why they duplicated

`CheckMissed` measured a monitor against its **grace period alone** and never
looked at its `schedule`. A cron that legitimately runs hourly with a
five-minute grace is therefore "missed" for fifty-five minutes out of every
hour:

```
ping ──── 5 min grace ──── MISSED, incident opens ──── … 55 min … ──── ping, incident closes
   └───────────────────────────── repeat, forever ─────────────────────────────┘
```

One incident per cron run. Ten monitors on schedules coarser than their grace
get to twelve thousand rows in a few weeks. This was never a dedup failure —
the dedup worked, and each cycle's incident was genuinely a new one. The
detector was simply wrong about when a cron is late.

Two things made it worse rather than causing it:

* **The dedup guard swallowed its error.** `if active, _ := ActiveByRule(...);
  len(active) > 0 { skip }` reads a *failed* query as "nothing is open" and
  declares another incident. On a 45s tick that is one per tick for as long as
  the query keeps failing.
* **`ActiveByRule` read every row for the rule and collapsed them in Go.** With
  thousands of rows per monitor that is thousands of rows hydrated on every
  tick, and it got slower as the table grew — a table that was growing because
  of the bug above.

## Why auto-resolve failed with `context canceled`

`OnCheckin` — the hook that closes a monitor's open incident when it checks
back in — ran on the **check-in request's own context**. A heartbeat client that
hangs up (`curl -m 5`, a cron job killed mid-ping, a lost connection) cancels
that context, and the close never lands. The incident stays open, which is what
put a band running to the right-hand edge of the chart.

It was a race the growing table kept losing: `CloseByRule` does one full-table
read plus one `INSERT … SELECT` per open incident, and once that took longer
than the ping client's timeout, every check-in cancelled itself.

## The fix in the code

* `internal/monitoring/schedule.go` — `SchedulePeriod` estimates how often a
  schedule fires (`@`-shorthands, `@every`, 5- and 6-field cron). Deliberately
  an estimate: being generous costs a slightly late alert, being wrong the other
  way costs a false one that repeats every tick.
* `internal/monitoring/monitoring.go` — `CheckMissed` now compares against
  `last check-in + SchedulePeriod + grace`, using `MAX(timestamp)` (a
  single-column aggregate, the shape that streams) instead of a count inside the
  grace window. A monitor that has never checked in is measured from its
  creation time. An unreadable schedule contributes 0, which is exactly the old
  behaviour, so nothing silently stops alerting.
* `internal/monitoring/monitoring.go` — the `OnCheckin` hook runs on
  `context.WithoutCancel` plus a 15s deadline, so a client hanging up cannot
  cancel the close.
* `internal/incidents/incidents.go` — `EnsureOpen` replaces the
  lookup-then-Create pattern at both auto-declare call sites. It reuses the
  rule's open incident and **returns lookup errors instead of swallowing them**,
  so a failed query declares nothing.
* `internal/incidents/incidents.go` — the collapse to one row per incident now
  runs in the database (`argMax(col, updated_at) … GROUP BY incident_id`), so
  `ActiveByRule` returns one row instead of thousands. `InRange` is capped at
  `MaxInRange` (1000) so the marker overlay cannot become a megabyte of JSON.
* `ui/src/utils/incidentMarkers.ts` — `prepareMarkers` clamps markers to the
  plot, merges anything overlapping or within 4px of a neighbour (per severity,
  so a merged band keeps a meaningful colour), and caps the result at 30 bands,
  reporting how many incidents are not drawn. The chart shows that count beside
  its legend. This is the part that holds regardless of what the API returns.

## The migration

`internal/schema/migrations/035_collapse_cron_incidents.up.sql`, in the style of
027/028/033/034 and non-destructive:

1. `ALTER TABLE incidents RENAME TO incidents_pre035`. No `IF EXISTS` — Nucleus
   resolves the table before it consults that flag, so `ALTER TABLE IF EXISTS`
   errors anyway; `incidents` is guaranteed to exist from 013.
2. Recreate `incidents` with its original DDL.
3. Copy non-cron incidents across as one row each, the highest-`updated_at`
   version.
4. Copy cron incidents across as **one row per `(site_id, rule_id)`** — one per
   monitor — carrying the most recent version's values and **closed** at
   `MAX(updated_at)`.

Closing them is deliberate. A months-old cron incident left open renders as a
band running to the right edge of every chart forever, which is not
information. If a monitor is genuinely still silent, the detector opens a fresh
incident 45s after start and that one is true.

`incidents_pre035` is **left in place deliberately**. The rename does not copy
data, so the artifact costs no extra disk. A fresh install ends up with one
empty artifact.

### Memory — unlike 034, nothing needs raising

The collapse materialises every row it groups over, and Nucleus caps a single
query's working set at `query_memory_percent` (75) of `server.max_memory_mb`.
Measured on a scratch Nucleus against 12,404 rows seeded to the live shape
(6,192 closed cron incidents, 14 open, 6 manual):

| query budget | result |
| --- | --- |
| 48 MB (`max_memory_mb = 64`) | completes, whole migration 3.4 s wall |
| 4608 MB (`max_memory_mb = 6144`) | completes, whole migration 2.7 s wall |

48 MB is **128 times smaller** than the live accessory's 6144 MB budget, so
there is nothing to raise and no chunking to think about. Both figures are on a
debug build; a release build is faster. Contrast 034, whose 16.8M-row collapse
needs about 10 GB and is blocked until the accessory's memory is raised — see
`issue-duplicate-collapse.md`.

`DELETE` against a Nucleus mergetree does not reliably remove every row, which
is why this is a rename-aside rather than a delete.

### If the copy fails

DDL is not transactional in Nucleus: `BEGIN; ALTER TABLE t RENAME TO t2;
ROLLBACK;` leaves the table renamed. So a failure leaves `incidents` as the new
empty table with every row in `incidents_pre035`. No data is lost, but the
marker overlay reads empty until you recover:

```sql
DROP TABLE incidents;
ALTER TABLE incidents_pre035 RENAME TO incidents;
```

## Verify after deploying

```sql
-- One row per monitor. 10 on the live instance.
SELECT COUNT(*) AS n FROM incidents WHERE source = 'cron';

-- No cron incident left ongoing by the migration itself. Should be 0 at first;
-- it climbs only if a monitor is genuinely still silent, which is correct.
SELECT COUNT(*) AS n FROM incidents WHERE source = 'cron' AND ended_at = 0;

-- The manual and alert incidents survived.
SELECT source, COUNT(*) AS n FROM incidents GROUP BY source ORDER BY source ASC;

-- The artifact still has everything.
SELECT COUNT(*) AS n FROM incidents_pre035;
```

Then load the dashboard: the chart should show the series, with at most a
handful of narrow markers and a count beside the legend. Watch the log for a
few minutes and confirm `cron incident auto-create failed` and
`cron incident auto-resolve failed` do not appear, and that
`SELECT COUNT(*) FROM incidents` is not climbing.

Once satisfied:

```sql
DROP TABLE incidents_pre035;
```

Do not fold that `DROP` into a later migration without thinking about the
upgrade path — the same mistake 027 originally made.

## Tests

| test | fails without |
| --- | --- |
| `internal/monitoring.TestCheckMissed_HonoursTheSchedulePeriod` | the schedule-aware deadline in `CheckMissed` |
| `internal/monitoring.TestSchedulePeriod`, `TestDueAfterMs` | `SchedulePeriod` / `DueAfterMs` |
| `internal/incidents.TestEnsureOpenReusesTheOpenIncident` | `EnsureOpen` at the auto-declare call sites |
| `internal/incidents.TestInRangeCollapsesVersions` | the database-side collapse in the read paths |
| `ui/src/utils/incidentMarkers.test.ts` | the merge and cap in `prepareMarkers` |

Reverting `CheckMissed` to the grace-only comparison fails with "an hourly cron
was reported missed 1.1s after checking in"; reverting the call site to `Create`
fails with "tick 2 declared a SECOND incident"; removing the cap fails with
"drew 6206 bands".

The UI tests run with `npm test` in `ui/` (`node --test`, no dependencies).
