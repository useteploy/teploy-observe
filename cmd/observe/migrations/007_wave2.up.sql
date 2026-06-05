-- Observe Wave 2 — Feature flags, experiments, surveys.

-- ============================================================================
-- Feature flags
-- ============================================================================

CREATE TABLE IF NOT EXISTS feature_flags (
    flag_id        TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    flag_key       TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    description    TEXT NOT NULL DEFAULT '',
    flag_type      TEXT NOT NULL DEFAULT 'boolean',
    enabled        TEXT NOT NULL DEFAULT 'false',
    rollout_pct    TEXT NOT NULL DEFAULT '100',
    variants       JSONB,
    targeting      JSONB,
    created_at     TEXT NOT NULL,
    version        BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, flag_id);

CREATE TABLE IF NOT EXISTS flag_evaluations (
    eval_id        TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    flag_key       TEXT NOT NULL,
    user_id        TEXT NOT NULL DEFAULT '',
    variant        TEXT NOT NULL DEFAULT '',
    timestamp      BIGINT NOT NULL
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, flag_key, timestamp);

-- ============================================================================
-- A/B experiments
-- ============================================================================

CREATE TABLE IF NOT EXISTS experiments (
    experiment_id  TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    flag_key       TEXT NOT NULL,
    goal_metric    TEXT NOT NULL DEFAULT 'pageview',
    goal_value     TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL DEFAULT 'draft',
    min_sample     TEXT NOT NULL DEFAULT '100',
    started_at     TEXT NOT NULL DEFAULT '0',
    ended_at       TEXT NOT NULL DEFAULT '0',
    created_at     TEXT NOT NULL,
    version        BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, experiment_id);

CREATE TABLE IF NOT EXISTS experiment_exposures (
    exposure_id    TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    experiment_id  TEXT NOT NULL,
    site_id        TEXT NOT NULL,
    user_id        TEXT NOT NULL DEFAULT '',
    variant        TEXT NOT NULL DEFAULT '',
    converted      TEXT NOT NULL DEFAULT 'false',
    timestamp      BIGINT NOT NULL
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, experiment_id, timestamp);

-- ============================================================================
-- Surveys
-- ============================================================================

CREATE TABLE IF NOT EXISTS surveys (
    survey_id      TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    questions      JSONB,
    appearance     JSONB,
    targeting      JSONB,
    status         TEXT NOT NULL DEFAULT 'draft',
    created_at     TEXT NOT NULL,
    version        BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, survey_id);

CREATE TABLE IF NOT EXISTS survey_responses (
    response_id    TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    survey_id      TEXT NOT NULL,
    site_id        TEXT NOT NULL,
    user_id        TEXT NOT NULL DEFAULT '',
    answers        JSONB,
    timestamp      BIGINT NOT NULL
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, survey_id, timestamp);
