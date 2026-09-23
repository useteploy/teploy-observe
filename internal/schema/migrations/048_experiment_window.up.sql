-- O09 experiment input semantics (decided 2026-09-23): conversions count
-- only within the declared conversion window after an exposure. The window
-- is anchored at exposure time, so a conversion recorded after the
-- experiment stopped still counts when it falls inside the window of an
-- in-interval exposure (late-arrival policy). Default 72 hours; a retry of
-- the same record after any expiry is processed as new - the same honest
-- posture as the O01 ledgers.
ALTER TABLE experiments ADD COLUMN IF NOT EXISTS conversion_window_hours BIGINT NOT NULL DEFAULT 72;
