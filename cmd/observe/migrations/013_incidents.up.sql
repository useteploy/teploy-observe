-- Incidents: manual or alert-fired markers that render as a vertical band
-- across every time-series chart in the UI. Lets users spot which metrics
-- moved during a known problem window.
CREATE TABLE IF NOT EXISTS incidents (
    incident_id  TEXT NOT NULL,
    tenant_id    TEXT NOT NULL DEFAULT 'default',
    site_id      TEXT NOT NULL,
    title        TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    severity     TEXT NOT NULL DEFAULT 'info',
    source       TEXT NOT NULL DEFAULT 'manual',
    rule_id      TEXT NOT NULL DEFAULT '',
    started_at   BIGINT NOT NULL,
    ended_at     BIGINT NOT NULL DEFAULT 0,
    created_by   TEXT NOT NULL DEFAULT '',
    updated_at   BIGINT NOT NULL
) WITH (engine = 'mergetree')
ORDER BY (incident_id);
