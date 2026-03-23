-- Observe v0.6 Feature Expansion
-- Goals, logs, uptime monitors, cron monitors, custom dashboards, session replays.

-- ============================================================================
-- Goals (conversion tracking)
-- ============================================================================

CREATE TABLE IF NOT EXISTS goals (
    goal_id        TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    goal_type      TEXT NOT NULL DEFAULT 'page',
    goal_value     TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    version        TEXT NOT NULL DEFAULT '0'
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, goal_id);

-- ============================================================================
-- Logs (observability third pillar)
-- ============================================================================

CREATE TABLE IF NOT EXISTS logs (
    log_id         TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    timestamp      BIGINT NOT NULL,
    level          TEXT NOT NULL DEFAULT 'info',
    message        TEXT NOT NULL DEFAULT '',
    service_name   TEXT NOT NULL DEFAULT '',
    trace_id       TEXT NOT NULL DEFAULT '',
    span_id        TEXT NOT NULL DEFAULT '',
    attributes     JSONB
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, timestamp);

-- ============================================================================
-- Uptime monitors
-- ============================================================================

CREATE TABLE IF NOT EXISTS uptime_monitors (
    monitor_id     TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    url            TEXT NOT NULL DEFAULT '',
    method         TEXT NOT NULL DEFAULT 'GET',
    interval_secs  TEXT NOT NULL DEFAULT '60',
    expected_status TEXT NOT NULL DEFAULT '200',
    enabled        TEXT NOT NULL DEFAULT 'true',
    created_at     TEXT NOT NULL,
    version        TEXT NOT NULL DEFAULT '0'
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, monitor_id);

CREATE TABLE IF NOT EXISTS uptime_results (
    result_id      TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    monitor_id     TEXT NOT NULL,
    site_id        TEXT NOT NULL,
    timestamp      BIGINT NOT NULL,
    status_code    TEXT NOT NULL DEFAULT '0',
    response_ms    TEXT NOT NULL DEFAULT '0',
    is_up          TEXT NOT NULL DEFAULT 'true',
    error_message  TEXT NOT NULL DEFAULT ''
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, monitor_id, timestamp);

-- ============================================================================
-- Cron monitors (heartbeat checks)
-- ============================================================================

CREATE TABLE IF NOT EXISTS cron_monitors (
    cron_id        TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    slug           TEXT NOT NULL,
    schedule       TEXT NOT NULL DEFAULT '',
    grace_period   TEXT NOT NULL DEFAULT '300',
    enabled        TEXT NOT NULL DEFAULT 'true',
    created_at     TEXT NOT NULL,
    version        TEXT NOT NULL DEFAULT '0'
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, cron_id);

CREATE TABLE IF NOT EXISTS cron_checkins (
    checkin_id     TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    cron_id        TEXT NOT NULL,
    site_id        TEXT NOT NULL,
    timestamp      BIGINT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'ok',
    duration_ms    TEXT NOT NULL DEFAULT '0'
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, cron_id, timestamp);

-- ============================================================================
-- Custom dashboards
-- ============================================================================

CREATE TABLE IF NOT EXISTS dashboards (
    dashboard_id   TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    description    TEXT NOT NULL DEFAULT '',
    created_by     TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    version        TEXT NOT NULL DEFAULT '0'
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, dashboard_id);

CREATE TABLE IF NOT EXISTS dashboard_panels (
    panel_id       TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    dashboard_id   TEXT NOT NULL,
    panel_type     TEXT NOT NULL DEFAULT 'metric',
    title          TEXT NOT NULL DEFAULT '',
    query_type     TEXT NOT NULL DEFAULT '',
    query_config   JSONB,
    position_x     TEXT NOT NULL DEFAULT '0',
    position_y     TEXT NOT NULL DEFAULT '0',
    width          TEXT NOT NULL DEFAULT '6',
    height         TEXT NOT NULL DEFAULT '4',
    version        TEXT NOT NULL DEFAULT '0'
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, dashboard_id, panel_id);

-- ============================================================================
-- Session replays
-- ============================================================================

CREATE TABLE IF NOT EXISTS replay_sessions (
    replay_id      TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    session_id     TEXT NOT NULL DEFAULT '',
    start_time     BIGINT NOT NULL,
    duration_ms    TEXT NOT NULL DEFAULT '0',
    page_count     TEXT NOT NULL DEFAULT '0',
    url            TEXT NOT NULL DEFAULT '',
    browser        TEXT NOT NULL DEFAULT '',
    os             TEXT NOT NULL DEFAULT '',
    device         TEXT NOT NULL DEFAULT '',
    has_error      TEXT NOT NULL DEFAULT 'false'
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, start_time);

CREATE TABLE IF NOT EXISTS replay_events (
    event_id       TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    replay_id      TEXT NOT NULL,
    timestamp      BIGINT NOT NULL,
    event_type     TEXT NOT NULL DEFAULT '',
    data           JSONB
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, replay_id, timestamp);

-- ============================================================================
-- Tracked links and pixels
-- ============================================================================

CREATE TABLE IF NOT EXISTS tracked_links (
    link_id        TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    destination    TEXT NOT NULL DEFAULT '',
    slug           TEXT NOT NULL,
    click_count    TEXT NOT NULL DEFAULT '0',
    created_at     TEXT NOT NULL,
    version        TEXT NOT NULL DEFAULT '0'
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, link_id);

CREATE TABLE IF NOT EXISTS link_clicks (
    click_id       TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    link_id        TEXT NOT NULL,
    timestamp      BIGINT NOT NULL,
    referrer       TEXT NOT NULL DEFAULT '',
    country        TEXT NOT NULL DEFAULT '',
    browser        TEXT NOT NULL DEFAULT '',
    device         TEXT NOT NULL DEFAULT ''
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, link_id, timestamp);
