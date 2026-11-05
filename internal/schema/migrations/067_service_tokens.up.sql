-- 067: scoped service tokens, managed-site identity, key provenance, and the
-- stable instance id published by GET /.well-known/teploy.json.
--
-- Everything here is a NEW table, on purpose. sites and api_keys are read on
-- every authenticated ingest request; rebuilding them (rename-aside + create +
-- copy, the only allowed way to add a column since 057) to carry fields that
-- only the management surface reads is risk without benefit. A binary rolled
-- back past this migration simply ignores these tables.
--
-- All four are plain mergetree, because plain OLTP tables and replacing
-- collapses are not something this codebase relies on (see 037).

-- service_tokens follows mcp_tokens: every write appends, reads collapse to
-- the newest row per token_id with argMax over updated_at, and revoked_at
-- collapses with MAX so a stale last-used append can never undo a revocation.
-- hash is the hex SHA-256 of the token; the plaintext is shown once at creation.
-- managed_by is the namespace whose sites the token may act on. It lives on
-- the token, not derived from token_id, so a rotated token keeps its sites.
CREATE TABLE IF NOT EXISTS service_tokens (
    token_id     TEXT NOT NULL,
    tenant_id    TEXT NOT NULL DEFAULT 'default',
    name         TEXT NOT NULL,
    managed_by   TEXT NOT NULL,
    hash         TEXT NOT NULL,
    scopes       TEXT NOT NULL,
    created_by   TEXT NOT NULL,
    created_at   BIGINT NOT NULL,
    last_used_at BIGINT NOT NULL DEFAULT 0,
    revoked_at   BIGINT NOT NULL DEFAULT 0,
    updated_at   BIGINT NOT NULL
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, token_id);

-- A site with no row here is an ordinary operator-created site and no service
-- token can touch it. Lookups always confirm the site still exists, so a row
-- that outlives its site is inert.
CREATE TABLE IF NOT EXISTS site_management (
    tenant_id    TEXT NOT NULL DEFAULT 'default',
    site_id      TEXT NOT NULL,
    managed_by   TEXT NOT NULL,
    external_ref TEXT NOT NULL,
    created_at   BIGINT NOT NULL
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id);

-- Who minted an API key. Keys older than this table have no row and report an
-- empty created_by, which is honest: nothing recorded it at the time.
CREATE TABLE IF NOT EXISTS api_key_provenance (
    tenant_id  TEXT NOT NULL DEFAULT 'default',
    key_id     TEXT NOT NULL,
    site_id    TEXT NOT NULL,
    created_by TEXT NOT NULL,
    created_at BIGINT NOT NULL
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, key_id);

CREATE TABLE IF NOT EXISTS instance_meta (
    meta_key   TEXT NOT NULL,
    meta_value TEXT NOT NULL,
    created_at BIGINT NOT NULL
) WITH (engine = 'mergetree')
ORDER BY (meta_key, meta_value);
