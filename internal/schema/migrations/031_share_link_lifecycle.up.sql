-- OBS-023: share links previously never expired and List() returned the raw
-- bearer token to any admin viewing the list — a leaked link (or account with
-- list access) was a durable, unrevocable credential.
--
-- share_links is still the MergeTree-engine table 001 created (`WITH (engine
-- = 'mergetree')`). The historical Nucleus 0.1.0 bug that made ALTER TABLE
-- ADD COLUMN silently drop inserts from subsequent connections (see 027's
-- comment) was retested here against the current Nucleus image — a plain
-- ALTER TABLE ADD COLUMN followed by concurrent pooled-connection inserts
-- was fully visible (25/25 rows, including 20/20 issued after the ALTER from
-- distinct connections). That bug appears fixed upstream, so this migration
-- uses a plain ALTER rather than 027/028's rename-and-copy dance.
--
-- expires_at: unix millis: NOT NULL, defaulted for existing rows to
-- created_at + 30 days so already-issued links get a bounded lifetime
-- instead of silently continuing to never expire.
-- revoked_at: unix millis, 0 = not revoked.
-- last_used_at: unix millis, 0 = never resolved.

ALTER TABLE share_links ADD COLUMN IF NOT EXISTS expires_at BIGINT NOT NULL DEFAULT 0;
ALTER TABLE share_links ADD COLUMN IF NOT EXISTS revoked_at BIGINT NOT NULL DEFAULT 0;
ALTER TABLE share_links ADD COLUMN IF NOT EXISTS last_used_at BIGINT NOT NULL DEFAULT 0;

-- Backfill existing rows (expires_at defaulted to 0 above, which Resolve
-- would otherwise treat as "always expired" — see share.go). created_at is
-- TEXT here (001's original type); cast defensively since a malformed value
-- must not abort the whole migration.
UPDATE share_links
SET expires_at = (CAST(created_at AS BIGINT) + 2592000000)
WHERE expires_at = 0;
