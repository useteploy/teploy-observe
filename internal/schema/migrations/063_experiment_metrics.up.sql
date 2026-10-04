-- 063 (2026-10-03): experiment depth - continuous metrics, secondary goals,
-- analysis settings.
--
-- The experiments table cannot take new columns (ALTER ADD COLUMN corrupts
-- populated tables, open P1 L9), and experiment_conversions carries no value,
-- so both new concerns get their own append-only tables:
--
--   experiment_settings       one JSON document per write (metric kind,
--                             winsorize percentile, planned sample size,
--                             early-winner override, up to 3 secondary
--                             goals). The newest row by timestamp wins; an
--                             experiment with no row behaves as before (one
--                             binary conversion goal).
--   experiment_metric_events  one observation per row for count / mean goals
--                             and secondary goals. metric is 'primary' or a
--                             secondary goal key. value is stored as TEXT
--                             (parsed to a finite float in Go, the same
--                             convention as llm cost_usd) so no DOUBLE wire
--                             path is involved.
--
-- Both are plain mergetree: the settings read orders by timestamp DESC, and
-- metric events are never updated. Comments pure ASCII (038 rule).

CREATE TABLE IF NOT EXISTS experiment_settings (
    setting_id     TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    experiment_id  TEXT NOT NULL,
    site_id        TEXT NOT NULL,
    config         TEXT NOT NULL DEFAULT '{}',
    timestamp      BIGINT NOT NULL
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, experiment_id, timestamp);

CREATE TABLE IF NOT EXISTS experiment_metric_events (
    event_id       TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    experiment_id  TEXT NOT NULL,
    site_id        TEXT NOT NULL,
    user_id        TEXT NOT NULL DEFAULT '',
    metric         TEXT NOT NULL DEFAULT 'primary',
    value          TEXT NOT NULL DEFAULT '0',
    timestamp      BIGINT NOT NULL
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, experiment_id, metric, timestamp);
