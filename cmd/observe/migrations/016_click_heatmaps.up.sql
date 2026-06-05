-- Click heatmaps: aggregated click coordinates per (site, url, position bucket).
-- Powers the URL-keyed heatmap overlay on session replays (PostHog parity gap #3 / W4.2).
--
-- Click coordinates already flow into replay_events via observe-replay.js
-- (see cmd/observe/tracker/observe-replay.js:79). On every replay-ingest batch
-- we also write per-bucket rollups here so the heatmap query is a single
-- SUM(count) GROUP BY (x,y) instead of re-bucketing every replay event.
--
-- Bucket sizes (kept in code in internal/heatmaps/aggregate.go):
--   x_bucket  = floor(client_x / 10)
--   y_bucket  = floor(client_y / 10)
--   vw_bucket = floor(viewport_width / 100)  (0 = unknown)
--
-- ReplacingMergeTree mirrors service_stats / service_dependencies: each
-- ingest writes one new row per bucket with a fresh `version`; the query
-- SUMs across rows in the window. Per dogfood finding #10 / #11 Nucleus
-- does not collapse rmt rows on read, but the SUM-based query is correct
-- regardless of whether it ever does.
CREATE TABLE IF NOT EXISTS click_heatmaps (
    tenant_id   TEXT NOT NULL DEFAULT 'default',
    site_id     TEXT NOT NULL,
    url         TEXT NOT NULL DEFAULT '',
    x_bucket    BIGINT NOT NULL,
    y_bucket    BIGINT NOT NULL,
    vw_bucket   BIGINT NOT NULL DEFAULT 0,
    count       TEXT NOT NULL DEFAULT '0',
    created_at  BIGINT NOT NULL,
    version     BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, url, x_bucket, y_bucket, vw_bucket, created_at);
