-- 053 (2026-09-24): O14 - scheduled SQL export runs: durable job history +
-- outbox-pattern retry.
--
-- Programme O14: "exports need scoped destinations, job history/retry and
-- restoration-friendly format". scheduled_exports carried only the LAST
-- run's status in its own columns - no history to inspect after a restart,
-- and a failed run was never retried (the next cron tick started fresh,
-- so a transient S3 outage silently skipped that interval's export).
--
-- scheduled_export_runs is the run ledger, shaped on the 046/050 outbox
-- pattern: a run row is an INTENT whose inputs (sql, format, destination
-- config) are FROZEN at enqueue time, so a retry reproduces the identical
-- upload and an edit to the export definition mid-retry cannot half-apply.
--
--   run_id    the durable run identity AND the S3 key component: every
--             attempt of one run writes the SAME object key, so an
--             at-least-once retry overwrites its own partial object
--             instead of leaving one object per attempt.
--   trigger   'cron' (scheduler-enqueued) | 'manual' (Run now)
--
-- Row lifecycle (mirroring derived_outbox / notification_outbox):
--   enqueue          attempts=0, finished_at=0, next_attempt_at=0
--   worker succeeds  new version with finished_at + rows/bytes set
--   worker fails     attempts+1, last_error, next_attempt_at = now +
--                    exponential backoff (doubling, capped)
--   attempt budget   DEAD LETTER: next_attempt_at = -1 (a sentinel no
--                    clock reaches), last_error kept - inspectable in the
--                    run history, never auto-pruned, never retried even by
--                    a later process with a larger budget
--
-- A completed run also mirrors last_* onto scheduled_exports (the legacy
-- summary columns existing readers use), but the RUNS table is the
-- history: one logical row per run forever, so job history survives
-- restarts by construction.
--
-- Restoration friendliness: every successful run uploads a .manifest.json
-- sidecar next to the data object (columns, row count, byte count, sha256
-- of the body, run/export identity, format) so an archive can be verified
-- and restored without parsing the data itself. The data format is
-- unchanged (pure CSV / NDJSON).
--
-- Scoped destinations are enforced at CREATE time in the service (region +
-- bucket required; an explicit endpoint must be a clean http(s) URL with a
-- host and no userinfo) - no schema for that, recorded here because the
-- spec names it.
--
-- ReplacingMergeTree keyed on the run id, collapsed at read time by the
-- argMax form (internal/query/replacing.go). Single-process boundary (the
-- standing AUD-018 posture): the drain serializes in-process.
--
-- Comments pure ASCII (038 rule).

CREATE TABLE IF NOT EXISTS scheduled_export_runs (
    run_id           TEXT NOT NULL,
    tenant_id        TEXT NOT NULL DEFAULT 'default',
    export_id        TEXT NOT NULL,
    name             TEXT NOT NULL DEFAULT '',
    sql              TEXT NOT NULL,
    format           TEXT NOT NULL DEFAULT 'ndjson',
    destination_type TEXT NOT NULL DEFAULT 's3',
    destination_cfg  TEXT NOT NULL,
    run_trigger      TEXT NOT NULL DEFAULT 'cron',
    created_at       BIGINT NOT NULL,
    attempts         BIGINT NOT NULL DEFAULT 0,
    next_attempt_at  BIGINT NOT NULL DEFAULT 0,
    finished_at      BIGINT NOT NULL DEFAULT 0,
    rows             BIGINT NOT NULL DEFAULT 0,
    bytes            BIGINT NOT NULL DEFAULT 0,
    last_error       TEXT NOT NULL DEFAULT '',
    version          BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, run_id);
