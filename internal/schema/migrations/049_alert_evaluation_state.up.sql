-- 049 (2026-09-23): O10 - alert evaluation state as a persisted transition
-- ledger, plus the rule columns the gate needs.
--
-- CheckRules used to be stateless: each tick re-queried the metric and
-- re-fired on every breach bounded only by an alert_history cooldown count,
-- so a sustained fault produced one webhook blast per cooldown, recovery
-- closed nothing, and a window with no data read as "threshold not breached"
-- (silently passing - for error_rate it literally computed 0%).
--
-- alert_evaluations is the replacement truth: ONE APPEND-ONLY ROW PER RULE
-- EVALUATION carrying from_state -> to_state, the value, the sample count
-- and the incident the edge belongs to. The current state of a rule is the
-- to_state of its latest row. States:
--
--   healthy      a decision: samples >= minimum AND value in bounds
--   firing       a decision: samples >= minimum AND threshold breached
--   no_data      NOT a decision: the window has zero underlying data points,
--                or fewer than the rule's minimum samples. Distinct from
--                healthy on purpose - it is never silently treated as
--                passing, and it can neither open nor recover an incident.
--   unavailable  NOT a decision: the metric query itself failed (detail
--                keeps the error). Same non-decision posture as the flags
--                package's 'unavailable': fail open on nothing.
--
-- Notification-bearing edges (see 050 for the delivery half):
--   * -> firing with no open incident   -> opening notification + incident
--   firing -> healthy (or a healthy decision while an incident is open)
--                                      -> recovery notification + close
--   firing -> firing past the cooldown  -> repeat notification
-- Hysteresis comes from the edges, not from a timer: the firing transition
-- is re-armed only after a recovery decision.
--
-- Plain mergetree, append-only, no collapse: the ledger IS the history.
-- Current-state reads use ORDER BY evaluated_at DESC, eval_id DESC
-- LIMIT 1 per rule (both output names, the Nucleus ORDER BY rule).
--
-- alert_rules gains:
--   min_samples  minimum underlying data points before the rule may decide
--                (TEXT to match the table's other numeric config columns;
--                '1' backfills every existing rule - one data point was the
--                previous implicit gate).
--   severity     carried onto the incident and the notification payload
--                ('warning' backfill matches what the auto-declare path
--                already hardcoded).
--
-- ADD COLUMN IF NOT EXISTS on existing tables follows the 048 precedent.
-- Comments pure ASCII (038 rule).

ALTER TABLE alert_rules ADD COLUMN IF NOT EXISTS min_samples TEXT NOT NULL DEFAULT '1';
ALTER TABLE alert_rules ADD COLUMN IF NOT EXISTS severity TEXT NOT NULL DEFAULT 'warning';

CREATE TABLE IF NOT EXISTS alert_evaluations (
    eval_id      TEXT NOT NULL,
    tenant_id    TEXT NOT NULL DEFAULT 'default',
    rule_id      TEXT NOT NULL,
    site_id      TEXT NOT NULL,
    evaluated_at BIGINT NOT NULL,
    from_state   TEXT NOT NULL DEFAULT '',
    to_state     TEXT NOT NULL,
    value        TEXT NOT NULL DEFAULT '0',
    threshold    TEXT NOT NULL DEFAULT '0',
    samples      BIGINT NOT NULL DEFAULT 0,
    detail       TEXT NOT NULL DEFAULT '',
    incident_id  TEXT NOT NULL DEFAULT ''
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, rule_id, evaluated_at);
