-- Nucleus 0.1.0 MergeTree bug: tables that have had ALTER TABLE ADD COLUMN
-- applied after creation silently drop inserts from subsequent connections.
-- Workaround: rebuild events and events_recent as plain OLTP tables (no engine
-- clause). Data loss is acceptable — these tables were effectively empty due
-- to the bug. Rollup tables (stats_hourly, stats_daily, sessions) were not
-- affected as they receive fresh data from the events table anyway.

DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS events_recent;

CREATE TABLE IF NOT EXISTS events (
    event_id        TEXT NOT NULL,
    tenant_id       TEXT NOT NULL DEFAULT 'default',
    site_id         TEXT NOT NULL,
    session_id      TEXT NOT NULL,
    visit_id        TEXT NOT NULL,
    event_type      TEXT NOT NULL DEFAULT 'pageview',
    timestamp       BIGINT NOT NULL,
    url             TEXT,
    referrer        TEXT,
    title           TEXT,
    hostname        TEXT,
    pathname        TEXT,
    language        TEXT,
    country         TEXT,
    region          TEXT,
    city            TEXT,
    browser         TEXT,
    browser_version TEXT,
    os              TEXT,
    os_version      TEXT,
    device          TEXT,
    screen_width    INTEGER,
    screen_height   INTEGER,
    utm_source      TEXT,
    utm_medium      TEXT,
    utm_campaign    TEXT,
    utm_term        TEXT,
    utm_content     TEXT,
    properties      JSONB,
    distinct_id     TEXT NOT NULL DEFAULT '',
    release_tag     TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS events_recent (
    event_id        TEXT NOT NULL,
    tenant_id       TEXT NOT NULL DEFAULT 'default',
    site_id         TEXT NOT NULL,
    session_id      TEXT NOT NULL,
    event_type      TEXT NOT NULL DEFAULT 'pageview',
    timestamp       BIGINT NOT NULL,
    pathname        TEXT,
    referrer        TEXT,
    browser         TEXT,
    os              TEXT,
    country         TEXT,
    properties      JSONB,
    distinct_id     TEXT NOT NULL DEFAULT '',
    release_tag     TEXT NOT NULL DEFAULT ''
);

-- Clean up shell test data
DELETE FROM sites WHERE site_id = 'test-site-999';
DROP TABLE IF EXISTS _test_regular;
