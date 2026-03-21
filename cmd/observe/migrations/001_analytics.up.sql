-- Observe v0.1 Analytics Schema
-- Nucleus MergeTree tables for web analytics events, sessions, and rollups.
--
-- Design principles:
--   - tenant_id placeholder for future multi-tenant (defaults to 'default')
--   - Cookie-free sessions via deterministic UUID v5(site + IP + UA + salt)
--   - JSONB for flexible event properties
--   - MergeTree for raw events (zone map pruning on timestamp)
--   - AggregatingMergeTree for session summaries
--   - ReplacingMergeTree for rollups (idempotent re-computation)

-- ============================================================================
-- Raw analytics events (pageviews, custom events)
-- ============================================================================
-- ORDER BY puts tenant + date + event_type first for partition pruning,
-- then session_id for locality within a session.

CREATE TABLE IF NOT EXISTS events (
    event_id       TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    session_id     TEXT NOT NULL,
    visit_id       TEXT NOT NULL,
    event_type     TEXT NOT NULL DEFAULT 'pageview',
    timestamp      BIGINT NOT NULL,
    url            TEXT,
    referrer       TEXT,
    title          TEXT,
    hostname       TEXT,
    pathname       TEXT,
    language       TEXT,
    country        TEXT,
    region         TEXT,
    city           TEXT,
    browser        TEXT,
    browser_version TEXT,
    os             TEXT,
    os_version     TEXT,
    device         TEXT,
    screen_width   INTEGER,
    screen_height  INTEGER,
    utm_source     TEXT,
    utm_medium     TEXT,
    utm_campaign   TEXT,
    utm_term       TEXT,
    utm_content    TEXT,
    properties     JSONB
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, timestamp, event_type, session_id);

-- ============================================================================
-- Recent events (fast table for "last hour" / real-time queries)
-- ============================================================================
-- Separate table with tighter ordering for real-time dashboard.
-- Populated by materialized view on events. Cleaned by retention job (7 days).

CREATE TABLE IF NOT EXISTS events_recent (
    event_id       TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    session_id     TEXT NOT NULL,
    event_type     TEXT NOT NULL DEFAULT 'pageview',
    timestamp      BIGINT NOT NULL,
    pathname       TEXT,
    referrer       TEXT,
    browser        TEXT,
    os             TEXT,
    country        TEXT,
    properties     JSONB
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, timestamp);

-- ============================================================================
-- Session summaries (ReplacingMergeTree)
-- ============================================================================
-- Re-computed every 5 minutes from recent events. ReplacingMergeTree
-- deduplicates by (tenant_id, site_id, session_id) on merge, keeping the
-- row with the highest version. This makes overlapping re-computation safe.

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
    version        BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, session_id);

-- ============================================================================
-- Hourly rollups (for dashboard time-series queries)
-- ============================================================================
-- Pre-aggregated hourly buckets. Populated by cron job.
-- ReplacingMergeTree deduplicates by ORDER BY key on merge, keeping the
-- row with the highest version. This makes rollup re-computation idempotent.
-- 1 year retention.

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

-- ============================================================================
-- Daily rollups (for long-range trend queries)
-- ============================================================================
-- Indefinite retention. ReplacingMergeTree for idempotent re-computation.

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

-- ============================================================================
-- API keys for ingestion auth
-- ============================================================================

CREATE TABLE IF NOT EXISTS api_keys (
    key_id         TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    key_hash       TEXT NOT NULL,
    label          TEXT,
    created_at     BIGINT NOT NULL,
    revoked        BOOLEAN DEFAULT false
);

-- ============================================================================
-- Sites registry
-- ============================================================================

CREATE TABLE IF NOT EXISTS sites (
    site_id        TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    domain         TEXT NOT NULL,
    name           TEXT,
    created_at     BIGINT NOT NULL,
    session_salt   TEXT NOT NULL
);

-- ============================================================================
-- Admin users for dashboard auth
-- ============================================================================

CREATE TABLE IF NOT EXISTS admin_users (
    id             TEXT NOT NULL,
    username       TEXT NOT NULL,
    password_hash  TEXT NOT NULL,
    created_at     BIGINT NOT NULL
);

-- ============================================================================
-- Share links for public dashboard access
-- ============================================================================

CREATE TABLE IF NOT EXISTS share_links (
    token          TEXT NOT NULL,
    site_id        TEXT NOT NULL,
    created_at     BIGINT NOT NULL
);
