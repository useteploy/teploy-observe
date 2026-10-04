-- 061 (2026-10-03): C3 - person properties, explicit alias/merge, erasure
-- tombstones. Implements the person-graph half of the identity ADR
-- (docs/IDENTITY_MODEL_ADR.md): persons stay an aggregate over
-- events.distinct_id; these tables ADD side data keyed by that same
-- person_key (the HMAC'd distinct_id, or the raw value when the site opted
-- in). Historical event rows are never rewritten (ADR D5).
--
-- All three tables are ReplacingMergeTree keyed on the entity, collapsed at
-- read time by the argMax-over-version form (internal/query/replacing.go),
-- written as strictly-monotonic new versions (the maintenance_windows
-- pattern). No row is ever DELETEd by the application: an erase or an
-- un-merge is a NEW VERSION (props blanked, active='false'); retention may
-- prune on the time column.
--
--   person_properties  identify(distinct_id, traits) traits. props is a
--                      bounded JSON object (<= 50 keys, scalar values
--                      <= 1 KiB each; enforced in internal/persons).
--   person_aliases     alias_key -> canonical_key, resolved forward at read
--                      time in Go. active='false' is an un-merge or an
--                      erasure cleanup tombstone.
--   person_tombstones  GDPR erasure marker. Insert-once per key; the person
--                      listing excludes tombstoned keys and property writes
--                      for them are refused. Events are NOT deleted (see the
--                      ADR for why).
--
-- Single-process boundary (the standing AUD-018 posture): the read-modify-
-- write of properties and the merge checks serialize in-process.
--
-- Comments pure ASCII (038 rule).

CREATE TABLE IF NOT EXISTS person_properties (
    tenant_id  TEXT NOT NULL DEFAULT 'default',
    site_id    TEXT NOT NULL,
    person_key TEXT NOT NULL,
    props      TEXT NOT NULL DEFAULT '{}',
    updated_at BIGINT NOT NULL,
    version    BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, person_key);

CREATE TABLE IF NOT EXISTS person_aliases (
    tenant_id     TEXT NOT NULL DEFAULT 'default',
    site_id       TEXT NOT NULL,
    alias_key     TEXT NOT NULL,
    canonical_key TEXT NOT NULL,
    active        TEXT NOT NULL DEFAULT 'true',
    created_by    TEXT NOT NULL DEFAULT '',
    created_at    BIGINT NOT NULL,
    version       BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, alias_key);

CREATE TABLE IF NOT EXISTS person_tombstones (
    tenant_id  TEXT NOT NULL DEFAULT 'default',
    site_id    TEXT NOT NULL,
    person_key TEXT NOT NULL,
    erased_by  TEXT NOT NULL DEFAULT '',
    erased_at  BIGINT NOT NULL,
    version    BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, person_key);
