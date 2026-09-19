-- 043 (2026-09-19): producer-scoped replay batch identity (round-3 TO-019).
--
-- The v2 replay protocol carries (producer_id, batch_id), but the 041
-- ledger keyed dedupe and ErrBatchIDReuse on (site, replay, batch) alone:
-- two producers sharing a batch id on one replay collided (409 or a wrong
-- dedupe) even though the protocol's identity is the producer/batch PAIR,
-- and child event ids derived without the producer. A v2 payload missing
-- one identity field also fell back silently to non-idempotent v1 writes.
--
-- producer_id joins the ORDER BY key (rebuild, the 042 pattern: ALTER ADD
-- is avoided on purpose). Existing rows copy with producer_id='' and are
-- still honored: the ledger lookup first tries the exact
-- (site, replay, producer, batch) key and, on a miss, falls back to the
-- legacy producer_id='' row for the same batch -- an in-flight retry of a
-- pre-043 batch keeps deduplicating across the upgrade. Legacy rows are
-- never written; every new ledger row stamps its producer. Child ids
-- derive from (site, replay, producer, batch, index) now; a rolled-back
-- pre-043 batch has no children (one transaction), so no id collision is
-- possible across the cutover.
--
-- Validation change riding along: a request declaring v2 (or carrying any
-- identity field) must carry ALL of producer_id, batch_id, and replay_id;
-- unsupported versions are rejected instead of silently degrading to v1.

ALTER TABLE replay_batches RENAME TO replay_batches_pre043;

CREATE TABLE IF NOT EXISTS replay_batches (
    tenant_id    TEXT NOT NULL DEFAULT 'default',
    site_id      TEXT NOT NULL,
    replay_id    TEXT NOT NULL,
    producer_id  TEXT NOT NULL DEFAULT '',
    batch_id     TEXT NOT NULL,
    event_count  BIGINT NOT NULL,
    payload_sha  TEXT NOT NULL,
    first_seen   BIGINT NOT NULL,
    version      BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, replay_id, producer_id, batch_id);

INSERT INTO replay_batches (
    tenant_id, site_id, replay_id, producer_id, batch_id,
    event_count, payload_sha, first_seen, version
)
SELECT
    tenant_id, site_id, replay_id, '', batch_id,
    event_count, payload_sha, first_seen, version
FROM replay_batches_pre043;
