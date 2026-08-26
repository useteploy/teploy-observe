-- MCP access tokens (see internal/mcp).
--
-- Dash keeps its MCP tokens in a JSON file beside auth.json because dash has a
-- local data directory. Observe has none — everything it owns lives in Nucleus
-- — so the store is a table.
--
-- Plain mergetree, not replacing: every write is an append and the read side
-- collapses to the newest row per token_id with argMax over updated_at, the
-- same shape `incidents` uses. A ReplacingMergeTree's collapse depends on an
-- engines.json registry entry that is absent for every table created before it
-- existed, so it is not something this codebase relies on.
--
-- `hash` is the hex SHA-256 of the token. The plaintext is shown once at
-- creation and is not recoverable — there is no read path that returns it.
-- A revoked token keeps its row (revoked_at > 0) rather than being deleted, so
-- the audit trail can still name what acted.

CREATE TABLE IF NOT EXISTS mcp_tokens (
    token_id     TEXT NOT NULL,
    tenant_id    TEXT NOT NULL DEFAULT 'default',
    name         TEXT NOT NULL,
    hash         TEXT NOT NULL,
    role         TEXT NOT NULL DEFAULT 'viewer',
    created_at   BIGINT NOT NULL,
    last_used_at BIGINT NOT NULL DEFAULT 0,
    revoked_at   BIGINT NOT NULL DEFAULT 0,
    updated_at   BIGINT NOT NULL
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, token_id);
