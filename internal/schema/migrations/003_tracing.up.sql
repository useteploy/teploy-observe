-- Observe v0.3 APM / Tracing Schema
-- OTLP-shaped span storage with RED metrics rollups.
-- All numeric columns use TEXT to match Nucleus pgwire conventions.

-- ============================================================================
-- Spans (raw trace data)
-- ============================================================================
-- Each span represents a unit of work. OTLP-shaped schema:
-- trace_id + span_id uniquely identify a span, parent_span_id links the tree.
-- Attributes stored as JSONB for flexible querying.
-- ORDER BY optimized for trace_id lookups (waterfall) and time-range scans.

CREATE TABLE IF NOT EXISTS spans (
    trace_id       TEXT NOT NULL,
    span_id        TEXT NOT NULL,
    parent_span_id TEXT NOT NULL DEFAULT '',
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    service_name   TEXT NOT NULL DEFAULT '',
    operation_name TEXT NOT NULL DEFAULT '',
    span_kind      TEXT NOT NULL DEFAULT 'internal',
    start_time     BIGINT NOT NULL,
    end_time       BIGINT NOT NULL,
    duration_ms    BIGINT NOT NULL DEFAULT 0,
    status_code    TEXT NOT NULL DEFAULT 'unset',
    status_message TEXT NOT NULL DEFAULT '',
    attributes     JSONB,
    resource       JSONB,
    events         JSONB
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, start_time, trace_id, span_id);

-- ============================================================================
-- Service stats (RED metrics per service + operation)
-- ============================================================================
-- Pre-computed at ingest time. ReplacingMergeTree with version column
-- for idempotent updates. One row per (service, operation, hourly bucket).
-- RED = Rate (request_count), Errors (error_count), Duration (duration_sum for avg).

CREATE TABLE IF NOT EXISTS service_stats (
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    service_name   TEXT NOT NULL,
    operation_name TEXT NOT NULL DEFAULT '',
    ts_bucket      BIGINT NOT NULL,
    request_count  TEXT NOT NULL DEFAULT '0',
    error_count    TEXT NOT NULL DEFAULT '0',
    duration_sum   TEXT NOT NULL DEFAULT '0',
    duration_min   TEXT NOT NULL DEFAULT '0',
    duration_max   TEXT NOT NULL DEFAULT '0',
    p50_ms         TEXT NOT NULL DEFAULT '0',
    p95_ms         TEXT NOT NULL DEFAULT '0',
    p99_ms         TEXT NOT NULL DEFAULT '0',
    version        BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, service_name, operation_name, ts_bucket);

-- ============================================================================
-- Service dependency map (caller -> callee pairs)
-- ============================================================================
-- Built from spans with parent_span_id. Tracks call counts and latency
-- between services for the dependency graph visualization.

CREATE TABLE IF NOT EXISTS service_dependencies (
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    src_service    TEXT NOT NULL,
    dst_service    TEXT NOT NULL,
    call_count     TEXT NOT NULL DEFAULT '0',
    error_count    TEXT NOT NULL DEFAULT '0',
    avg_duration   TEXT NOT NULL DEFAULT '0',
    ts_bucket      BIGINT NOT NULL,
    version        BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, src_service, dst_service, ts_bucket);
