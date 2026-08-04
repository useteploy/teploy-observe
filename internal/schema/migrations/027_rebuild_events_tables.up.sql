-- Nucleus 0.1.0 MergeTree bug: tables that have had ALTER TABLE ADD COLUMN
-- applied after creation silently drop inserts from subsequent connections.
-- Workaround: rebuild events and events_recent as plain OLTP tables (no engine
-- clause). Rollup tables (stats_hourly, stats_daily, sessions) were not
-- affected as they receive fresh data from the events table anyway.
--
-- This migration originally DROPped both tables outright, on the reasoning
-- that they were "effectively empty due to the bug". That was true of the one
-- install it was written for, but not in general: any install still behind
-- this version — an older deployment being upgraded, or a restored snapshot —
-- may hold real rows, and a DROP destroys them irrecoverably. So instead:
-- rename the old table aside, create the fresh OLTP table, and copy across
-- whatever rows the old one still yields. The renamed table is deliberately
-- LEFT IN PLACE as a recovery artifact — drop events_pre027 and
-- events_recent_pre027 by hand once you have confirmed the copy.

-- No IF EXISTS: Nucleus resolves the table before consulting that flag, and
-- both tables are guaranteed to exist here (created by 001).
ALTER TABLE events RENAME TO events_pre027;
ALTER TABLE events_recent RENAME TO events_recent_pre027;

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

-- Carry across whatever the old tables still return. Columns are listed
-- explicitly rather than SELECT * because the pre-027 tables reached this
-- point via ALTER TABLE ADD COLUMN (018, 019) and so may present a different
-- column order.
INSERT INTO events (
    event_id, tenant_id, site_id, session_id, visit_id, event_type, timestamp,
    url, referrer, title, hostname, pathname, language, country, region, city,
    browser, browser_version, os, os_version, device, screen_width,
    screen_height, utm_source, utm_medium, utm_campaign, utm_term, utm_content,
    properties, distinct_id, release_tag
)
SELECT
    event_id, tenant_id, site_id, session_id, visit_id, event_type, timestamp,
    url, referrer, title, hostname, pathname, language, country, region, city,
    browser, browser_version, os, os_version, device, screen_width,
    screen_height, utm_source, utm_medium, utm_campaign, utm_term, utm_content,
    properties, distinct_id, release_tag
FROM events_pre027;

-- events_recent gains distinct_id and release_tag here for the first time:
-- 018 and 019 added them to `events` but never to `events_recent`, so the old
-- table has no such columns to copy from. Both are NOT NULL DEFAULT '' and
-- carry no history worth reconstructing, so the copy supplies '' literals.
INSERT INTO events_recent (
    event_id, tenant_id, site_id, session_id, event_type, timestamp, pathname,
    referrer, browser, os, country, properties, distinct_id, release_tag
)
SELECT
    event_id, tenant_id, site_id, session_id, event_type, timestamp, pathname,
    referrer, browser, os, country, properties, '', ''
FROM events_recent_pre027;

-- Clean up shell test data
DELETE FROM sites WHERE site_id = 'test-site-999';
DROP TABLE IF EXISTS _test_regular;
