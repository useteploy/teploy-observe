-- Integration deliveries: a row per attempt to deliver an alert payload through
-- a configured integration (slack/email/jira/github/pagerduty). Backs the
-- "delivery history + replay" UX so users can debug failed alerts.
CREATE TABLE IF NOT EXISTS integration_deliveries (
    delivery_id     TEXT NOT NULL,
    tenant_id       TEXT NOT NULL DEFAULT 'default',
    integration_id  TEXT NOT NULL,
    site_id         TEXT NOT NULL,
    payload         TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    error_message   TEXT NOT NULL DEFAULT '',
    duration_ms     BIGINT NOT NULL DEFAULT 0,
    created_at      BIGINT NOT NULL,
    is_test         TEXT NOT NULL DEFAULT 'false',
    is_replay       TEXT NOT NULL DEFAULT 'false'
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, integration_id, created_at);
