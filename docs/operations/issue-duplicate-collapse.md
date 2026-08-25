# Collapsing duplicate `issues` rows (migration 034)

## What happened

`issues` is a ReplacingMergeTree keyed on `(tenant_id, site_id, issue_id)`, and
every error batch rewrote its row with

```sql
INSERT INTO issues (...) SELECT ... FROM issues WHERE issue_id = $1
```

That writes one row per row the SELECT returns. It is only harmless if the
engine collapses the superseded versions before the SELECT sees them, and
Nucleus does not — see *Why the engine does not collapse* below. So each bump
inserted as many rows as already existed and the physical count **doubled**.

Reproduced on a scratch Nucleus v0.1.8, one issue, twelve bumps:

| bumps | 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 | 9 | 10 | 11 | 12 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| rows | 2 | 4 | 8 | 16 | 32 | 64 | 128 | 256 | 512 | 1024 | 2048 | 4096 |

`UpdateStatus` had the identical shape.

On the live instance, `issues` holds **16,847,389 rows across 29 issues** —
every count an exact power of two:

| issue_id | rows |
| --- | --- |
| `60693312f561a5d91e030ace177f6a2c` | 16,777,216 (2^24) |
| `483d9f2de7831080a3f0054d68ff275c` | 65,536 (2^16) |
| `939a1e65ee612a776d2787e9f5365b65` | 4,096 (2^12) |
| `77db90a005336ad8c6a7be42cd0dc9c8` | 256 |
| `01c091c24eda8700ff7903fc9c17797c` | 128 |
| two more | 64 each |
| the remaining 22 | 4 or fewer |

## Why the engine does not collapse

A replacing table's read-time dedup is not a property of the stored table. It
is an entry in a process-global registry that Nucleus populates when it
*executes* `CREATE TABLE`, and repopulates on restart only for the tables
listed in the data directory's `engines.json`. Tables created before that file
existed are absent from it, so after the next restart they read as plain
MergeTrees and every version comes back.

The live instance's `engines.json` lists **exactly one table** (`audit_events`).
A fresh install of this release writes 61. That is the whole difference between
"the bug is invisible on a scratch box" and "the live table has 16.8M rows",
and it is why `internal/nucleustest.AsPlainMergeTree` exists: an integration
test on a freshly created table proves nothing, because the registry is warm
and the buggy and fixed code agree.

`FINAL` parses but is silently ignored, and the engine's own merge does not
honour the version column. `argMax(col, version)` grouped by the declared
ORDER BY key is the only form that works.

## The fix in the code

`internal/errors/issues.go` reads through the collapse on both sides:

- Every read — `ListIssues`, `GetIssue`, `findIssueByHash`, the full-text
  search hydration in `search.go` — selects `argMax(col, version)` grouped by
  the ORDER BY key. `status` changes between versions, so it is filtered
  **after** the collapse, never inside it.
- `bumpIssue` and `UpdateStatus` read through the same collapse before
  inserting, so exactly one row is written however many versions exist.
  Verified on scratch: against a key that already had 4096 rows, the fixed
  shape wrote 1.

New duplicates therefore stop the moment the binary is deployed, and reads are
correct in spite of the old ones. Migration 034 repairs the rows on disk.

## The migration

`internal/schema/migrations/034_collapse_issue_duplicates.up.sql`, in the style
of 027/028/033 and non-destructive:

1. `ALTER TABLE issues RENAME TO issues_pre034`. No `IF EXISTS` — Nucleus
   resolves the table before it consults that flag, so `ALTER TABLE IF EXISTS`
   errors anyway; `issues` is guaranteed to exist from 002.
2. Recreate `issues` with its original DDL (BIGINT `version`, per 024).
3. `INSERT … SELECT argMax(col, version) … GROUP BY tenant_id, site_id,
   issue_id` from the renamed table.

`issues_pre034` is **left in place deliberately**. The rename does not copy
data, so the artifact costs no extra disk; the new table holds only the
collapsed rows. A fresh install ends up with one empty artifact.

