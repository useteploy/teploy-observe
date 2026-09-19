-- 042 (2026-09-18): audit chain key ids (F46, the dedicated key store half).
--
-- The chain hash is HMAC(key, prev_hash || fields), but rows never recorded
-- WHICH key signed them: verification implicitly assumed the one key it was
-- handed, so rotation was impossible (retiring an old key retired the
-- ability to verify everything it signed), and the only persistent key most
-- installs had was the JWT secret — shared with a different security domain.
--
-- key_id names the signing key per row: '' is the legacy encoding (signed
-- pre-042 by whatever NewService was handed — the dedicated audit key, the
-- JWT-secret fallback, or nothing; verification tries the configured legacy
-- candidates), any other value is looked up in the verification keyring
-- (OBSERVE_AUDIT_KEYRING) or matches the active signing key. New rows are
-- stamped by the resolved signer: OBSERVE_AUDIT_KEY when set, else the
-- persistent generated key file (data/audit.key — created on first boot, so
-- every install's chain is keyed without operator action), else the JWT
-- fallback.
--
-- Rebuild rather than ALTER-ADD (the 027 pattern: ALTER ADD COLUMN is the
-- Nucleus 0.1.0 silent-insert-loss bug). Rename aside, create with the new
-- column, copy rows verbatim with key_id='' — existing rows keep verifying
-- under the legacy candidates, preserving the chain across the migration.
-- audit_events_pre042 is deliberately LEFT IN PLACE as a recovery artifact;
-- drop it by hand once the copy is confirmed.

ALTER TABLE audit_events RENAME TO audit_events_pre042;

CREATE TABLE IF NOT EXISTS audit_events (
    audit_id    TEXT NOT NULL,
    tenant_id   TEXT NOT NULL DEFAULT 'default',
    site_id     TEXT NOT NULL DEFAULT 'default',
    timestamp   BIGINT NOT NULL,
    actor       TEXT NOT NULL DEFAULT '',
    actor_type  TEXT NOT NULL DEFAULT 'user',
    action      TEXT NOT NULL,
    target      TEXT NOT NULL DEFAULT '',
    result      TEXT NOT NULL DEFAULT 'success',
    source_ip   TEXT NOT NULL DEFAULT '',
    user_agent  TEXT NOT NULL DEFAULT '',
    metadata    TEXT NOT NULL DEFAULT '{}',
    -- F46: id of the key that signed this row's hash ('' = pre-042 legacy
    -- encoding; verified against the configured legacy candidates).
    key_id      TEXT NOT NULL DEFAULT '',
    seq         BIGINT NOT NULL DEFAULT 0,
    prev_hash   TEXT NOT NULL DEFAULT '',
    hash        TEXT NOT NULL DEFAULT ''
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, timestamp);

INSERT INTO audit_events (
    audit_id, tenant_id, site_id, timestamp, actor, actor_type, action,
    target, result, source_ip, user_agent, metadata, key_id, seq,
    prev_hash, hash
)
SELECT
    audit_id, tenant_id, site_id, timestamp, actor, actor_type, action,
    target, result, source_ip, user_agent, metadata, '', seq,
    prev_hash, hash
FROM audit_events_pre042;
