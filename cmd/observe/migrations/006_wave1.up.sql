-- Observe Wave 1 — Integrations, feedback, saved views, report schedules.

-- ============================================================================
-- Integrations (Jira, GitHub, PagerDuty, email)
-- ============================================================================

CREATE TABLE IF NOT EXISTS integrations (
    integration_id TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    int_type       TEXT NOT NULL DEFAULT 'http',
    config         JSONB,
    enabled        TEXT NOT NULL DEFAULT 'true',
    created_at     TEXT NOT NULL,
    version        TEXT NOT NULL DEFAULT '0'
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, integration_id);

-- ============================================================================
-- User feedback
-- ============================================================================

CREATE TABLE IF NOT EXISTS feedback (
    feedback_id    TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    session_id     TEXT NOT NULL DEFAULT '',
    url            TEXT NOT NULL DEFAULT '',
    message        TEXT NOT NULL DEFAULT '',
    email          TEXT NOT NULL DEFAULT '',
    category       TEXT NOT NULL DEFAULT 'general',
    timestamp      BIGINT NOT NULL
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, timestamp);

-- ============================================================================
-- Saved views
-- ============================================================================

CREATE TABLE IF NOT EXISTS saved_views (
    view_id        TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    view_config    JSONB,
    created_by     TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    version        TEXT NOT NULL DEFAULT '0'
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, view_id);

-- ============================================================================
-- Report schedules
-- ============================================================================

CREATE TABLE IF NOT EXISTS report_schedules (
    schedule_id    TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    frequency      TEXT NOT NULL DEFAULT 'weekly',
    recipients     TEXT NOT NULL DEFAULT '',
    enabled        TEXT NOT NULL DEFAULT 'true',
    last_sent      TEXT NOT NULL DEFAULT '0',
    created_at     TEXT NOT NULL,
    version        TEXT NOT NULL DEFAULT '0'
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, schedule_id);
