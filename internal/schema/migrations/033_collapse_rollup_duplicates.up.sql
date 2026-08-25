-- 033 (2026-08-25): collapse the accumulated duplicate rows in the analytics
-- rollups.
--
-- stats_hourly, stats_daily and sessions are ReplacingMergeTree tables whose
-- writers deliberately recompute an overlapping window on every tick. That was
-- only ever safe if the engine collapsed the repeated writes down to the
-- highest version, and Nucleus does not do so reliably: it collapses within a
-- memtable but leaves rows written into separate segments in place, and it has
-- no OPTIMIZE / merge-now command. On the live instance stats_hourly held 740
-- duplicated bucket keys out of 1956 rows, the oldest two months old, and every
-- dashboard read summed them — a window whose raw events prove 72 pageviews was
-- reported as 158.
--
-- The read path now collapses by version at query time
-- (internal/query/replacing.go) and the rollup jobs clear each window before
-- rewriting it (internal/jobs/rollups.go), so new data is correct either way.
-- This migration repairs the rows already on disk, so the tables stop carrying
-- several copies of every bucket forever — stats_daily in particular has no
-- retention policy, so nothing would ever have removed them.
--
-- Non-destructive, in the style of 027/028: rename aside, recreate, copy the
-- highest-version row per ORDER BY key across. The renamed tables are LEFT IN
-- PLACE as recovery artifacts. Drop stats_hourly_pre033, stats_daily_pre033
-- and sessions_pre033 by hand once you have confirmed the copy; a fresh install
-- simply ends up with three empty artifacts, which is harmless.
--
-- No IF EXISTS on the renames: Nucleus resolves the table before it consults
-- that flag, so ALTER TABLE IF EXISTS errors anyway. All three are guaranteed
-- to exist from 001.

ALTER TABLE stats_hourly RENAME TO stats_hourly_pre033;
ALTER TABLE stats_daily RENAME TO stats_daily_pre033;
ALTER TABLE sessions RENAME TO sessions_pre033;

CREATE TABLE IF NOT EXISTS stats_hourly (
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    ts_bucket      BIGINT NOT NULL,
    pathname       TEXT NOT NULL DEFAULT '',
    event_type     TEXT NOT NULL DEFAULT 'pageview',
    pageviews      BIGINT NOT NULL DEFAULT 0,
    visitors       BIGINT NOT NULL DEFAULT 0,
    sessions       BIGINT NOT NULL DEFAULT 0,
    bounces        BIGINT NOT NULL DEFAULT 0,
    total_duration BIGINT NOT NULL DEFAULT 0,
    version        BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, ts_bucket, pathname, event_type);

CREATE TABLE IF NOT EXISTS stats_daily (
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    ts_bucket      BIGINT NOT NULL,
    pathname       TEXT NOT NULL DEFAULT '',
    event_type     TEXT NOT NULL DEFAULT 'pageview',
    referrer       TEXT NOT NULL DEFAULT '',
    browser        TEXT NOT NULL DEFAULT '',
    os             TEXT NOT NULL DEFAULT '',
    country        TEXT NOT NULL DEFAULT '',
    device         TEXT NOT NULL DEFAULT '',
    utm_source     TEXT NOT NULL DEFAULT '',
    utm_medium     TEXT NOT NULL DEFAULT '',
    utm_campaign   TEXT NOT NULL DEFAULT '',
    pageviews      BIGINT NOT NULL DEFAULT 0,
    visitors       BIGINT NOT NULL DEFAULT 0,
    sessions       BIGINT NOT NULL DEFAULT 0,
    bounces        BIGINT NOT NULL DEFAULT 0,
    total_duration BIGINT NOT NULL DEFAULT 0,
    version        BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, ts_bucket, pathname, event_type, referrer, browser, os, country, device, utm_source, utm_medium, utm_campaign);

-- sessions carries release_tag, added by 019 after 001 created the table.
CREATE TABLE IF NOT EXISTS sessions (
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    session_id     TEXT NOT NULL,
    first_ts       BIGINT NOT NULL,
    last_ts        BIGINT NOT NULL,
    pageviews      BIGINT NOT NULL DEFAULT 1,
    events_count   BIGINT NOT NULL DEFAULT 1,
    entry_url      TEXT,
    exit_url       TEXT,
    referrer       TEXT,
    browser        TEXT,
    os             TEXT,
    device         TEXT,
    country        TEXT,
    language       TEXT,
    screen_width   INTEGER,
    screen_height  INTEGER,
    utm_source     TEXT,
    utm_medium     TEXT,
    utm_campaign   TEXT,
    is_bounce      BOOLEAN DEFAULT true,
    version        BIGINT NOT NULL DEFAULT 0,
    release_tag    TEXT NOT NULL DEFAULT ''
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, session_id);

-- The copies. argMax(col, version) grouped by the ORDER BY key is the form
-- verified against the live engine to return exactly what the raw events
-- prove; `FINAL` parses but is silently ignored and cannot be used here.
INSERT INTO stats_hourly (
    tenant_id, site_id, ts_bucket, pathname, event_type,
    pageviews, visitors, sessions, bounces, total_duration, version
)
SELECT
    tenant_id, site_id, ts_bucket, pathname, event_type,
    argMax(pageviews, version),
    argMax(visitors, version),
    argMax(sessions, version),
    argMax(bounces, version),
    argMax(total_duration, version),
    MAX(version)
FROM stats_hourly_pre033
GROUP BY tenant_id, site_id, ts_bucket, pathname, event_type;

INSERT INTO stats_daily (
    tenant_id, site_id, ts_bucket, pathname, event_type,
    referrer, browser, os, country, device,
    utm_source, utm_medium, utm_campaign,
    pageviews, visitors, sessions, bounces, total_duration, version
)
SELECT
    tenant_id, site_id, ts_bucket, pathname, event_type,
    referrer, browser, os, country, device,
    utm_source, utm_medium, utm_campaign,
    argMax(pageviews, version),
    argMax(visitors, version),
    argMax(sessions, version),
    argMax(bounces, version),
    argMax(total_duration, version),
    MAX(version)
FROM stats_daily_pre033
GROUP BY tenant_id, site_id, ts_bucket, pathname, event_type,
         referrer, browser, os, country, device,
         utm_source, utm_medium, utm_campaign;

INSERT INTO sessions (
    tenant_id, site_id, session_id, first_ts, last_ts,
    pageviews, events_count, entry_url, exit_url,
    referrer, browser, os, device, country, language,
    screen_width, screen_height,
    utm_source, utm_medium, utm_campaign,
    is_bounce, version, release_tag
)
SELECT
    tenant_id, site_id, session_id,
    argMax(first_ts, version),
    argMax(last_ts, version),
    argMax(pageviews, version),
    argMax(events_count, version),
    argMax(entry_url, version),
    argMax(exit_url, version),
    argMax(referrer, version),
    argMax(browser, version),
    argMax(os, version),
    argMax(device, version),
    argMax(country, version),
    argMax(language, version),
    argMax(screen_width, version),
    argMax(screen_height, version),
    argMax(utm_source, version),
    argMax(utm_medium, version),
    argMax(utm_campaign, version),
    argMax(is_bounce, version),
    MAX(version),
    argMax(release_tag, version)
FROM sessions_pre033
GROUP BY tenant_id, site_id, session_id;
