-- 034 (2026-08-25): collapse the accumulated duplicate rows in `issues`.
--
-- issues is a ReplacingMergeTree keyed on (tenant_id, site_id, issue_id), and
-- every error batch rewrote its row. The rewrite was
--
--     INSERT INTO issues (...) SELECT ... FROM issues WHERE issue_id = $1
--
-- which inserts one row per row the SELECT returns. Nucleus does not reliably
-- collapse a replacing table's superseded versions — its read-time dedup is a
-- process-global registry populated by CREATE TABLE and restored on restart
-- only for tables listed in the data directory's engines.json, and every table
-- older than that file is absent from it — so the SELECT saw every version and
-- the physical row count DOUBLED on each bump. Measured on a scratch Nucleus
-- v0.1.8: 1, 2, 4, 8 ... 4096 over twelve bumps. On the live instance `issues`
--
-- That "..." is deliberately ASCII. A non-ASCII character followed by a NUMBER
-- inside a `--` comment panics the Nucleus v0.1.8 SQL lexer and drops the
-- connection ("unexpected EOF"), which made this migration fail on every fresh
-- install and would have crash-looped the next deploy of the live instance.
-- `... 4096` is fine, `<multi-byte char> 4096` is not; the same character
-- followed by a word is harmless, which is why 033 survives its em dashes.
-- TestMigrationsAvoidNucleusLexerPanic pins this.
-- reached 16,847,389 rows, essentially all of them one issue at 2^24.
--
-- UpdateStatus had the same shape, and every read of the table took an
-- arbitrary version, so a resolved issue could keep reading as open.
--
-- internal/errors/issues.go now reads through the argMax collapse on both the
-- read and the write side, so no new duplicates are written and reads are
-- correct in spite of the old ones. This migration repairs the rows on disk.
--
-- Non-destructive, in the style of 027/028/033: rename aside, recreate, copy
-- the highest-version row per ORDER BY key across. `issues_pre034` is LEFT IN
-- PLACE as a recovery artifact — the rename does not copy data, so it costs no
-- extra disk; drop it by hand once the copy is confirmed. A fresh install ends
-- up with one empty artifact, which is harmless.
--
-- No IF EXISTS on the rename: Nucleus resolves the table before it consults
-- that flag, so ALTER TABLE IF EXISTS errors anyway. `issues` is guaranteed to
-- exist from 002.
--
-- RUNTIME AND MEMORY — READ docs/operations/issue-duplicate-collapse.md BEFORE
-- DEPLOYING THIS TO A LARGE INSTANCE.
--
-- This runs automatically on the next start, inside one transaction, before
-- the HTTP listeners bind: the container looks wedged while it works and
-- nothing serves traffic until it finishes. Budget MINUTES, not seconds, on a
-- table of the live instance's size. A failure exits the process, so on a
-- container that is a restart loop.
--
-- It does NOT roll cleanly back. An earlier version of this note claimed it
-- did; that is wrong and was verified wrong against Nucleus v0.1.8. DDL is not
-- transactional there: `BEGIN; ALTER TABLE t RENAME TO t2; ROLLBACK;` leaves
-- the table renamed. So if the copy fails, `issues` is the new EMPTY table and
-- every row is in `issues_pre034`. No data is lost, but the errors UI reads
-- empty until you recover, and the restart will fail differently because the
-- rename target already exists. Recovery: DROP the empty `issues`, rename
-- `issues_pre034` back, and re-run with more memory (or use the by-hand
-- procedure in the runbook).
--
-- The collapse materialises every row it groups over, and Nucleus caps a
-- single query's working set at 75% of server.max_memory_mb. Measured: 200,000
-- rows of realistic issue shape collapse in ~2s inside a 384 MB budget, and
-- the same statement run read-only against the live 16,847,389-row table is
-- rejected at the accessory's 6144 MB — roughly 375-400 bytes per row, so that
-- table needs about 6.5 GB. Raise NUCLEUS_MAX_MEMORY_MB (and the container's
-- own cap above it) for the one start that applies this, or use the by-hand
-- procedure in the runbook.
--
-- Chunking the copy does not help on that instance: one issue accounts for
-- 16,777,216 of the 16,847,389 rows, so no predicate on issue_id splits it.
-- A two-stage form filtering on the surviving versions was tried and
-- abandoned: `WHERE version IN (SELECT ...)` panics the Nucleus connection
-- (src/executor/session.rs:106, "there is no reactor running"). Do not put an
-- IN (subquery) in a migration.

ALTER TABLE issues RENAME TO issues_pre034;

-- issues as 002 declares it, with the BIGINT version 024 converted it to.
CREATE TABLE IF NOT EXISTS issues (
    issue_id       TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    group_hash     TEXT NOT NULL,
    title          TEXT NOT NULL DEFAULT '',
    culprit        TEXT NOT NULL DEFAULT '',
    level          TEXT NOT NULL DEFAULT 'error',
    status         TEXT NOT NULL DEFAULT 'open',
    first_seen     TEXT NOT NULL,
    last_seen      TEXT NOT NULL,
    event_count    TEXT NOT NULL DEFAULT '1',
    user_count     TEXT NOT NULL DEFAULT '0',
    release_tag    TEXT NOT NULL DEFAULT '',
    version        BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, issue_id);

-- argMax(col, version) grouped by the ORDER BY key is the only form that
-- collapses correctly here: `FINAL` parses but is silently ignored, and the
-- engine's own merge does not honour the version column.
INSERT INTO issues (
    issue_id, tenant_id, site_id, group_hash, title, culprit, level, status,
    first_seen, last_seen, event_count, user_count, release_tag, version
)
SELECT
    issue_id,
    tenant_id,
    site_id,
    argMax(group_hash, version),
    argMax(title, version),
    argMax(culprit, version),
    argMax(level, version),
    argMax(status, version),
    argMax(first_seen, version),
    argMax(last_seen, version),
    argMax(event_count, version),
    argMax(user_count, version),
    argMax(release_tag, version),
    MAX(version)
FROM issues_pre034
GROUP BY tenant_id, site_id, issue_id;
