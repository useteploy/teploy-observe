-- Performance issues: detector-emitted findings (n+1 db, slow query, consecutive
-- db, slow http) grouped by stable fingerprint. Replacing-mergetree ordered by
-- (tenant_id, site_id, fingerprint) collapses re-detections of the same pattern
-- into a single row, with last_seen as the version column so the highest-ts row
-- wins on read.
CREATE TABLE IF NOT EXISTS performance_issues (
    issue_id       TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    trace_id       TEXT NOT NULL,
    detector_name  TEXT NOT NULL,
    fingerprint    TEXT NOT NULL,
    title          TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    severity       TEXT NOT NULL DEFAULT 'warning',
    count          BIGINT NOT NULL DEFAULT 1,
    first_seen     BIGINT NOT NULL,
    last_seen      BIGINT NOT NULL
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'last_seen'
)
ORDER BY (tenant_id, site_id, fingerprint);
