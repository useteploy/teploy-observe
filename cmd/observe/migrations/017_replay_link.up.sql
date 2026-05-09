-- Replay <-> error linking (Sentry parity, W4.8).
-- Adds `replay_id` to `error_events` so SDK clients can attach a session
-- replay to every captured exception. The errors UI cross-jumps to the
-- replay player for context; the sessions UI reverse-counts errors.
ALTER TABLE error_events ADD COLUMN IF NOT EXISTS replay_id TEXT NOT NULL DEFAULT '';
