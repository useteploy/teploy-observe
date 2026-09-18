-- 041 (2026-09-18): replay batch ledger (audits F12/F19, idempotency half).
--
-- A v2 replay batch carries (producer_id, batch_id) identity. The ledger
-- records, per (tenant, site, replay, batch) key, the event count and a
-- sha256 digest of the batch's event list. It is written INSIDE the same
-- transaction as the session upsert and the child event inserts, so the
-- dedupe boundary is one atomic SQL unit. This closes the audit's core
-- objection to the pre-039 design: a KV SetNX claim followed by a SQL
-- INSERT is not atomic on Nucleus, and a crash between the two orphaned
-- every later batch. Here the claim IS a row in the same transaction as
-- the data it claims to admit.
--
-- Retry semantics:
--  * retry of a committed batch -> ledger hit, digest matches -> the
--    server returns deduped=true and writes nothing (no children, no
--    session aggregate merge, no heatmap rollup).
--  * retry of a rolled-back batch -> no ledger row AND no children (they
--    rolled back together) -> full reprocess, with child event ids
--    derived deterministically from (site, replay, batch, index), so the
--    re-inserted children land on the same identities.
--  * same (producer, batch) key with different content -> 409
--    ErrBatchIDReuse; a silent drop would hide producer bugs.
--
-- ReplacingMergeTree keyed on the full identity: rows are insert-once in
-- the happy path; the replacing engine exists so a cross-process double
-- submit of the same key collapses to one visible row at read time (the
-- documented single-process boundary is the striped replay lock + tx;
-- multi-replica needs the CAS design deferred with AUD-018).
--
-- Retention: no policy. The ledger is one row per batch and is the
-- dedupe memory for exactly-once ingest; pruning it would reintroduce
-- the double-count window for producers that retry late. Revisit only
-- with a producer-visible dedupe horizon contract.

CREATE TABLE IF NOT EXISTS replay_batches (
    tenant_id    TEXT NOT NULL DEFAULT 'default',
    site_id      TEXT NOT NULL,
    replay_id    TEXT NOT NULL,
    batch_id     TEXT NOT NULL,
    event_count  BIGINT NOT NULL,
    payload_sha  TEXT NOT NULL,
    first_seen   BIGINT NOT NULL,
    version      BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, replay_id, batch_id);
