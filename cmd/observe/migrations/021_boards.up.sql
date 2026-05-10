-- C4 (Wave 2, 2026-05-10): multi-site boards. Agency / MSP target — one
-- saved entity that aggregates pageviews / errors / uptime / replays
-- across N sites in a single grid view.
--
-- Distinct from saved_views (per-site, JSON config) because boards are
-- inherently cross-site: they store an array of site_ids in payload and
-- a tenant-level scope (no NOT NULL site_id). Using saved_views with a
-- sentinel site_id would have been a pun.

CREATE TABLE IF NOT EXISTS boards (
    board_id    TEXT NOT NULL,
    tenant_id   TEXT NOT NULL DEFAULT 'default',
    name        TEXT NOT NULL DEFAULT '',
    payload     JSONB,
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    version     TEXT NOT NULL DEFAULT '0'
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, board_id);
