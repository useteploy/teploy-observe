-- 047 (2026-09-23): O05 slice 1 - versioned fingerprints and the honest
-- issue lifecycle. issues gains four columns:
--
--   fingerprint_version  the grouping-derivation version the issue was
--                        created under. '1' for every pre-existing row
--                        (the v1 derivation is pinned by golden fixture
--                        tests in internal/errors). A future v2 lands as
--                        a new derivation function plus a documented
--                        cutover; existing rows KEEP their recorded
--                        version and are never rewritten - no history
--                        rewrite.
--   first_regression_at  unix-ms-as-text of the first event that
--                        reopened a resolved issue ('' = never
--                        regressed / not tracked for pre-047 history).
--   regression_count     how many times a resolved issue was reopened by
--                        a new event ('0' for pre-047 rows - honest
--                        default, history was not tracked).
--   snooze_until         a resolved issue may carry a snooze deadline
--                        (unix-ms-as-text, '' = none). New events during
--                        the window reopen the issue immediately; once
--                        the deadline passes with no new events the
--                        issue reads as plain resolved again (evaluation
--                        is lazy, at read time - no background timer).
--
-- Semantics live in internal/errors/issues.go bumpIssue/UpdateStatus:
--   - new event on a RESOLVED issue -> status 'open', regression markers
--     bumped, snooze cleared.
--   - new event on an IGNORED issue -> stays ignored (deliberate; the
--     operator said stop bothering me).
--   - new event on an OPEN issue -> stays open, no regression marker.
--
-- first_seen/last_seen are INGESTION-time (the pinned truth: the error
-- wire protocol carries no client event-time, same as analytics O03
-- D8). The bump clamps first_seen down (LEAST) and last_seen up
-- (GREATEST), so an out-of-order apply (PENDING retry, WAL replay) can
-- widen the window but never regress it.
--
-- Rename-aside + create + copy, the 027/028/034 pattern; argMax collapse
-- keyed on the ORDER BY key, MAX(version) carried. issues_pre047 stays
-- in place as the recovery artifact (the rename does not copy data). A
-- fresh install ends with an empty artifact. No IF EXISTS on the rename
-- (Nucleus resolves the table before consulting the flag; issues is
-- guaranteed to exist from 002).
--
-- Comments deliberately ASCII: a multi-byte character followed by a
-- number inside a -- comment panics the Nucleus v0.1.8 SQL lexer
-- (034's trap, pinned by TestMigrationsAvoidNucleusLexerPanic).

ALTER TABLE issues RENAME TO issues_pre047;

CREATE TABLE IF NOT EXISTS issues (
    issue_id            TEXT NOT NULL,
    tenant_id           TEXT NOT NULL DEFAULT 'default',
    site_id             TEXT NOT NULL,
    group_hash          TEXT NOT NULL,
    title               TEXT NOT NULL DEFAULT '',
    culprit             TEXT NOT NULL DEFAULT '',
    level               TEXT NOT NULL DEFAULT 'error',
    status              TEXT NOT NULL DEFAULT 'open',
    first_seen          TEXT NOT NULL,
    last_seen           TEXT NOT NULL,
    event_count         TEXT NOT NULL DEFAULT '1',
    user_count          TEXT NOT NULL DEFAULT '0',
    release_tag         TEXT NOT NULL DEFAULT '',
    fingerprint_version TEXT NOT NULL DEFAULT '1',
    first_regression_at TEXT NOT NULL DEFAULT '',
    regression_count    TEXT NOT NULL DEFAULT '0',
    snooze_until        TEXT NOT NULL DEFAULT '',
    version             BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, issue_id);

INSERT INTO issues (
    issue_id, tenant_id, site_id, group_hash, title, culprit, level, status,
    first_seen, last_seen, event_count, user_count, release_tag,
    fingerprint_version, first_regression_at, regression_count, snooze_until, version
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
    '1',
    '',
    '0',
    '',
    MAX(version)
FROM issues_pre047
GROUP BY tenant_id, site_id, issue_id;
