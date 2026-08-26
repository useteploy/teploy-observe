-- 036 (2026-08-25): conversion goals carry a monetary value.
--
-- Goals could be counted but never valued, so Observe reported "412
-- conversions" where Plausible and Fathom report "412 conversions worth
-- $18,204". Four columns carry it:
--
--   value_minor     the money ONE conversion is worth, as an integer in the
--                   ISO-4217 minor units of `currency` (cents for USD, whole
--                   yen for JPY, thousandths for KWD). Never a float: money
--                   summed as float64 drifts, and 0.1 + 0.2 is the canonical
--                   demonstration. Formatting back to a decimal happens once,
--                   at the UI edge.
--   currency        ISO-4217 alphabetic code, uppercase. Empty means the goal
--                   carries no value. There is deliberately NO default of
--                   'USD': a self-hosted analytics tool has no business
--                   guessing that its operator bills in dollars.
--   value_source    'fixed'  — every conversion is worth value_minor.
--                   'event'  — each conversion event carries its own amount in
--                              an event property, so a $12 order and a $400
--                              order are counted at their real values.
--   value_property  the event property holding that amount when
--                   value_source = 'event' (conventionally 'revenue'). Ignored
--                   for 'fixed'.
--
-- Note this is NOT the existing `goal_value` column, which is the MATCHER —
-- the pathname or event_type a conversion is recognised by. The names sit
-- uncomfortably close together; `goal_value` was there first and renaming it
-- would break every existing read for no gain.
--
-- Rename-aside + create + copy, in the style of 027/028/033/034/035. ALTER
-- TABLE ADD COLUMN is not available: on a MergeTree table it makes subsequent
-- connections silently drop inserts (the bug 027 exists to repair), and
-- `ALTER TABLE IF EXISTS` errors because Nucleus resolves the table before it
-- consults the flag.
--
-- No IF EXISTS on the rename: `goals` is guaranteed to exist, created by 005.
--
-- Cost: `goals` holds one row per goal per site — tens of rows on the largest
-- install, 5 KB of table. The copy is a single grouped scan of that. This is
-- not a table-materialising migration in the sense of
-- docs/operations/issue-duplicate-collapse.md and does not approach the
-- 6144 MB production budget.
--
-- goals_pre036 is deliberately LEFT IN PLACE as a recovery artifact. Drop it
-- by hand once the copy is confirmed; a fresh install ends up with one empty
-- artifact, which is harmless.

ALTER TABLE goals RENAME TO goals_pre036;

CREATE TABLE IF NOT EXISTS goals (
    goal_id        TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    goal_type      TEXT NOT NULL DEFAULT 'page',
    goal_value     TEXT NOT NULL DEFAULT '',
    value_minor    BIGINT NOT NULL DEFAULT 0,
    currency       TEXT NOT NULL DEFAULT '',
    value_source   TEXT NOT NULL DEFAULT 'fixed',
    value_property TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    version        BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, goal_id);

-- argMax(col, version) grouped by the ORDER BY key is the only form that
-- collapses a Nucleus replacing table reliably; `FINAL` parses and is silently
-- ignored. Pre-036 goals were insert-once so duplicates are unlikely, but the
-- collapse costs nothing and the edit path added alongside this migration
-- writes new versions from here on.
INSERT INTO goals (
    goal_id, tenant_id, site_id, name, goal_type, goal_value,
    value_minor, currency, value_source, value_property, created_at, version
)
SELECT
    goal_id, tenant_id, site_id,
    argMax(name, version),
    argMax(goal_type, version),
    argMax(goal_value, version),
    0,
    '',
    'fixed',
    '',
    argMax(created_at, version),
    MAX(version)
FROM goals_pre036
GROUP BY tenant_id, site_id, goal_id;