It runs automatically on the next start, inside a single transaction, before
the HTTP listeners bind. A failure rolls the whole thing back — there is no
half-copied state and no data loss — but the process exits, so on a container
that means a restart loop rather than a broken table.

### Read this before deploying to a large instance

**On the live instance the migration will fail as-is.** Nucleus caps a single
query's working set at `query_memory_percent` (75) of `server.max_memory_mb`,
and the collapse materialises every row it groups over. Measured on a scratch
Nucleus v0.1.8 against a table of realistic issue shape:

| rows | budget | result |
| --- | --- | --- |
| 200,000 | 384 MB | collapses in ~2 s |
| 588,000 | 384 MB | collapses in ~4 s |
| 610,000 | 288 MB | rejected |
| 610,000 | 144 MB | rejected |
| 16,847,389 (live, read-only) | 6144 MB | rejected |

That brackets the working set at roughly **500-650 bytes per row**, so the live
table needs somewhere around **10 GB** and the accessory currently allows
6144 MB.

Chunking the copy does not help here: one issue is 16,777,216 of the
16,847,389 rows, so no predicate on `issue_id` splits it. A two-stage form that
filters on the surviving versions was tried and abandoned — `WHERE version IN
(SELECT …)` panics the Nucleus connection (`there is no reactor running, must
be called from the context of a Tokio 1.x runtime`,
`src/executor/session.rs:106`). **Do not put an `IN (subquery)` in a
migration.**

So, before deploying this release to `infra-home` — which has 29 GB of RAM,
about 21 GB of it available, and caps `observe-nucleus` at 10 GB:

```bash
# 1. Give the accessory headroom for the one start that runs 034. The query
#    budget is 75% of NUCLEUS_MAX_MEMORY_MB, so 16384 -> 12288 MB, comfortably
#    above the ~10 GB estimate. The container's own cap must be above that or
#    the kernel OOM-kills it before Nucleus's limiter fires: raise it to 20g
#    for the same window.
#      accessory memory cap: 10g -> 20g
#      NUCLEUS_MAX_MEMORY_MB=16384

# 2. Deploy. The start that applies 034 takes noticeably longer than a normal
#    one — budget minutes, and nothing serves traffic until it finishes.
teploy logs observe -f

# 3. Verify (below), drop the artifact, then put BOTH limits back to 10g and
#    the default NUCLEUS_MAX_MEMORY_MB. Leaving a 20 GB cap on this accessory
#    on a 29 GB host is not a state to walk away from.
```

If the migration is rejected anyway, the transaction rolls back and the process
exits, so the container restarts and retries — raise the budget again (double
it) or use the by-hand procedure below. Nothing is lost either way.

### The by-hand alternative DOES NOT WORK — attempted live 2026-08-25

**Do not try this. It was attempted end to end on the live instance and every
form of it was rejected.** Recorded here so nobody repeats it.

The procedure below assumed a targeted read of one row is cheap. It is not,
and the reason is the rename itself: `ALTER TABLE issues RENAME TO
issues_pre034` leaves the renamed table with no engine registration, so it is a
plain heap with no ORDER BY pruning, and Nucleus materialises **every one of
the 16.8M rows** before it applies any predicate. Measured against the live
instance, whose real budget is **6144 MB** (`NUCLEUS_MAX_MEMORY_MB=8192`, 75%),
not the 10 GB container cap:

| statement | result |
| --- | --- |
| `SELECT COUNT(*)` | works, streams |
| `SELECT tenant_id, site_id, issue_id, MAX(version) … GROUP BY` | works, streams |
| `SELECT …14 cols… WHERE tenant_id=… AND site_id=… AND issue_id=… AND version=… LIMIT 1` | **rejected**, working set > 6144 MB |
| `SELECT tenant_id, site_id, issue_id, argMax(title, version) … GROUP BY` (ONE value column) | **rejected**, > 6144 MB |
| `INSERT INTO issues … SELECT … WHERE <key> AND version=… LIMIT 1` | pinned nucleus at 9.998 GiB of its 10 GiB cap; killed before the kernel did |
| `SELECT … WHERE version IN (29 literals)` | **crashed the server** — connection lost, container restarted, WAL replayed cleanly, no data lost |

