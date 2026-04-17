-- Scheduled exports: admin-defined cron jobs that run a SELECT and upload
-- the result as NDJSON or CSV to S3 / R2 / any S3-compatible target.
CREATE TABLE IF NOT EXISTS scheduled_exports (
    export_id        TEXT NOT NULL,
    tenant_id        TEXT NOT NULL DEFAULT 'default',
    name             TEXT NOT NULL,
    sql              TEXT NOT NULL,
    format           TEXT NOT NULL DEFAULT 'ndjson',
    cron             TEXT NOT NULL,
    destination_type TEXT NOT NULL DEFAULT 's3',
    destination_cfg  TEXT NOT NULL,
    enabled          TEXT NOT NULL DEFAULT 'true',
    last_run_at      BIGINT NOT NULL DEFAULT 0,
    last_status      TEXT NOT NULL DEFAULT '',
    last_error       TEXT NOT NULL DEFAULT '',
    last_rows        BIGINT NOT NULL DEFAULT 0,
    created_at       BIGINT NOT NULL,
    updated_at       BIGINT NOT NULL
) WITH (engine = 'mergetree')
ORDER BY (export_id);
