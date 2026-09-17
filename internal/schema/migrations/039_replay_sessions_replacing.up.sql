-- 039 (2026-09-17): replay_sessions becomes a ReplacingMergeTree keyed on the
-- replay, so session metadata can be updated by later batches (audit F20) and
-- a crash between claim and insert can no longer orphan a replay (audit F19).
--
-- Background: replay_sessions was a plain mergetree ordered by
-- (tenant_id, site_id, start_time). Multi-batch replays (the normal case —
-- the tracker flushes every 10s) wrote their session row ONCE, guarded by a
-- KV SetNX claim keyed replay_seen:<site>:<id>. That claim was the F19
-- defect: it succeeded before the session INSERT, so a transient SQL failure
-- or crash after the claim left every later batch believing the session
-- existed. It also meant duration/page_count/has_error were frozen at the
-- first batch's values (F20).
--
-- With a replacing table keyed on the replay, the session row is simply
-- re-inserted as a new version with merged aggregates on every batch: the
-- collapse keeps one row per replay, and there is no claim to orphan. The KV
-- dedup key and its 6-hour expiry are gone entirely (removed in replays.go).
--
-- Key choice: (tenant_id, site_id, start_time, replay_id). replay_id must be
-- in the key for versions to collapse; start_time STAYS in the key because
-- (a) every version of a row preserves the first batch's start_time verbatim
-- so the key is stable across versions, and (b) the retention job DELETEs by
-- start_time, and Nucleus DML filtered on a column outside the ORDER BY key
-- silently no-ops (see internal/auth/apikeys.go) — dropping start_time from
-- the key would have made the 14-day replay TTL inert.
--
-- Rename-aside + create + copy, in the style of 027/028/033-036. ALTER TABLE
-- ADD COLUMN cannot change the engine or the ORDER BY, so a rebuild is the
-- only route. Legacy version is start_time (epoch-ms) — every new version
-- written after this migration stamps version = max(now_ms, prior+1), which
-- always supersedes it. Legacy duplicates from the pre-OBS-029 race collapse
-- via argMax on the shared key.
--
-- replay_sessions_pre039 is deliberately LEFT IN PLACE as a recovery
-- artifact; drop it by hand once the copy is confirmed.

ALTER TABLE replay_sessions RENAME TO replay_sessions_pre039;

CREATE TABLE IF NOT EXISTS replay_sessions (
    replay_id    TEXT NOT NULL,
    tenant_id    TEXT NOT NULL DEFAULT 'default',
    site_id      TEXT NOT NULL,
    session_id   TEXT NOT NULL DEFAULT '',
    start_time   BIGINT NOT NULL,
    duration_ms  TEXT NOT NULL DEFAULT '0',
    page_count   TEXT NOT NULL DEFAULT '0',
    url          TEXT NOT NULL DEFAULT '',
    browser      TEXT NOT NULL DEFAULT '',
    os           TEXT NOT NULL DEFAULT '',
    device       TEXT NOT NULL DEFAULT '',
    has_error    TEXT NOT NULL DEFAULT 'false',
    distinct_id  TEXT NOT NULL DEFAULT '',
    version      BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, start_time, replay_id);

INSERT INTO replay_sessions (
    replay_id, tenant_id, site_id, session_id, start_time,
    duration_ms, page_count, url, browser, os, device,
    has_error, distinct_id, version
)
SELECT
    replay_id, tenant_id, site_id,
    argMax(session_id, start_time),
    start_time,
    argMax(duration_ms, start_time),
    argMax(page_count, start_time),
    argMax(url, start_time),
    argMax(browser, start_time),
    argMax(os, start_time),
    argMax(device, start_time),
    argMax(has_error, start_time),
    argMax(distinct_id, start_time),
    MAX(start_time)
FROM replay_sessions_pre039
GROUP BY tenant_id, site_id, start_time, replay_id;
