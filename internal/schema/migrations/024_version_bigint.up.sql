-- 024 (2026-06-05): ReplacingMergeTree version columns TEXT -> BIGINT.
--
-- Every mutable table declared its replacing version column as
-- `version TEXT NOT NULL DEFAULT '0'`. Verified against the current Nucleus
-- engine (2026-06-05): newest-wins dedup orders the version column BY ITS
-- DECLARED TYPE, so a TEXT version compares LEXICALLY — argMax(val, version)
-- picks "2" over "10". With a BIGINT version it orders numerically and
-- newest-wins is correct. (The 001_analytics tables — sessions, stats_hourly,
-- stats_daily — were already BIGINT; performance_issues.last_seen and
-- cohorts.updated_at are already BIGINT and serve as their own version.)
--
-- ALTER COLUMN ... TYPE BIGINT rewrites the stored values (Nucleus dogfood D-7,
-- commit 05bb371): existing TEXT '0'/'<ts>' values cast cleanly to int64. The
-- matching CREATE TABLE statements in 002..021 were also changed to BIGINT so
-- fresh installs never create the TEXT column; this ALTER is then an idempotent
-- no-op on a fresh DB (BIGINT -> BIGINT) and the real migration on an existing
-- one (TEXT -> BIGINT).

ALTER TABLE issues ALTER COLUMN version TYPE BIGINT;
ALTER TABLE service_stats ALTER COLUMN version TYPE BIGINT;
ALTER TABLE service_dependencies ALTER COLUMN version TYPE BIGINT;
ALTER TABLE alert_rules ALTER COLUMN version TYPE BIGINT;
ALTER TABLE webhooks ALTER COLUMN version TYPE BIGINT;
ALTER TABLE goals ALTER COLUMN version TYPE BIGINT;
ALTER TABLE uptime_monitors ALTER COLUMN version TYPE BIGINT;
ALTER TABLE cron_monitors ALTER COLUMN version TYPE BIGINT;
ALTER TABLE dashboards ALTER COLUMN version TYPE BIGINT;
ALTER TABLE dashboard_panels ALTER COLUMN version TYPE BIGINT;
ALTER TABLE tracked_links ALTER COLUMN version TYPE BIGINT;
ALTER TABLE integrations ALTER COLUMN version TYPE BIGINT;
ALTER TABLE saved_views ALTER COLUMN version TYPE BIGINT;
ALTER TABLE report_schedules ALTER COLUMN version TYPE BIGINT;
ALTER TABLE feature_flags ALTER COLUMN version TYPE BIGINT;
ALTER TABLE experiments ALTER COLUMN version TYPE BIGINT;
ALTER TABLE surveys ALTER COLUMN version TYPE BIGINT;
ALTER TABLE groups ALTER COLUMN version TYPE BIGINT;
ALTER TABLE sso_configs ALTER COLUMN version TYPE BIGINT;
ALTER TABLE log_pipelines ALTER COLUMN version TYPE BIGINT;
ALTER TABLE click_heatmaps ALTER COLUMN version TYPE BIGINT;
ALTER TABLE boards ALTER COLUMN version TYPE BIGINT;
