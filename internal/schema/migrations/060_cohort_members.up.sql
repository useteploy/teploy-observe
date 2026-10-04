-- 060 (2026-10-03): C2 - static cohort membership.
--
-- A rule cohort (023) re-evaluates its definition on every use. A static
-- cohort is an explicit list of entity ids (distinct_id values) the caller
-- supplied - a bulk create, a CSV import, or add/remove edits. Its cohorts
-- row carries the rule {"op":"static"}; the members live here.
--
--   cohort_id, site_id   the owning cohort. Every read filters on BOTH, so
--                        a cohort id from another site matches nothing.
--   entity_id            the member (a distinct_id), at most 256 bytes
--   added_at             epoch ms the entity first joined this cohort; kept
--                        across later versions of the same row
--   removed              0 = member, 1 = tombstone (member removed). Hard
--                        DELETE is unreliable across replacing merges in
--                        Nucleus, so removal is a newer version with
--                        removed = 1 (same pattern as the cohorts tombstone)
--   version              epoch ms of the mutation batch that wrote the row;
--                        strictly increasing per cohort (the service takes
--                        max(now, cohort.updated_at + 1)), so the highest
--                        version per (cohort, entity) is the current state
--
-- ReplacingMergeTree keyed on the member, versioned on version. The read
-- path collapses versions in Go (highest version wins) because read-time
-- dedup is unreliable (finding #10) and reads are bounded by a row LIMIT.
--
-- Comments pure ASCII (038 rule).

CREATE TABLE IF NOT EXISTS cohort_members (
    cohort_id   TEXT NOT NULL,
    tenant_id   TEXT NOT NULL DEFAULT 'default',
    site_id     TEXT NOT NULL,
    entity_id   TEXT NOT NULL,
    added_at    BIGINT NOT NULL,
    removed     BIGINT NOT NULL DEFAULT 0,
    version     BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, cohort_id, entity_id);
