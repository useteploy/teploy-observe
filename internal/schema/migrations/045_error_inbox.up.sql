-- 045 (2026-09-22): error inbox (O01 ADR section 5.6, implementation
-- slice 2; closes R14's deferred durable half).
--
-- An error record with producer identity (site_id, producer_id,
-- event_id) carries a sha256 payload digest computed at admission over
-- the frozen request body. The inbox row is the durable dedupe ledger:
-- it is checked and inserted INSIDE the same transaction as the
-- error_events insert (the claim IS a row in the same atomic SQL unit -
-- the same boundary replay_batches established in 041, not a KV claim
-- followed by an insert).
--
-- Dispositions:
--  * same key + same digest      -> deduped: acknowledged, applied once
--    by the first admission (crash-after-ack replay, lost-response
--    retry, post-restart retry - all collapse here).
--  * same key + different digest -> CONFLICT: rejected 409 at admission
--    when the in-process cache knows the id; after a restart the flush
--    counts it conflicting and never applies or merges it. Reusing an
--    event_id for different content is a producer bug and is surfaced,
--    never silently absorbed.
--  * no event_id (legacy producers) -> no inbox row: durable WAL ack and
--    replay exactly-once server-side, but a client retry re-applies
--    (the documented v1 posture, same as events without ids).
--
-- Retention: none yet - slice 5 (ADR section 5.8) gives the ledger an
-- explicit 30d policy with the documented expiry semantics (a retry
-- after expiry is processed as new). Until then this is the same
-- unbounded exactly-once memory replay_batches carries.

CREATE TABLE IF NOT EXISTS error_inbox (
    tenant_id    TEXT NOT NULL DEFAULT 'default',
    site_id      TEXT NOT NULL,
    producer_id  TEXT NOT NULL DEFAULT '',
    event_id     TEXT NOT NULL,
    payload_sha  TEXT NOT NULL,
    issue_id     TEXT NOT NULL DEFAULT '',
    applied_at   BIGINT NOT NULL,
    version      BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, producer_id, event_id);
