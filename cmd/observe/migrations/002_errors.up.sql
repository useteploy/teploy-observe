-- Observe v0.2 Error Tracking Schema
-- MergeTree tables for error events and issues.
-- NOTE: All numeric columns use TEXT to avoid Nucleus pgwire binary-format
-- scan issues with BIGINT on MergeTree tables. Values are stored and
-- compared as text-encoded integers.

-- ============================================================================
-- Raw error events
-- ============================================================================

CREATE TABLE IF NOT EXISTS error_events (
    error_id       TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    session_id     TEXT NOT NULL DEFAULT '',
    issue_id       TEXT NOT NULL DEFAULT '',
    group_hash     TEXT NOT NULL,
    timestamp      BIGINT NOT NULL,
    error_type     TEXT NOT NULL DEFAULT '',
    error_value    TEXT NOT NULL DEFAULT '',
    mechanism      TEXT NOT NULL DEFAULT '',
    handled        TEXT NOT NULL DEFAULT 'true',
    level          TEXT NOT NULL DEFAULT 'error',
    release_tag    TEXT NOT NULL DEFAULT '',
    environment    TEXT NOT NULL DEFAULT '',
    url            TEXT NOT NULL DEFAULT '',
    browser        TEXT NOT NULL DEFAULT '',
    os             TEXT NOT NULL DEFAULT '',
    device         TEXT NOT NULL DEFAULT '',
    stack_trace    JSONB,
    breadcrumbs    JSONB,
    contexts       JSONB,
    extra          JSONB
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, timestamp, group_hash);

-- ============================================================================
-- Issues (grouped errors)
-- ============================================================================

CREATE TABLE IF NOT EXISTS issues (
    issue_id       TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    group_hash     TEXT NOT NULL,
    title          TEXT NOT NULL DEFAULT '',
    culprit        TEXT NOT NULL DEFAULT '',
    level          TEXT NOT NULL DEFAULT 'error',
    status         TEXT NOT NULL DEFAULT 'open',
    first_seen     TEXT NOT NULL,
    last_seen      TEXT NOT NULL,
    event_count    TEXT NOT NULL DEFAULT '1',
    user_count     TEXT NOT NULL DEFAULT '0',
    release_tag    TEXT NOT NULL DEFAULT '',
    version        BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, issue_id);
