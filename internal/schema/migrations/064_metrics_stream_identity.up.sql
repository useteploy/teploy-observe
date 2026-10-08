-- 064: Persist producer metric stream identity; historical identity is unrecoverable.
-- Append-only migration: applied history remains checksum-frozen.
-- Rename/rebuild avoids populated-table column layout changes.
-- metric_points_pre064 is retained as the complete recovery source.
--
-- Nucleus DDL is nontransactional. This migration intentionally fails closed
-- on retry after RENAME (the aside name already exists); never replace that
-- source or silently copy twice. Stop all writers before migration/recovery.
-- If this migration is NOT recorded as applied, verify the aside contains the
-- original rows, drop only the newly created destination if present, rename
-- the aside back to metric_points, then rerun the unchanged migration.
-- If history is recorded, do not restore the old schema: inspect/repair the
-- destination separately. Do not drop the recovery artifact automatically.

ALTER TABLE metric_points RENAME TO metric_points_pre064;

CREATE TABLE metric_points (
    site_id                 TEXT NOT NULL,
    tenant_id               TEXT NOT NULL DEFAULT 'default',
    metric_name             TEXT NOT NULL,
    metric_kind             TEXT NOT NULL,                  -- 'gauge' | 'sum' | 'histogram'
    service_name            TEXT NOT NULL DEFAULT '',
    attributes              TEXT NOT NULL DEFAULT '{}',     -- JSON of label key/value pairs
    ts_ns                   BIGINT NOT NULL,                -- nanosecond timestamp
    value                   DOUBLE NOT NULL DEFAULT 0,      -- gauge / sum value
    histogram               TEXT NOT NULL DEFAULT '',       -- JSON {bounds:[],counts:[],sum,count}
    is_monotonic            TEXT NOT NULL DEFAULT 'false',  -- sum-only flag
    aggregation_temporality TEXT NOT NULL DEFAULT 'cumulative', -- 'cumulative' | 'delta'
    stream_identity         TEXT NOT NULL DEFAULT ''
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, metric_name, ts_ns);

INSERT INTO metric_points (site_id, tenant_id, metric_name, metric_kind, service_name, attributes, ts_ns, value, histogram, is_monotonic, aggregation_temporality, stream_identity)
SELECT site_id, tenant_id, metric_name, metric_kind, service_name, attributes, ts_ns, value, histogram, is_monotonic, aggregation_temporality, ''
FROM metric_points_pre064;
