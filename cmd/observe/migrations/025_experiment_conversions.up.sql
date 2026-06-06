-- The experiments table was created (migration 007) without a `variants`
-- column, yet the service reads and writes it on every create/list/results
-- call — so experiment creation failed outright with "column variants does
-- not exist". Add it. This is the schema half of why A/B testing was
-- non-functional end to end.
ALTER TABLE experiments ADD COLUMN IF NOT EXISTS variants TEXT NOT NULL DEFAULT '';

-- Experiment results were non-functional: conversions were modeled as a
-- row-copy back into experiment_exposures (a plain mergetree), which both
-- duplicated rows and corrupted exposure/conversion counts. Conversions now
-- live in their own append-only table; Results counts DISTINCT user_id on each
-- side so repeated exposures or repeated conversions for the same user collapse
-- to one. No engine change to experiment_exposures is needed because the read
-- path no longer trusts row counts.

CREATE TABLE IF NOT EXISTS experiment_conversions (
    conversion_id  TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    experiment_id  TEXT NOT NULL,
    site_id        TEXT NOT NULL,
    user_id        TEXT NOT NULL DEFAULT '',
    variant        TEXT NOT NULL DEFAULT '',
    timestamp      BIGINT NOT NULL
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, experiment_id, user_id);
