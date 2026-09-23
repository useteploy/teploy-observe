-- 050 (2026-09-23): O10 - the notification outbox, the durable delivery half
-- of alerting (the 046 derived_outbox shape applied to externally-visible
-- side effects; 046's reserved 'webhook' kind stays reserved - notifications
-- get their own table because their dispositions differ: a notification can
-- be SUPPRESSED by a maintenance window, and suppression must stay visible).
--
-- A threshold crossing enqueues one row per (notification, webhook target)
-- INSIDE the same transaction as the evaluation edge that caused it, so an
-- intent can never orphan ahead of its evaluation and an evaluation never
-- lands without the notification it owes. The row IS the delivery identity:
--
--   id  stable across every attempt of this notification, sent as the
--       X-Observe-Delivery header (the R22 dedupe marker, keyed on the
--       intent id). Delivery is at-least-once (a crash between the POST and
--       the disposition mark re-POSTs after restart); the receiver dedupes
--       on the header for the exactly-once effect - the same posture as the
--       replay ledger.
--
-- Target fields (webhook_id, target_type, target_url, secret) are FROZEN at
-- enqueue: a retry derives from the frozen target, so a webhook edited
-- mid-retry cannot half-apply, and re-delivery reproduces the identical POST.
--
-- Row lifecycle (mirroring derived_outbox exactly):
--   enqueue          attempts=0, delivered_at=0, suppressed_at=0
--   worker delivers  new version with delivered_at set
--   worker fails     attempts+1, last_error, next_attempt_at = now +
--                    exponential backoff (doubling, capped)
--   attempt budget   DEAD LETTER: next_attempt_at = -1, a sentinel no clock
--                    reaches, last_error kept - countable at /healthz,
--                    never auto-pruned, never retried even by a later
--                    process with a larger budget
--   maintenance      new version with suppressed_at + suppressed_reason set
--                    (checked at delivery time; evaluation is unaffected -
--                    the suppression is a delivery decision, and it stays
--                    inspectable on the row)
--
-- ReplacingMergeTree keyed on the intent id, collapsed at read time by the
-- argMax form (internal/query/replacing.go) - one logical row per intent.
--
-- Retention: delivered OR suppressed rows prune at the decided outbox
-- window (7d, jobs.DefaultLedgerPolicies); pending, retrying and dead
-- letters are never auto-deleted.
--
-- Single-process boundary (the standing AUD-018 posture): the drain
-- serializes in-process; a multi-replica deployment needs a lease/CAS claim
-- before this table carries external side effects.
--
-- Comments pure ASCII (038 rule).

CREATE TABLE IF NOT EXISTS notification_outbox (
    tenant_id         TEXT NOT NULL DEFAULT 'default',
    id                TEXT NOT NULL,
    kind              TEXT NOT NULL,
    rule_id           TEXT NOT NULL DEFAULT '',
    incident_id       TEXT NOT NULL DEFAULT '',
    site_id           TEXT NOT NULL,
    webhook_id        TEXT NOT NULL DEFAULT '',
    target_type       TEXT NOT NULL DEFAULT 'http',
    target_url        TEXT NOT NULL DEFAULT '',
    secret            TEXT NOT NULL DEFAULT '',
    payload           TEXT NOT NULL DEFAULT '',
    created_at        BIGINT NOT NULL,
    attempts          BIGINT NOT NULL DEFAULT 0,
    next_attempt_at   BIGINT NOT NULL DEFAULT 0,
    delivered_at      BIGINT NOT NULL DEFAULT 0,
    suppressed_at     BIGINT NOT NULL DEFAULT 0,
    suppressed_reason TEXT NOT NULL DEFAULT '',
    last_error        TEXT NOT NULL DEFAULT '',
    version           BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, id);
