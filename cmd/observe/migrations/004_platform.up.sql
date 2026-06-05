-- Observe v0.4 Platform Features
-- Multi-user, alerting, webhooks.

-- ============================================================================
-- Users with roles (extends admin_users concept)
-- ============================================================================
-- Roles: admin (full access), editor (can manage sites/keys), viewer (read-only)

CREATE TABLE IF NOT EXISTS users (
    user_id        TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    username       TEXT NOT NULL,
    email          TEXT NOT NULL DEFAULT '',
    password_hash  TEXT NOT NULL,
    role           TEXT NOT NULL DEFAULT 'viewer',
    created_at     TEXT NOT NULL,
    invited_by     TEXT NOT NULL DEFAULT ''
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, user_id);

-- ============================================================================
-- Alert rules
-- ============================================================================
-- metric: pageviews, visitors, error_count, p95_latency, etc.
-- operator: gt, lt, gte, lte, eq
-- threshold: numeric value as text
-- check_interval: seconds between checks
-- cooldown: seconds after trigger before re-alerting

CREATE TABLE IF NOT EXISTS alert_rules (
    rule_id        TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    metric         TEXT NOT NULL,
    operator       TEXT NOT NULL DEFAULT 'gt',
    threshold      TEXT NOT NULL DEFAULT '0',
    window_minutes TEXT NOT NULL DEFAULT '5',
    check_interval TEXT NOT NULL DEFAULT '60',
    cooldown       TEXT NOT NULL DEFAULT '300',
    enabled        TEXT NOT NULL DEFAULT 'true',
    created_by     TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    version        BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, rule_id);

-- ============================================================================
-- Alert history (triggered alerts)
-- ============================================================================

CREATE TABLE IF NOT EXISTS alert_history (
    alert_id       TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    rule_id        TEXT NOT NULL,
    site_id        TEXT NOT NULL,
    triggered_at   BIGINT NOT NULL,
    metric_value   TEXT NOT NULL DEFAULT '0',
    threshold      TEXT NOT NULL DEFAULT '0',
    status         TEXT NOT NULL DEFAULT 'triggered'
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, triggered_at);

-- ============================================================================
-- Webhooks (notification targets)
-- ============================================================================
-- type: slack, http, email

CREATE TABLE IF NOT EXISTS webhooks (
    webhook_id     TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    webhook_type   TEXT NOT NULL DEFAULT 'http',
    url            TEXT NOT NULL DEFAULT '',
    secret         TEXT NOT NULL DEFAULT '',
    enabled        TEXT NOT NULL DEFAULT 'true',
    created_at     TEXT NOT NULL,
    version        BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, webhook_id);
