-- 052 (2026-09-24): O14 - survey exposure tracking + response identity.
--
-- Programme O14: "Surveys need exposure/response identity and lifecycle."
-- Two gaps closed here:
--
-- 1. EXPOSURE IDENTITY. Surveys had no exposure tracking at all, so a
--    response rate (the only number a survey owner actually needs) was
--    uncomputable. survey_exposures records who was OFFERED a survey, on
--    the O03 identity model: entity_type is 'person' (the HMAC distinct_id
--    of the SDK's identify value, derived with the per-site salt exactly
--    like the analytics ingest path) or 'visitor-estimate' (the anonymous
--    site/IP/UA/salt/month fingerprint - explicitly an estimate, not
--    people; O03 ADR vocabulary). exposure_id is a row identity, NOT the
--    dedupe key: counts derive DISTINCT entity_id, so a retried exposure
--    ping appends a row without distorting unique-exposed. The table is
--    append-only mergetree like flag_evaluations / experiment_exposures.
--
-- 2. RESPONSE IDENTITY. survey_responses accepted no client identity, so
--    a retried submit inserted a second row (the F12 class - a lost HTTP
--    response used to double-count). client_id carries the producer-minted
--    response identity (validated [A-Za-z0-9_-]{8,64} at the boundary);
--    SubmitResponse dedupes on (site_id, client_id) and returns the
--    existing response_id with deduped:true. Absent client_id keeps the
--    v1 posture (no dedupe), documented - same ladder as events/errors.
--    entity_type/entity_id mirror exposures so the numerator of the
--    response rate counts the same kind of entity as the denominator.
--
-- survey_responses is a plain mergetree; ADD COLUMN IF NOT EXISTS follows
-- the 048/051 precedent, TEXT '' backfills existing rows as legacy
-- (unattributed, never deduped).
--
-- Lifecycle states (draft/active/closed) already exist and gate both
-- paths: exposures and responses are recorded for ACTIVE surveys only.
--
-- Comments pure ASCII (038 rule).

CREATE TABLE IF NOT EXISTS survey_exposures (
    exposure_id  TEXT NOT NULL,
    tenant_id    TEXT NOT NULL DEFAULT 'default',
    survey_id    TEXT NOT NULL,
    site_id      TEXT NOT NULL,
    entity_type  TEXT NOT NULL DEFAULT '',
    entity_id    TEXT NOT NULL DEFAULT '',
    timestamp    BIGINT NOT NULL
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, survey_id, timestamp);

ALTER TABLE survey_responses ADD COLUMN IF NOT EXISTS entity_type TEXT NOT NULL DEFAULT '';
ALTER TABLE survey_responses ADD COLUMN IF NOT EXISTS entity_id TEXT NOT NULL DEFAULT '';
ALTER TABLE survey_responses ADD COLUMN IF NOT EXISTS client_id TEXT NOT NULL DEFAULT '';
