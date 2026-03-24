-- Observe — LLM observability, log pipelines, infrastructure monitoring.

-- ============================================================================
-- LLM traces (AI model call tracking)
-- ============================================================================

CREATE TABLE IF NOT EXISTS llm_traces (
    trace_id       TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    session_id     TEXT NOT NULL DEFAULT '',
    span_id        TEXT NOT NULL DEFAULT '',
    timestamp      BIGINT NOT NULL,
    model          TEXT NOT NULL DEFAULT '',
    provider       TEXT NOT NULL DEFAULT '',
    operation      TEXT NOT NULL DEFAULT 'completion',
    prompt_tokens  TEXT NOT NULL DEFAULT '0',
    completion_tokens TEXT NOT NULL DEFAULT '0',
    total_tokens   TEXT NOT NULL DEFAULT '0',
    cost_usd       TEXT NOT NULL DEFAULT '0',
    latency_ms     TEXT NOT NULL DEFAULT '0',
    status         TEXT NOT NULL DEFAULT 'ok',
    error_message  TEXT NOT NULL DEFAULT '',
    prompt         TEXT NOT NULL DEFAULT '',
    completion     TEXT NOT NULL DEFAULT '',
    metadata       JSONB
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, timestamp);

-- ============================================================================
-- Log pipelines (processing rules)
-- ============================================================================

CREATE TABLE IF NOT EXISTS log_pipelines (
    pipeline_id    TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    priority       TEXT NOT NULL DEFAULT '0',
    rules          JSONB,
    enabled        TEXT NOT NULL DEFAULT 'true',
    created_at     TEXT NOT NULL,
    version        TEXT NOT NULL DEFAULT '0'
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (tenant_id, site_id, pipeline_id);

-- ============================================================================
-- Host metrics (infrastructure monitoring)
-- ============================================================================

CREATE TABLE IF NOT EXISTS host_metrics (
    metric_id      TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    site_id        TEXT NOT NULL,
    hostname       TEXT NOT NULL DEFAULT '',
    timestamp      BIGINT NOT NULL,
    cpu_percent    TEXT NOT NULL DEFAULT '0',
    memory_percent TEXT NOT NULL DEFAULT '0',
    memory_used_mb TEXT NOT NULL DEFAULT '0',
    memory_total_mb TEXT NOT NULL DEFAULT '0',
    disk_percent   TEXT NOT NULL DEFAULT '0',
    disk_used_gb   TEXT NOT NULL DEFAULT '0',
    disk_total_gb  TEXT NOT NULL DEFAULT '0',
    net_rx_bytes   TEXT NOT NULL DEFAULT '0',
    net_tx_bytes   TEXT NOT NULL DEFAULT '0',
    load_1m        TEXT NOT NULL DEFAULT '0',
    load_5m        TEXT NOT NULL DEFAULT '0',
    load_15m       TEXT NOT NULL DEFAULT '0'
) WITH (engine = 'mergetree')
ORDER BY (tenant_id, site_id, hostname, timestamp);
