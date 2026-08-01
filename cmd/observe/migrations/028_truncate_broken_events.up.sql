-- Clear rows inserted before the propertiesJSON("") -> "{}" fix (2026-06-26).
-- Those rows have empty-string JSONB for the properties column, which causes
-- Nucleus to make them invisible to WHERE-clause queries. They show up in
-- COUNT(*) but stats queries (which all use WHERE site_id = ?) can't see them.
-- DELETE could not reach them either, which is why this originally used a
-- blanket TRUNCATE.
--
-- TRUNCATE is wrong for anyone still behind this version: it destroys every
-- GOOD row alongside the broken ones, and there is no recovering them. Rebuild
-- instead, and exploit the very property that made these rows a problem —
-- a WHERE clause cannot see them. Copying `WHERE site_id <> ''` therefore
-- carries across exactly the rows that are actually queryable and silently
-- leaves the broken ones behind.
--
-- If a future Nucleus makes those rows visible to WHERE again, this copies
-- them too and they stay as they were: no cleanup, but no data loss either —
-- the safe direction to fail in.
--
-- The renamed tables are deliberately LEFT IN PLACE as recovery artifacts.
-- Drop events_pre028 / events_recent_pre028 by hand once you have confirmed
-- the copy.

ALTER TABLE events RENAME TO events_pre028;
ALTER TABLE events_recent RENAME TO events_recent_pre028;

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
FROM events_pre028
WHERE site_id <> '';

INSERT INTO events_recent (
    event_id, tenant_id, site_id, session_id, event_type, timestamp, pathname,
    referrer, browser, os, country, properties, distinct_id, release_tag
)
SELECT
    event_id, tenant_id, site_id, session_id, event_type, timestamp, pathname,
    referrer, browser, os, country, properties, distinct_id, release_tag
FROM events_recent_pre028
WHERE site_id <> '';
