-- Observe Wave 4 — Groups, SSO.

-- ============================================================================
-- Groups (company-level analytics)
-- ============================================================================

CREATE TABLE IF NOT EXISTS groups (
    group_id       TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    group_type     TEXT NOT NULL DEFAULT 'company',
    name           TEXT NOT NULL DEFAULT '',
    properties     JSONB,
    created_at     TEXT NOT NULL,
    version        BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, group_id);

CREATE TABLE IF NOT EXISTS group_members (
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    group_id       TEXT NOT NULL,
    session_id     TEXT NOT NULL,
    user_id        TEXT NOT NULL DEFAULT '',
    joined_at      BIGINT NOT NULL
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, group_id, session_id);

-- ============================================================================
-- SSO configurations
-- ============================================================================

CREATE TABLE IF NOT EXISTS sso_configs (
    sso_id         TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    provider       TEXT NOT NULL DEFAULT 'saml',
    entity_id      TEXT NOT NULL DEFAULT '',
    sso_url        TEXT NOT NULL DEFAULT '',
    certificate    TEXT NOT NULL DEFAULT '',
    attribute_map  JSONB,
    enabled        TEXT NOT NULL DEFAULT 'false',
    created_at     TEXT NOT NULL,
    version        BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, sso_id);
