-- 051 (2026-09-23): O10 - maintenance windows, incident acknowledgment and
-- the incident timeline.
--
-- maintenance_windows suppress NOTIFICATION DELIVERY only (checked by the
-- notifier at delivery time, 050): evaluation state keeps recording, the
-- incident still opens and closes, and the suppressed delivery stays
-- visible on the notification row plus this table. ReplacingMergeTree with
-- tombstone deletes, the webhooks pattern.
--
-- incident_events is the append-only timeline every incident action lands
-- on: 'opened' / 'recovered' (engine edges), 'notified' / 'suppressed'
-- (notifier dispositions), 'ack' (the API). Acknowledgment is ALSO carried
-- as columns on incidents (idempotent re-ack is a read not a write), which
-- is what the ack API mutates; the timeline row is the human-readable
-- record with actor and time.
--
-- incidents is a PLAIN mergetree whose reads collapse by argMax over
-- updated_at (internal/incidents latestSelect). The two new columns ride
-- that existing collapse: Close and Ack copy them forward on every
-- version-rewriting insert (an explicit column list that omitted them
-- would silently drop an ack on close). ADD COLUMN IF NOT EXISTS follows
-- the 048 precedent; BIGINT 0 / TEXT '' backfill every existing incident
-- as unacknowledged.
--
-- Comments pure ASCII (038 rule).

ALTER TABLE incidents ADD COLUMN IF NOT EXISTS acknowledged_at BIGINT NOT NULL DEFAULT 0;
ALTER TABLE incidents ADD COLUMN IF NOT EXISTS acknowledged_by TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS maintenance_windows (
    window_id  TEXT NOT NULL,
    tenant_id  TEXT NOT NULL DEFAULT 'default',
    site_id    TEXT NOT NULL DEFAULT '',
    starts_at  BIGINT NOT NULL,
    ends_at    BIGINT NOT NULL,
    reason     TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL DEFAULT '',
    created_at BIGINT NOT NULL,
    enabled    TEXT NOT NULL DEFAULT 'true',
    version    BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, window_id);

CREATE TABLE IF NOT EXISTS incident_events (
    event_id    TEXT NOT NULL,
    tenant_id   TEXT NOT NULL DEFAULT 'default',
    incident_id TEXT NOT NULL,
    at          BIGINT NOT NULL,
    kind        TEXT NOT NULL,
    actor       TEXT NOT NULL DEFAULT '',
    detail      TEXT NOT NULL DEFAULT ''
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, incident_id, at);
