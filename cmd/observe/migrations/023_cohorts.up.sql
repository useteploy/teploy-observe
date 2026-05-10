-- C2 (Wave 4, 2026-05-10): cohorts metadata. Persons need no schema —
-- they're an aggregate over events.distinct_id (already added in
-- migration 018). This migration only stores the saved cohort
-- definitions and a cached member_count.
--
-- Engine: replacing_mergetree so updates / refreshes overwrite by
-- (tenant_id, site_id, cohort_id) on merge. Per finding #10/#27 the
-- read path dedups by PK in Go because read-time dedup is unreliable.
--
-- rule is JSON of the cohort rule DSL (see internal/cohorts).
-- updated_at is the version column so a refresh that bumps
-- member_count + updated_at wins on merge.

CREATE TABLE IF NOT EXISTS cohorts (
    cohort_id     TEXT NOT NULL,
    tenant_id     TEXT NOT NULL DEFAULT 'default',
    site_id       TEXT NOT NULL,
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    rule          TEXT NOT NULL,
    member_count  BIGINT NOT NULL DEFAULT 0,
    created_at    BIGINT NOT NULL,
    updated_at    BIGINT NOT NULL
) WITH (engine = 'replacing_mergetree', version_column = 'updated_at')
ORDER BY (tenant_id, site_id, cohort_id);
