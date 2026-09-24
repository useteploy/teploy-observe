-- 054 (2026-09-24): O14 - LLM cost honesty: versioned model/cost catalog +
-- token/cost provenance.
--
-- Programme O14: "LLM traces need versioned model/cost catalogs, token-
-- source provenance ... estimated cost must say estimated."
--
-- llm_model_prices is the versioned cost catalog. Prices were a compiled-
-- in Go table (still the seed source + fallback), which meant every price
-- correction rode a binary release and no operator could pin what their
-- deployment actually believed. Rows resolve by:
--
--   provider      '' matches any provider; a provider-specific row wins
--                 over an provider-agnostic one for the same prefix.
--   model_prefix  longest-prefix match, most specific first (real model
--                 strings are dated: 'gpt-4o-2024-08-06' must price as
--                 gpt-4o, not fall to the unknown-model default).
--   valid_from    the VERSIONING axis: rows are effective from a unix-ms
--                 timestamp; a resolution picks the newest effective row
--                 per (provider, model_prefix). A price change lands as a
--                 new valid_from row - history is never rewritten, so a
--                 cost computed last month is reproducible.
--
-- input_per_1k / output_per_1k are USD per 1K tokens (DOUBLE, scanned
-- natively); source records where the number came from ('builtin' for the
-- seeded compiled table, or an operator note).
--
-- ReplacingMergeTree keyed on (provider, model_prefix, valid_from): an
-- operator corrects a row by writing the same key at a higher version -
-- the argMax collapse keeps one logical row per effective price.
--
-- llm_traces gains provenance (ADD COLUMN IF NOT EXISTS, the 048/051
-- precedent; '' backfills existing rows as legacy-unattributed):
--
--   token_source  'reported' (counts arrived in the ingest payload - the
--                 default when tokens are present and the producer said
--                 nothing) | 'estimated' (the PRODUCER derived the counts,
--                 e.g. chars/4 - the producer says so; the server never
--                 invents token counts) | '' (legacy rows).
--   cost_source   'reported' (caller supplied cost_usd) | 'estimated'
--                 (derived from the catalog at ingest) | '' (legacy rows:
--                 zero cost, nothing derivable).
--
-- Every read surface that sums cost_usd also reports the estimated subset
-- (stats/model breakdown/recent traces carry both fields), so an
-- "estimated" number can never render unlabeled next to a reported one.
--
-- Comments pure ASCII (038 rule).

CREATE TABLE IF NOT EXISTS llm_model_prices (
    provider      TEXT NOT NULL DEFAULT '',
    model_prefix  TEXT NOT NULL,
    input_per_1k  DOUBLE NOT NULL DEFAULT 0,
    output_per_1k DOUBLE NOT NULL DEFAULT 0,
    currency      TEXT NOT NULL DEFAULT 'usd',
    valid_from    BIGINT NOT NULL DEFAULT 0,
    source        TEXT NOT NULL DEFAULT '',
    created_at    BIGINT NOT NULL,
    tenant_id     TEXT NOT NULL DEFAULT 'default',
    version       BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, provider, model_prefix, valid_from);

ALTER TABLE llm_traces ADD COLUMN IF NOT EXISTS token_source TEXT NOT NULL DEFAULT '';
ALTER TABLE llm_traces ADD COLUMN IF NOT EXISTS cost_source TEXT NOT NULL DEFAULT '';
