-- 058: error-tracking depth - issue merge/unmerge and assignment.
--
-- Two NEW tables only (CREATE TABLE IF NOT EXISTS); no existing table is
-- altered. Both are ReplacingMergeTree keyed on the logical row and are
-- collapsed at read time with argMax over version, the 034/047 pattern.
-- Nothing is ever deleted: an unmerge writes a newer version with
-- active = 'false', clearing an assignee writes assignee = ''.
--
--   issue_merges      one logical row per SOURCE issue. active = 'true'
--                     means events and reads for source_issue_id resolve
--                     to target_issue_id. New events whose fingerprint
--                     maps to a merged source are attributed to the
--                     target at ingest. Chains are followed to a bounded
--                     depth; cycles are rejected by the service.
--   issue_assignments one logical row per issue; assignee '' = unassigned.
--
-- version is a strictly increasing unix-ms stamp chosen by the writer
-- (GREATEST(now, previous + 1)). merged_at / assigned_at are unix ms.
--
-- Comments pure ASCII (038 rule).

CREATE TABLE IF NOT EXISTS issue_merges (
    tenant_id       TEXT NOT NULL DEFAULT 'default',
    site_id         TEXT NOT NULL,
    source_issue_id TEXT NOT NULL,
    target_issue_id TEXT NOT NULL,
    active          TEXT NOT NULL DEFAULT 'true',
    merged_at       BIGINT NOT NULL DEFAULT 0,
    merged_by       TEXT NOT NULL DEFAULT '',
    version         BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, source_issue_id);

CREATE TABLE IF NOT EXISTS issue_assignments (
    tenant_id   TEXT NOT NULL DEFAULT 'default',
    site_id     TEXT NOT NULL,
    issue_id    TEXT NOT NULL,
    assignee    TEXT NOT NULL DEFAULT '',
    assigned_at BIGINT NOT NULL DEFAULT 0,
    assigned_by TEXT NOT NULL DEFAULT '',
    version     BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, issue_id);
