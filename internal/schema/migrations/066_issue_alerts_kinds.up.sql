-- 066 (2026-10-17): EV1 - issue-level alert notifications on the signed
-- outbox, with a per-webhook `kinds` routing filter. webhooks.kinds is a
-- comma-delimited subset of the notification kind domain
-- (incident_opened, incident_recovered, incident_repeat, cron_missed,
-- cron_recovered, issue_new, issue_regression, issue_spike). EMPTY means
-- "the kinds that existed before this migration": an existing webhook
-- receives NO issue notifications until an operator opts in. That is
-- deliberate - a receiver that never asked for issue traffic must not
-- start parsing it, and a Slack channel must never suddenly receive
-- stacks. Validation happens at CREATE time in the service (webhooks.go);
-- the emitter freezes the target per intent at enqueue, so the routing
-- decision is durable on the intent row (same posture as severities,
-- 056).
--
-- Rename-aside + create + copy, the 027/028/047 pattern - NOT
-- ALTER TABLE ADD COLUMN, which corrupts populated tables on the current
-- engine (open P1 L9; 056 predates the rule, enforced for new migrations
-- by TestNewMigrationsDoNotAlterAddColumn). argMax collapse keyed on the
-- ORDER BY key, MAX(version) carried. webhooks_pre066 stays in place as
-- the recovery artifact (the rename does not copy data); a fresh install
-- ends with an empty artifact. No IF EXISTS on the rename (Nucleus
-- resolves the table before consulting the flag; webhooks is guaranteed
-- to exist from 004).
--
-- Comments deliberately ASCII (038 rule: a multi-byte character followed
-- by a number inside a -- comment panics the Nucleus v0.1.8 SQL lexer).

ALTER TABLE webhooks RENAME TO webhooks_pre066;

CREATE TABLE IF NOT EXISTS webhooks (
    webhook_id     TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    webhook_type   TEXT NOT NULL DEFAULT 'http',
    url            TEXT NOT NULL DEFAULT '',
    secret         TEXT NOT NULL DEFAULT '',
    severities     TEXT NOT NULL DEFAULT '',
    kinds          TEXT NOT NULL DEFAULT '',
    enabled        TEXT NOT NULL DEFAULT 'true',
    created_at     TEXT NOT NULL,
    version        BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, webhook_id);

INSERT INTO webhooks (
    webhook_id, tenant_id, site_id, name, webhook_type, url, secret,
    severities, kinds, enabled, created_at, version
)
SELECT
    webhook_id,
    tenant_id,
    site_id,
    argMax(name, version),
    argMax(webhook_type, version),
    argMax(url, version),
    argMax(secret, version),
    argMax(severities, version),
    '',
    argMax(enabled, version),
    argMax(created_at, version),
    MAX(version)
FROM webhooks_pre066
GROUP BY tenant_id, site_id, webhook_id;