So only cheap single-column aggregates stream. Anything that returns row
content — even one row — does not. There is no by-hand form that fits.

**Rolling back is clean and is what was done:** `DROP TABLE issues;` (the empty
recreated one) then `ALTER TABLE issues_pre034 RENAME TO issues;`. The row
count then climbs for a few minutes as segments reload — it read 11.3M, then
14.0M, before settling at exactly 16,847,389. Wait for it to settle before
concluding anything. Migration bookkeeping was never touched, so the database
was left at 32 and identical to its starting state.

**Consequence: raising the memory budget is MANDATORY, not optional**, and
**deploying this release is BLOCKED until it is done** — the deploy runs 034,
034 exceeds the budget, the migration fails, the process exits, and the
container crash-loops. Follow the raise-the-limit procedure above.

### The original by-hand text, retained only so it is not attempted again

#### If you would rather not raise the limit

There are only 29 issues, so the collapse can be done by hand in a form whose
working set is one row at a time. Against the instance's own Nucleus, with the
app stopped so nothing writes underneath you:

```sql
-- 1. The keys and their surviving version. A single-column aggregate over the
--    whole table is cheap — this one returns in well under the limit.
SELECT tenant_id, site_id, issue_id, MAX(version) AS v
  FROM issues GROUP BY tenant_id, site_id, issue_id;

-- 2. Rename aside and recreate, exactly as the migration does.
ALTER TABLE issues RENAME TO issues_pre034;
--    (paste the CREATE TABLE from the migration here)

-- 3. One statement per key, with the version from step 1 substituted in.
INSERT INTO issues (issue_id, tenant_id, site_id, group_hash, title, culprit,
                    level, status, first_seen, last_seen, event_count,
                    user_count, release_tag, version)
SELECT issue_id, tenant_id, site_id, group_hash, title, culprit, level, status,
       first_seen, last_seen, event_count, user_count, release_tag, version
  FROM issues_pre034
 WHERE tenant_id = '…' AND site_id = '…' AND issue_id = '…' AND version = …
 LIMIT 1;

-- 4. Tell the migration runner it is done, so the next start does not redo it.
INSERT INTO _neutron_migrations (version, name)
VALUES (34, '034_collapse_issue_duplicates');
```

Step 4 is what makes this safe to combine with the deploy: 034 is skipped
because it is already recorded.

## Verify

```sql
-- One row per key. Should be 0.
SELECT COUNT(*) FROM (
  SELECT COUNT(*) c FROM issues
  GROUP BY tenant_id, site_id, issue_id
  HAVING COUNT(*) > 1) t;

-- 29 on the live instance, and the row count should equal it.
SELECT COUNT(*) FROM issues;

-- The artifact still has everything.
SELECT COUNT(*) FROM issues_pre034;
```

Then check the dashboard: the issue list, an issue's status, and that
resolving an issue sticks. Once satisfied:

```sql
DROP TABLE issues_pre034;
```

Do not fold that DROP into a later migration without thinking about the upgrade
path — the same mistake 027 originally made.

## What was proven, and where

- The doubling was reproduced on a scratch Nucleus v0.1.8 by recreating
  `issues` without its replacing registration (which is the live instance's
  actual state): 1 → 4096 over twelve bumps. The fixed insert shape then wrote
  exactly one row against the same 4096-row key.
- The live numbers above are read-only queries against `infra-home`'s
  `observe-nucleus`. Nothing was written there.
- Migration 034 was applied end to end on a scratch Nucleus v0.1.8 on both
  paths: an **upgrade** from migration 033 with eight seeded rows across three
  keys — including a key whose newest version was `resolved` and whose older
  versions were `open` — which reached version 34 with three rows, each
  carrying the highest-version value of every column, and `issues_pre034`
  holding all eight; and a **fresh install**, which reached version 34 with
  `issues` and `issues_pre034` both empty and no errors in the log.
- The regression tests are in `internal/errors/issues_duplicates_test.go`.
  With `internal/errors/issues.go` reverted they fail with "after 5 bumps:
  want 6 rows, got 32" and "find by hash returned last_seen 2001, want 2003".
