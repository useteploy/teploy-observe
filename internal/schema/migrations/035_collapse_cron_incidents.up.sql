-- 035 (2026-08-25): collapse the accumulated duplicate cron incidents.
--
-- On the live instance `incidents` held 12,398 rows: 6,192 closed incidents at
-- two rows each (Create writes one, Close appends a second) plus 14 open ones,
-- all severity 'warning', all source 'cron', from only TEN distinct monitors.
-- Every analytics chart draws one shaded band per incident, so the chart was a
-- solid block of translucent orange.
--
-- They accumulated because the missed-run detector measured a monitor against
-- its GRACE PERIOD alone and never looked at its schedule. A cron that
-- legitimately runs hourly with a five-minute grace is therefore "missed" for
-- fifty-five minutes out of every hour: an incident opens, the next hourly ping
-- closes it, and the cycle repeats — one incident per cron run, forever. Ten
-- monitors get you to twelve thousand rows in a few weeks.
-- internal/monitoring.CheckMissed now measures against the schedule's period
-- plus the grace, so new ones stop the moment the binary is deployed. This
-- migration repairs what is already on disk.
--
-- WHAT IT KEEPS
--
--   * Every non-cron incident (manual notes, alert-fired ones): the
--     highest-updated_at version of each, unchanged. incidents has no version
--     column, so updated_at is the version — Create sets it to the insert time
--     and Close to the close time, both monotonic per incident.
--   * Cron incidents: exactly ONE row per (site_id, rule_id) — one per monitor
--     — carrying the most recent version's title, severity and start, and
--     CLOSED at that same timestamp.
--
-- Closing them is deliberate. A cron incident left open renders as a band that
-- runs to the right-hand edge of every chart forever, and a months-old one
-- saying so is not information. If a monitor is genuinely still silent, the
-- detector opens a fresh incident on its next tick (45s after start) and that
-- one is true.
--
-- Non-destructive, in the style of 027/028/033/034: rename aside, recreate,
-- copy across. `incidents_pre035` is LEFT IN PLACE as a recovery artifact — the
-- rename does not copy data, so it costs no extra disk; drop it by hand once
-- the copy is confirmed. A fresh install ends up with one empty artifact, which
-- is harmless.
--
-- No IF EXISTS on the rename: Nucleus resolves the table before it consults
-- that flag, so ALTER TABLE IF EXISTS errors anyway. `incidents` is guaranteed
-- to exist from 013.
--
-- MEMORY AND RUNTIME
--
-- Unlike 034 this one is small and does NOT need the accessory's budget raised.
-- The collapse materialises every row it groups over, and Nucleus caps a single
-- query's working set at 75% of server.max_memory_mb (6144 MB on the live
-- accessory, from NUCLEUS_MAX_MEMORY_MB=8192). Measured on a scratch Nucleus
-- against 12,404 rows seeded to the live shape (6,192 closed cron incidents,
-- 14 open, 6 manual), and 12,404 rows is the live table's exact size:
--
--   query budget | result
--   -------------|-----------------------------------------------
--   48 MB        | completes, whole migration in 3.4 s wall
--   4608 MB      | completes, whole migration in 2.7 s wall
--
-- 48 MB is `max_memory_mb = 64, query_memory_percent = 75`, which is 128 times
-- SMALLER than the live accessory's budget. Nothing here needs the budget
-- raised and there is no chunking to think about. Both figures are on a debug
-- build, so a release build is faster.
--
-- It does NOT roll cleanly back — DDL is not transactional in Nucleus, so
-- `BEGIN; ALTER TABLE t RENAME TO t2; ROLLBACK;` leaves the table renamed. If
-- the copy fails, `incidents` is the new EMPTY table and every row is in
-- `incidents_pre035`. No data is lost, but the markers read empty until you
-- recover: DROP the empty `incidents` and rename `incidents_pre035` back.
--
-- Do not put an `IN (subquery)` in this or any migration — it panics the
-- Nucleus connection (src/executor/session.rs:106). See
-- docs/operations/issue-duplicate-collapse.md.

ALTER TABLE incidents RENAME TO incidents_pre035;

-- incidents exactly as 013 declares it.
CREATE TABLE IF NOT EXISTS incidents (
    incident_id  TEXT NOT NULL,
    tenant_id    TEXT NOT NULL DEFAULT 'default',
    site_id      TEXT NOT NULL,
    title        TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    severity     TEXT NOT NULL DEFAULT 'info',
    source       TEXT NOT NULL DEFAULT 'manual',
    rule_id      TEXT NOT NULL DEFAULT '',
    started_at   BIGINT NOT NULL,
    ended_at     BIGINT NOT NULL DEFAULT 0,
    created_by   TEXT NOT NULL DEFAULT '',
    updated_at   BIGINT NOT NULL
) WITH (engine = 'mergetree')
ORDER BY (incident_id);

-- Non-cron incidents: one row per incident_id, the highest-updated_at version.
-- argMax grouped by the key is the only form that collapses here: `FINAL`
-- parses but is silently ignored, and the engine's own merge does not honour a
-- version column.
INSERT INTO incidents (
    incident_id, tenant_id, site_id, title, description, severity,
    source, rule_id, started_at, ended_at, created_by, updated_at
)
SELECT
    incident_id,
    'default',
    argMax(site_id, updated_at),
    argMax(title, updated_at),
    argMax(description, updated_at),
    argMax(severity, updated_at),
    argMax(source, updated_at),
    argMax(rule_id, updated_at),
    argMax(started_at, updated_at),
    argMax(ended_at, updated_at),
    argMax(created_by, updated_at),
    MAX(updated_at)
FROM incidents_pre035
WHERE source <> 'cron'
GROUP BY incident_id;

-- Cron incidents: one row per monitor, closed. Grouping by (site_id, rule_id)
-- rather than incident_id is what does the collapsing — rule_id is
-- 'cron:<cron_id>', so one group per monitor. ended_at and updated_at are both
-- MAX(updated_at): the last moment that monitor's incidents were touched, which
-- for a closed incident is its real close time and for a still-open one is when
-- it opened.
INSERT INTO incidents (
    incident_id, tenant_id, site_id, title, description, severity,
    source, rule_id, started_at, ended_at, created_by, updated_at
)
SELECT
    argMax(incident_id, updated_at),
    'default',
    site_id,
    argMax(title, updated_at),
    argMax(description, updated_at),
    argMax(severity, updated_at),
    'cron',
    rule_id,
    argMax(started_at, updated_at),
    MAX(updated_at),
    argMax(created_by, updated_at),
    MAX(updated_at)
FROM incidents_pre035
WHERE source = 'cron'
GROUP BY site_id, rule_id;
