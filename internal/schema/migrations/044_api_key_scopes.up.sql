-- 044 (2026-09-19): API key capabilities (R07, round 4).
--
-- Site keys are published in page configuration for telemetry, but the
-- source-map upload route accepted ANY valid site key: a browser-visible
-- telemetry credential authorized artifact publication and retention
-- triggering. The missing boundary was capability, not site ownership.
--
-- scopes is a comma-separated capability set. Valid capabilities:
--   telemetry   ingest analytics events, errors, logs, traces, metrics
--   publish     upload source maps and trigger release retention
-- Migration backfill grants existing keys 'telemetry' ONLY (a browser-exposed
-- key must not silently gain publish rights); operators mint a dedicated
-- publish key for CI. Rebuild rather than ALTER-ADD (the 027 pattern);
-- api_keys_pre044 is left in place as a recovery artifact.

ALTER TABLE api_keys RENAME TO api_keys_pre044;

CREATE TABLE IF NOT EXISTS api_keys (
    key_id         TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    key_hash       TEXT NOT NULL,
    label          TEXT,
    created_at     TEXT NOT NULL,
    revoked        TEXT NOT NULL DEFAULT 'false',
    scopes         TEXT NOT NULL DEFAULT 'telemetry'
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, key_hash);

INSERT INTO api_keys (key_id, tenant_id, site_id, key_hash, label, created_at, revoked, scopes)
SELECT key_id, tenant_id, site_id, key_hash, label, created_at, revoked, 'telemetry'
FROM api_keys_pre044;
