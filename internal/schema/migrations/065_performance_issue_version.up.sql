-- 065: Separate snapshot replacement version from detector event time.
-- Append-only migration: applied history remains checksum-frozen.
-- Rename/rebuild avoids populated-table column layout changes.
-- performance_issues_pre065 is retained as the complete recovery source.
--
-- Nucleus DDL is nontransactional. This migration intentionally fails closed
-- on retry after RENAME (the aside name already exists); never replace that
-- source or silently copy twice. Stop all writers before migration/recovery.
-- If this migration is NOT recorded as applied, verify the aside contains the
-- original rows, drop only the newly created destination if present, rename
-- the aside back to performance_issues, then rerun the unchanged migration.
-- If history is recorded, do not restore the old schema: inspect/repair the
-- destination separately. Do not drop the recovery artifact automatically.

ALTER TABLE performance_issues RENAME TO performance_issues_pre065;

CREATE TABLE performance_issues (
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
    last_seen      BIGINT NOT NULL,
    version        BIGINT NOT NULL
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, fingerprint);

INSERT INTO performance_issues (issue_id, tenant_id, site_id, trace_id, detector_name, fingerprint, title, description, severity, count, first_seen, last_seen, version)
SELECT issue_id, tenant_id, site_id, trace_id, detector_name, fingerprint, title, description, severity, count, first_seen, last_seen, last_seen
FROM performance_issues_pre065;
