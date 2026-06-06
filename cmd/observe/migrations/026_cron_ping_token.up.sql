-- Cron check-ins were authorized by (site_id, slug), both guessable, so anyone
-- could spoof a heartbeat for another site's cron (suppressing a real
-- missed-cron alert). createCron also never set a slug, so the slug-based
-- check-in route never matched. Switch to an opaque per-cron ping token: the
-- check-in URL carries a high-entropy token and possession of the token is what
-- authorizes the heartbeat.
ALTER TABLE cron_monitors ADD COLUMN IF NOT EXISTS ping_token TEXT NOT NULL DEFAULT '';
