-- Observe Wave 3 (W3.A) — OTLP Metrics
-- Append-only point storage for gauge / sum (counter) / histogram metrics.
-- Mirrors the OTLP metrics shape so any OTLP HTTP exporter can target /v1/metrics.
--
-- Engine choice: mergetree, NOT replacing_mergetree.
--   * Metric points are append-only — a (site, name, ts_ns, attributes) tuple
--     is never updated after write.
--   * Dogfood findings #8 + #10 make replacing_mergetree fragile under
--     SIGKILL / cross-version reads (#10 returns 17-32 duplicate rows on
--     PK reads). Plain mergetree avoids both classes of bug.
--
-- All numeric columns use the native types Nucleus exposes (BIGINT for
-- timestamps, DOUBLE for values) — neutron-go's scanner can read these
-- directly per the Phase-1 scanner fix; no pgwire-TEXT workaround needed
-- on writes (parameter binding still goes through dbutil.IntParam).

CREATE TABLE IF NOT EXISTS metric_points (
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
    aggregation_temporality TEXT NOT NULL DEFAULT 'cumulative' -- 'cumulative' | 'delta'
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, metric_name, ts_ns);
