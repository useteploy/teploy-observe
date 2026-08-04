-- Audit events: an append-only, immutable compliance/audit trail of who did
-- what, when, from where. The shared sink for access-audit across the Teploy
-- stack (observe admin actions, CLI, dash, Ship agent) — HashiCorp parity A4 +
-- SOC2 evidence. Never UPDATEd or DELETEd; timestamp is unix-millis (BIGINT),
-- matching the rest of observe. ORDER BY puts the common query grain
-- (tenant, site, most-recent-first) at the sort-key prefix.
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
    -- Tamper-evidence hash chain: seq is a per-writer monotonic counter, hash
    -- is HMAC(key, prev_hash || fields). Any edit/delete/insert breaks the
    -- chain; see internal/audit Verify.
    seq         BIGINT NOT NULL DEFAULT 0,
    prev_hash   TEXT NOT NULL DEFAULT '',
    hash        TEXT NOT NULL DEFAULT ''
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, timestamp);
