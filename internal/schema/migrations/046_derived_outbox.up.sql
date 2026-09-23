-- 046 (2026-09-22): derived-work outbox (O01 ADR section 5.7, implementation
-- slice 3; closes the TO-020 class for the trace signal and gives R22's
-- durable webhook half its landing table).
--
-- Trace ingest used to commit spans and then run rollups + detectors in a
-- detached goroutine with no durable record: a crash between the span commit
-- and the derived write silently lost that derived work forever. The outbox
-- row is the durable intent - written INSIDE the same transaction as the
-- spans it describes (the same atomic-unit boundary replay_batches
-- established in 041 and error_inbox in 045), so intents can never orphan
-- ahead of their originating state, and originating state never lands
-- without its intents.
--
-- Row lifecycle:
--  * enqueue          - attempts=0, processed_at=0, payload = the exact
--    derived inputs frozen at ingest (for trace rollups: the per-bucket
--    aggregates; for trace detectors: the computed issues; re-derivation
--    from the frozen payload reproduces identical output, so the worker is
--    idempotent under at-least-once delivery).
--  * worker processes - marks processed_at by writing a new version
--    (strictly-monotonic version stamp, the version-tie convention).
--  * worker fails     - attempts+1, last_error, next_attempt_at = now +
--    exponential backoff; after the attempt budget the row is marked as a
--    DEAD LETTER durably (next_attempt_at = -1, a sentinel no clock ever
--    reaches) with last_error kept - loudly countable, never silently
--    dropped, and never retried again even if a later process runs with a
--    larger attempt budget.
--
-- ReplacingMergeTree keyed on the intent id: enqueue writes version once;
-- every worker state transition is a version-rewriting insert collapsed at
-- read time by the argMax form (internal/query/replacing.go). A plain
-- append table would need a reconciliation query over an unbounded number
-- of physical rows per intent; the replacing shape keeps one logical row.
--
-- Single-process boundary (the standing AUD-018 posture): the worker
-- serializes in-process; there is no cross-replica claim. Two replicas
-- would both drain due intents - derivation is idempotent, so the output
-- survives, but a multi-replica deployment needs a lease/CAS design before
-- webhook deliveries (external side effects) ride this table.
--
-- Retention: none yet - slice 5 (ADR section 5.8) gives processed intents an
-- explicit policy, same as replay_batches and error_inbox. Until then the
-- table grows with intent count (2 rows per trace export: one rollup, one
-- detector intent).
--
-- Kinds in flight at this migration: 'rollup' + 'detector' (trace ingest).
-- Reserved for the next slices: 'heatmap' (replay click rollups, TO-020's
-- replay half) and 'webhook' (alert-fire deliveries, R22's durable half -
-- receivers dedupe on the stable X-Observe-Delivery id the payload carries).

CREATE TABLE IF NOT EXISTS derived_outbox (
    tenant_id       TEXT NOT NULL DEFAULT 'default',
    id              TEXT NOT NULL,
    kind            TEXT NOT NULL,
    site_id         TEXT NOT NULL,
    payload         TEXT NOT NULL DEFAULT '',
    created_at      BIGINT NOT NULL,
    attempts        BIGINT NOT NULL DEFAULT 0,
    next_attempt_at BIGINT NOT NULL DEFAULT 0,
    processed_at    BIGINT NOT NULL DEFAULT 0,
    last_error      TEXT NOT NULL DEFAULT '',
    version         BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, id);
