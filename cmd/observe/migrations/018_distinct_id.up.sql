-- A3 (Wave 1, 2026-05-10): land the schema columns the SDK identify()
-- contract needs. No persons/cohorts UI yet — those wait for Wave 4 — but
-- once this lands, every event from a `client.identify(userId)` call has
-- a place to live, and the day the persons UI ships there's no "we have
-- no history" debt.
--
-- distinct_id is the public-facing logical user ID supplied by the SDK.
-- The server hashes it with site.session_salt before storage (HMAC-SHA256
-- truncated to 16 hex chars, matching the existing session-id pattern)
-- unless the site setting raw_distinct_id is set, in which case the
-- raw value is stored as-is.
--
-- Default '' means pre-existing rows and any event without an identify
-- call still satisfy NOT NULL.

ALTER TABLE events ADD COLUMN IF NOT EXISTS distinct_id TEXT NOT NULL DEFAULT '';
ALTER TABLE replay_sessions ADD COLUMN IF NOT EXISTS distinct_id TEXT NOT NULL DEFAULT '';
ALTER TABLE error_events ADD COLUMN IF NOT EXISTS distinct_id TEXT NOT NULL DEFAULT '';

-- Per-site opt-out. When true, the server stores the raw user ID without
-- HMAC. Default false keeps the privacy-by-default contract.
ALTER TABLE sites ADD COLUMN IF NOT EXISTS raw_distinct_id BOOLEAN NOT NULL DEFAULT false;
