# Collapsing duplicate rollup rows (migration 033)

## Do the existing rollups need a rebuild?

**Yes, on any instance that has been running before this release.** The code
fix stops new duplicates being written and makes reads correct in spite of the
old ones, so the dashboard is right the moment the new binary is deployed. But
the rows already on disk stay there:

- `stats_hourly` is pruned after 365 days, so its duplicates age out slowly.
- `sessions` is pruned after 90 days.
- **`stats_daily` has no retention policy at all.** Nothing would ever have
  removed its duplicates.

On the live instance at the time of the fix:

| table | rows | duplicated keys |
| --- | --- | --- |
| `stats_hourly` (one site) | 1956 | 740 |
| `stats_daily` (all sites) | 3221 | 871 |
| `sessions` (all sites) | 5479 | 347 |

The oldest un-collapsed duplicate bucket was two months old, which is the
evidence that Nucleus does not merge these tables on its own — there is no
`OPTIMIZE`, and `FINAL` parses but is silently ignored.

## The procedure

`internal/schema/migrations/033_collapse_rollup_duplicates.up.sql` does the
whole thing, and it runs automatically on the next start. It is non-destructive
in the style of 027/028:

1. `ALTER TABLE … RENAME TO …_pre033` for `stats_hourly`, `stats_daily` and
   `sessions`. No `IF EXISTS` — Nucleus resolves the table before it consults
   that flag, so `ALTER TABLE IF EXISTS` errors anyway; all three are
   guaranteed to exist from migration 001.
2. Recreate each table with its original DDL (`sessions` includes the
   `release_tag` column that 019 added).
3. `INSERT … SELECT argMax(col, version) … GROUP BY <ORDER BY key>` from the
   renamed table.

The renamed tables are **left in place deliberately**. A fresh install ends up
with three empty artifacts, which is harmless; an upgraded install ends up with
its pre-collapse rows still readable.

## Verify, then clean up

After the deploy, against the instance's own Nucleus:

```sql
-- Should be 0 on every table.
SELECT COUNT(*) FROM (
  SELECT COUNT(*) c FROM stats_hourly
  GROUP BY tenant_id, site_id, ts_bucket, pathname, event_type
  HAVING COUNT(*) > 1) t;

-- The dashboard number must equal what the raw events prove.
SELECT COUNT(*) AS pageviews FROM events
 WHERE site_id = '<site>' AND event_type = 'pageview' AND timestamp >= <from_ms>;
SELECT SUM(pageviews) FROM stats_hourly
 WHERE site_id = '<site>' AND event_type = 'pageview' AND ts_bucket >= <from_ms>;
```

Once both agree, drop the artifacts by hand:

```sql
DROP TABLE stats_hourly_pre033;
DROP TABLE stats_daily_pre033;
DROP TABLE sessions_pre033;
```

Do not fold that DROP into a later migration without thinking about the upgrade
path — the same mistake 027 originally made.

## What was proven, and where

- The collapse arithmetic was checked read-only against the live instance:
  a window whose raw events prove 72 pageviews read 158 with a bare `SUM` and
  exactly 72 with the `argMax` form. `FINAL` returned 158, i.e. it is a no-op.
- The migration itself was applied end to end on a scratch Nucleus v0.1.8
  (`ghcr.io/neutron-build/nucleus:v0.1.8`) on both paths: an upgrade from
  migration 032 with seeded rows, which reached version 33 with every row and
  value preserved and the three artifacts present, and a fresh 001-033 install.
- The `argMax` collapse was exercised against genuinely duplicated rows by
  building the source as a plain `mergetree` (which does not collapse): three
  rows for one key, bare `SUM` 15, collapse 7 at the highest version.

## One caveat worth knowing

Nucleus's own ReplacingMergeTree collapse **does not reliably keep the highest
version**. Seeded with `1@v500`, `8@v1000` and `8@v2000` for a single key on a
scratch instance, it kept the `1`. That is why the fix does not depend on the
engine at any point: the rollup jobs never write a duplicate key, and the read
path selects the version explicitly.
