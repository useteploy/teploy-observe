-- B2 Phase 1 (Wave 1, 2026-05-10): release_tag on the sessions table so
-- crash-free session % can be computed per release. error_events already
-- carries release_tag (set on every error ingest); without the matching
-- column on sessions, the join needed for crash-free % was impossible.
--
-- The session rollup picks the FIRST event's release_tag for the session,
-- so events also needs the column. Default '' so existing sessions and
-- any session without a release in the SDK init still satisfy NOT NULL.

ALTER TABLE events ADD COLUMN IF NOT EXISTS release_tag TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS release_tag TEXT NOT NULL DEFAULT '';
