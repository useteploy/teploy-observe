-- 055 (2026-09-24): O14 - audit checkpoint digests, the path from
-- "internally consistent" to a truncation-detectable claim.
--
-- The programme's exact point: an append-only hash chain in one mutable
-- database does not prove that its tail was not truncated. A database-
-- level attacker who deletes the tail (and the checkpoint rows with it)
-- leaves a shorter chain that still verifies perfectly. F47 (external
-- WORM anchor) stays the full fix; this table is the cheap, honest middle
-- step the O14 spec names: an EXPORTED checkpoint digest an operator can
-- store outside the database.
--
-- audit_checkpoints is append-only (mergetree - rows are never rewritten,
-- matching the chain they describe). One row per checkpoint:
--
--   seq        the chain head's sequence number at checkpoint time
--   head_hash  the chain head's hash (the value row `seq` must hash to)
--   digest     HMAC-SHA256(signing key, "observe-audit-checkpoint\n" ||
--              seq || "\n" || head_hash) - hex. The signing key lives on
--              the observe host (OBSERVE_AUDIT_KEY / data/audit.key), NOT
--              in the database being protected, so a DB-level attacker
--              cannot forge a digest that matches one stored externally.
--   key_id     which key material signed it (F46 rotation: an old key's
--              checkpoints stay verifiable through the keyring)
--
-- How the guarantee composes: the operator ships the digest line out of
-- band (log pipeline, ticket, paper). Later - after any incident - they
-- recompute the digest from the CURRENT database's row at `seq` and
-- compare with the stored artifact. A match proves the prefix through
-- `seq` is unchanged AND that the chain reached at least `seq` (a
-- truncation that removed the checkpointed rows cannot reproduce the
-- digest). Verify() additionally checks the in-database copy (chain hash
-- at the checkpoint seq equals head_hash) and reports the checkpoint in
-- its result, with wording narrowed to what each layer actually proves.
--
-- Checkpoints are written (a) automatically every N records (default
-- 1000) - the log line carries the exportable digest - and (b) on demand
-- via POST /api/v1/audit/checkpoint (admin), which returns the checkpoint.
--
-- Comments pure ASCII (038 rule).

CREATE TABLE IF NOT EXISTS audit_checkpoints (
    checkpoint_id TEXT NOT NULL,
    tenant_id     TEXT NOT NULL DEFAULT 'default',
    seq           BIGINT NOT NULL,
    head_hash     TEXT NOT NULL,
    digest        TEXT NOT NULL,
    key_id        TEXT NOT NULL DEFAULT '',
    created_at    BIGINT NOT NULL
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, seq, created_at);
