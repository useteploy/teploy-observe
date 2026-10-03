-- 057 (2026-10-03): WP1.1 - persist OTLP span links.
--
-- Links (a span's causal references to spans in the same or another trace:
-- batch consumers, fan-in, retries) were parsed by no ingest path and so were
-- silently dropped. span_links stores them, one row per (owning span, link).
--
--   trace_id, span_id            the OWNING span (joins to spans)
--   link_idx                     position of the link on the span, so two
--                                links to the same target stay distinct
--   linked_trace_id/_span_id     the link target, lowercase hex as on spans
--   trace_state                  W3C tracestate of the target, may be empty
--   attributes                   JSONB string map, NULL when none
--   start_time                   the OWNING span's start (epoch ms): the
--                                retention column, so links age out with the
--                                spans they annotate
--
-- Ingest caps links per span and attributes per link; see links.go.
-- Mirrors the spans table in 003: mergetree, site_id leading the ordering.

CREATE TABLE IF NOT EXISTS span_links (
    trace_id         TEXT NOT NULL,
    span_id          TEXT NOT NULL,
    link_idx         BIGINT NOT NULL DEFAULT 0,
    tenant_id        TEXT NOT NULL DEFAULT 'default',
    site_id          TEXT NOT NULL,
    linked_trace_id  TEXT NOT NULL DEFAULT '',
    linked_span_id   TEXT NOT NULL DEFAULT '',
    trace_state      TEXT NOT NULL DEFAULT '',
    attributes       JSONB,
    start_time       BIGINT NOT NULL
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, start_time, trace_id, span_id, link_idx);
