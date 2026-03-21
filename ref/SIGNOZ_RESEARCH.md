# SignOz Competitor Analysis

Comprehensive research document based on source code analysis of the SignOz monorepo.
Repository: `github.com/SigNoz/signoz` (Go 1.25, as of March 2026)

---

## 1. Tech Stack

### Backend
- **Language:** Go 1.25
- **HTTP framework:** gorilla/mux + gorilla/handlers for CORS
- **CLI framework:** spf13/cobra
- **SQL builder:** huandu/go-sqlbuilder (ClickHouse dialect)
- **ClickHouse driver:** ClickHouse/clickhouse-go/v2
- **Prometheus engine:** prometheus/prometheus (embedded PromQL engine for metric queries)
- **Alertmanager:** prometheus/alertmanager (embedded)
- **ORM (metadata):** uptrace/bun (Postgres + SQLite dialects)
- **Auth:** golang-jwt/jwt, coreos/go-oidc, russellhaering/gosaml2
- **Authorization:** openfga/api (OpenFGA-based RBAC in enterprise)
- **Config:** knadh/koanf v2
- **Redis:** go-redis/v9 (caching)
- **Expression evaluation:** antonmedv/expr, SigNoz/govaluate
- **Parser:** antlr4-go/antlr (custom query grammar in `grammar/` directory)
- **Protobuf:** google.golang.org/protobuf
- **OpenTelemetry SDK:** full OTel Go SDK for self-instrumentation

### Frontend
- **Language:** TypeScript
- **Framework:** React (Vite build system)
- **UI library:** Ant Design + custom `@signozhq/*` design system components
- **State management:** React providers/stores
- **Charts:** @grafana/data integration
- **Editor:** Monaco editor
- **Monitoring:** Sentry integration
- **Testing:** Jest + Playwright (e2e)
- **Package manager:** Yarn
- **Node requirement:** >= 22.0.0

### Database
- **Telemetry data:** ClickHouse 25.5.6 (primary data store for all traces, metrics, logs)
- **Metadata / application state:** SQLite (default self-hosted) or PostgreSQL (enterprise)
- **Caching:** Redis (optional), in-memory

### Message Queue
- **No Kafka.** SignOz does NOT use a message queue in its architecture. The OTel Collector writes directly to ClickHouse via custom exporters. There is no intermediate buffering layer beyond the OTel Collector's internal batch processor.

### Build System
- **Go:** Standard Go toolchain via Makefile
- **Frontend:** Vite
- **Docker:** docker-compose for self-hosted deployment
- **Two variants:** Community (open source) and Enterprise (adds licensing, SAML, advanced RBAC, anomaly detection)

### Deployment Model
- Docker Compose (primary self-hosted)
- Helm charts (Kubernetes)
- Docker Swarm support
- Single `signoz` binary serves both API server and frontend (embedded web assets)
- Separate `signoz-otel-collector` binary handles telemetry ingestion and ClickHouse schema migrations

---

## 2. Core Features (Exhaustive 2026 Feature List)

### Tracing
- Distributed trace search and filtering (by service, operation, duration, status, attributes)
- Trace waterfall/timeline visualization (spans ordered by time, collapsible tree)
- Flamegraph view for traces
- Trace detail with span attributes, events, links
- Span search with full attribute indexing
- Root span / entry point span filtering (`isRoot`, `isEntryPoint`)
- Trace funnels (multi-step trace analysis)
- Smart trace search (progressive search for large time ranges)
- Trace-to-logs correlation (via trace_id/span_id)
- Trace-to-metrics correlation

### Metrics
- OTLP metrics ingestion (gauges, counters, histograms, exponential histograms, summaries)
- Metrics Explorer with tree map visualizations
- Time series queries with PromQL compatibility (embedded Prometheus engine)
- Custom query builder for metrics (v4/v5 API)
- Rate/increase calculations for cumulative and delta temporality
- Histogram quantile calculations (p50, p75, p90, p95, p99)
- Exponential histogram support
- Pre-aggregated metric tables (5m, 30m, 1d rollups)
- Time series table selection based on query time range (auto-optimization)
- Metric metadata management (units, descriptions)
- Related metrics discovery (name similarity, attribute similarity)
- Active time series tracking
- Metric normalization

### Logs
- Structured log ingestion via OTLP
- Full-text search on log body
- JSON body parsing with `body_v2` (ClickHouse JSON column type)
- Attribute-based log filtering
- Log pipelines (transformation/parsing pipeline configuration via OpAMP)
- Live tail (real-time log streaming via WebSocket)
- Log-to-trace correlation
- Saved views for log queries
- Log attribute keys/values exploration
- Custom skip indices (bloom filter) on log attributes

### APM (Application Performance Monitoring)
- Services list with p99, avg duration, call rate, error rate
- Top operations per service
- Entry point operations
- Service dependency graph / service map with latency percentiles
- Apdex score configuration and tracking
- RED metrics (Rate, Error, Duration) derived from spans

### Dashboards
- Custom dashboards with multiple panel types
- Time series, scalar, list/table panels
- PromQL and query builder support
- Dashboard variables with query-driven dynamic values
- Dashboard locking
- Public dashboards (shareable without auth)

### Alerting
- Threshold-based alerts (metric, trace, log signals)
- PromQL-based alerts (Prometheus-compatible rules)
- Alert rule management (CRUD)
- Alert channels/receivers (notification routing)
- Route policies for alert routing
- Downtime schedules / maintenance windows
- Alert state history and timeline tracking
- Top contributors analysis for alert triggers
- Resolution time analytics
- Test rule execution before saving

### Exceptions / Errors
- Error listing and grouping
- Error detail with associated span data
- Error count tracking
- Next/previous error navigation within groups

### Infrastructure Monitoring
- Host metrics monitoring
- Kubernetes monitoring: pods, nodes, namespaces, clusters, deployments, daemonsets, statefulsets, jobs, PVCs
- Process monitoring
- Infrastructure onboarding wizard

### Integrations
- Built-in integrations marketplace
- Cloud integrations (AWS, GCP, etc.)
- Messaging queue monitoring (Kafka)
- Celery monitoring
- ClickHouse self-monitoring integration
- Log parsing pipelines (configurable)

### Data Management
- TTL management (configurable retention per signal type)
- Custom retention per organization
- Cold storage configuration
- Disk management and monitoring
- Usage explorer

### Platform
- Multi-organization support
- User management with roles (viewer, editor, admin)
- RBAC with OpenFGA (enterprise)
- SSO: OIDC, SAML (enterprise)
- API keys / Personal Access Tokens
- Service accounts
- Licensing system (enterprise)
- OpAMP (Open Agent Management Protocol) for collector configuration
- Analytics / telemetry reporting
- Feature flags system

### Newer Features (2025-2026)
- Trace funnels
- API monitoring (quick filters)
- Metrics Explorer v2 with tree maps and related metrics
- JSON body search for logs (`body_v2` with ClickHouse JSON column)
- Query builder v5 with expression-based aggregations
- Anomaly detection (enterprise: daily, hourly, weekly, seasonal)
- Public dashboards
- Advanced RBAC migration (OpenFGA-based)
- Meter tables (separate DB for derived metrics from spans)

---

## 3. Data Model / ClickHouse Schemas

SignOz uses **5 ClickHouse databases** with distributed tables (for clustering) and local tables (for single-node). The schema migrations live in the separate `signoz-otel-collector` repository and are executed by the collector binary (`signoz-otel-collector migrate`).

### 3.1 Traces Database: `signoz_traces`

#### Main Table: `signoz_index_v3` (distributed: `distributed_signoz_index_v3`)

This is the primary span storage table. Schema reconstructed from field mapper code:

```
ts_bucket_start          UInt64
resource_fingerprint     String

-- Intrinsic span fields
timestamp                DateTime64(9, 'UTC')     -- nanosecond precision
trace_id                 FixedString(32)
span_id                  String
trace_state              String
parent_span_id           String
flags                    UInt32
name                     LowCardinality(String)
kind                     Int8
kind_string              String
duration_nano            UInt64
status_code              Int16
status_message           String
status_code_string       String

-- Attribute maps
attributes_string        Map(LowCardinality(String), String)
attributes_number        Map(LowCardinality(String), Float64)
attributes_bool          Map(LowCardinality(String), Bool)
resources_string         Map(LowCardinality(String), String)
resource                 JSON                      -- ClickHouse native JSON column

-- Events and links
events                   Array(String)
links                    String

-- Derived/calculated columns
response_status_code     LowCardinality(String)
external_http_url        LowCardinality(String)
http_url                 LowCardinality(String)
external_http_method     LowCardinality(String)
http_method              LowCardinality(String)
http_host                LowCardinality(String)
db_name                  LowCardinality(String)
db_operation             LowCardinality(String)
has_error                Bool
is_remote                LowCardinality(String)

-- Materialized columns from map fields (MATERIALIZED from attributes/resources maps)
resource_string_service$$name             String    -- service.name
attribute_string_http$$route              String    -- http.route
attribute_string_messaging$$system        String
attribute_string_messaging$$operation     String
attribute_string_db$$system               String
attribute_string_rpc$$system              String
attribute_string_rpc$$service             String
attribute_string_rpc$$method              String
attribute_string_peer$$service            String

-- Materialized exists columns (for efficient existence checks)
resource_string_service$$name_exists      Bool
attribute_string_http$$route_exists       Bool
-- ... (one _exists column per materialized attribute)
```

**Key design decisions:**
- `$$` is used instead of `.` in materialized column names (ClickHouse column naming limitation)
- `ts_bucket_start` is a bucketed timestamp used as part of the primary key for efficient time-range scans
- `resource_fingerprint` is a hash of resource attributes, enabling resource-level filtering via a separate resource table
- Attributes are stored in typed Map columns (string, number, bool) -- not a single JSON blob
- The `resource` column uses ClickHouse's native JSON type (newer addition alongside the legacy `resources_string` map)
- Materialized columns are auto-extracted from the Map columns for commonly queried attributes

#### Resource Table: `traces_v3_resource` (distributed: `distributed_traces_v3_resource`)
Separate table for resource attributes, joined via `resource_fingerprint`. Enables efficient filtering on resource attributes without scanning the main spans table.

#### Other Traces Tables
| Table | Purpose |
|-------|---------|
| `distributed_top_level_operations` | Tracks top-level (root) operations per service. Columns: name, serviceName, time |
| `distributed_trace_summary` | Pre-aggregated trace-level summaries |
| `distributed_dependency_graph_minutes_v2` | Service dependency graph with pre-aggregated latency quantiles. Columns: src, dest, timestamp, duration_quantiles_state (AggregateFunction), total_count, error_count |
| `distributed_signoz_error_index_v2` | Error/exception index |
| `distributed_tag_attributes_v2` | Attribute key/value metadata for autocomplete |
| `distributed_span_attributes_keys` | Span attribute keys index (tagKey, tagType, dataType, isColumn) |
| `distributed_durationSort` | Materialized view sorted by duration |
| `distributed_usage_explorer` | Usage tracking |
| `distributed_signoz_spans` | Raw span data |

### 3.2 Metrics Database: `signoz_metrics`

#### Time Series Table: `time_series_v4` (distributed: `distributed_time_series_v4`)

```
temporality              LowCardinality(String)    -- 'delta' or 'cumulative'
metric_name              LowCardinality(String)
type                     LowCardinality(String)    -- gauge, sum, histogram, exp_histogram
is_monotonic             Bool
fingerprint              UInt64                     -- hash of metric identity + labels
unix_milli               Int64
labels                   String                     -- JSON-encoded label set
attrs                    Map(LowCardinality(String), String)
scope_attrs              Map(LowCardinality(String), String)
resource_attrs           Map(LowCardinality(String), String)
```

**Pre-aggregated time series tables** (for longer time ranges):
| Table | Granularity | Used when |
|-------|-------------|-----------|
| `time_series_v4` | 1 hour | < 6 hours range |
| `time_series_v4_6hrs` | 6 hours | 6h - 1 day |
| `time_series_v4_1day` | 1 day | 1 day - 1 week |
| `time_series_v4_1week` | 1 week | > 1 week |

#### Samples Table: `samples_v4` (distributed: `distributed_samples_v4`)

Stores actual metric data points:
```
fingerprint    UInt64
unix_milli     Int64
value          Float64
```

**Pre-aggregated samples tables:**
| Table | Granularity | Columns | Used when |
|-------|-------------|---------|-----------|
| `samples_v4` | Raw | value | < 1 day |
| `samples_v4_agg_5m` | 5 minutes | sum, count, min, max, last | 1 day - 1 week |
| `samples_v4_agg_30m` | 30 minutes | sum, count, min, max, last | > 1 week |

#### Exponential Histogram Table: `exp_hist` (distributed: `distributed_exp_hist`)
Stores exponential histogram data points (separate from regular samples).

#### Metadata Tables
| Table | Purpose |
|-------|---------|
| `distributed_metadata` / `metadata` | Metric attribute metadata |
| `distributed_updated_metadata` / `updated_metadata` | Updated metric metadata (user overrides) |

### 3.3 Logs Database: `signoz_logs`

#### Main Table: `logs_v2` (distributed: `distributed_logs_v2`)

```
ts_bucket_start          UInt64
resource_fingerprint     String

-- Intrinsic fields
timestamp                UInt64                     -- nanoseconds
observed_timestamp       UInt64
id                       String
trace_id                 String
span_id                  String
trace_flags              UInt32
severity_text            LowCardinality(String)
severity_number          UInt8
body                     String                     -- plain text body
body_v2                  JSON(max_dynamic_types=32, max_dynamic_paths=0)  -- structured JSON body
body_promoted            JSON                       -- promoted paths from body_v2

-- Attribute maps
attributes_string        Map(LowCardinality(String), String)
attributes_number        Map(LowCardinality(String), Float64)
attributes_bool          Map(LowCardinality(String), Bool)
resources_string         Map(LowCardinality(String), String)
resource                 JSON

-- Scope
scope_name               String
scope_version            String
scope_string             Map(LowCardinality(String), String)
```

#### Other Logs Tables
| Table | Purpose |
|-------|---------|
| `distributed_logs_v2_resource` | Resource attributes for logs (fingerprint-based) |
| `distributed_tag_attributes_v2` | Attribute metadata for autocomplete |
| `distributed_logs_attribute_keys` | Log attribute key index |
| `distributed_logs_resource_keys` | Log resource key index |
| `distributed_json_path_types` | JSON body path type tracking |
| `distributed_json_promoted_paths` | Promoted JSON body paths |

### 3.4 Meter Database: `signoz_meter`

Used for **derived metrics from spans** (generated by the `signozspanmetrics` connector in the OTel Collector):

| Table | Purpose |
|-------|---------|
| `distributed_samples` / `samples` | Raw derived metric samples |
| `distributed_samples_agg_1d` / `samples_agg_1d` | 1-day aggregated samples (sum, count, min, max, last) |

Table selection: raw for < 30 days, aggregated for >= 30 days.

### 3.5 Metadata Database: `signoz_metadata`

| Table | Purpose |
|-------|---------|
| `distributed_attributes_metadata` / `attributes_metadata` | Cross-signal attribute metadata |

### 3.6 Analytics Database: `signoz_analytics`

| Table | Purpose |
|-------|---------|
| `distributed_rule_state_history_v0` | Alert rule state history |

### Design Patterns

**Resource Fingerprinting:** Resources (service.name, host.name, etc.) are hashed into a `resource_fingerprint` and stored in a separate resource table. The main data tables reference this fingerprint. This is a critical optimization -- resource attributes change infrequently, so deduplicating them saves significant storage and allows efficient resource-level filtering via a subquery/CTE pattern:
```sql
WITH __resource_filter AS (
    SELECT fingerprint FROM signoz_traces.distributed_traces_v3_resource
    WHERE <resource conditions>
)
SELECT ... FROM signoz_traces.distributed_signoz_index_v3
WHERE resource_fingerprint GLOBAL IN (SELECT fingerprint FROM __resource_filter)
```

**Materialized Columns:** Frequently queried attributes (service.name, http.route, etc.) are materialized as top-level columns extracted from the Map columns. This avoids Map lookups for common queries. The naming convention uses `$$` as a dot replacement: `resource_string_service$$name`.

**Time Bucket Partitioning:** `ts_bucket_start` is a coarse timestamp bucket used in the primary key for partition pruning. Queries always include `ts_bucket_start` bounds alongside `timestamp` bounds.

---

## 4. OTLP Ingestion Pipeline

### Architecture Overview

```
[Application + OTel SDK] --> [OTLP gRPC/HTTP] --> [signoz-otel-collector] --> [ClickHouse]
```

The SignOz OTel Collector (`signoz/signoz-otel-collector`) is a **custom build of the OpenTelemetry Collector** with SignOz-specific exporters and processors. It is the sole ingestion path -- the SignOz backend (query service) does NOT accept telemetry data directly.

### Receivers

From `otel-collector-config.yaml`:

```yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318
  prometheus:
    config:
      scrape_configs:
        - job_name: otel-collector
          static_configs:
            - targets: [localhost:8888]
```

- **OTLP gRPC** on port 4317 (primary)
- **OTLP HTTP** on port 4318
- **Prometheus scrape** for self-monitoring

### Processors

```yaml
processors:
  batch:
    send_batch_size: 10000
    send_batch_max_size: 11000
    timeout: 10s
  batch/meter:
    send_batch_max_size: 25000
    send_batch_size: 20000
    timeout: 1s
  resourcedetection:
    detectors: [env, system]
    timeout: 2s
  signozspanmetrics/delta:
    metrics_exporter: signozclickhousemetrics
    metrics_flush_interval: 60s
    latency_histogram_buckets: [100us, 1ms, 2ms, 6ms, 10ms, 50ms, 100ms, 250ms, 500ms, 1000ms, 1400ms, 2000ms, 5s, 10s, 20s, 40s, 60s]
    dimensions_cache_size: 100000
    aggregation_temporality: AGGREGATION_TEMPORALITY_DELTA
    enable_exp_histogram: true
    dimensions:
      - name: service.namespace (default: default)
      - name: deployment.environment (default: default)
      - name: signoz.collector.id
      - name: service.version
      - name: browser.platform
      - name: browser.mobile
      - name: k8s.cluster.name
      - name: k8s.node.name
      - name: k8s.namespace.name
      - name: host.name
      - name: host.type
      - name: container.name
```

**Key processor: `signozspanmetrics/delta`** -- This is the RED metrics generator. It processes trace spans and emits:
- Call count metrics
- Error count metrics
- Duration histogram metrics (with configurable buckets + exponential histogram)
- These derived metrics power the APM service overview (p99, error rate, call rate)

### Connectors

```yaml
connectors:
  signozmeter:
    metrics_flush_interval: 1h
    dimensions:
      - name: service.name
      - name: deployment.environment
      - name: host.name
```

The `signozmeter` connector derives usage/metering metrics from all signals and writes them to the `signoz_meter` database.

### Exporters (Signal -> ClickHouse mapping)

```yaml
exporters:
  clickhousetraces:
    datasource: tcp://clickhouse:9000/signoz_traces
    use_new_schema: true
  signozclickhousemetrics:
    dsn: tcp://clickhouse:9000/signoz_metrics
  clickhouselogsexporter:
    dsn: tcp://clickhouse:9000/signoz_logs
    timeout: 10s
    use_new_schema: true
  signozclickhousemeter:
    dsn: tcp://clickhouse:9000/signoz_meter
    timeout: 45s
    sending_queue:
      enabled: false
  metadataexporter:
    cache:
      provider: in_memory
    dsn: tcp://clickhouse:9000/signoz_metadata
    enabled: true
    timeout: 45s
```

### Pipeline Configuration

```yaml
pipelines:
  traces:
    receivers: [otlp]
    processors: [signozspanmetrics/delta, batch]
    exporters: [clickhousetraces, metadataexporter, signozmeter]
  metrics:
    receivers: [otlp]
    processors: [batch]
    exporters: [signozclickhousemetrics, metadataexporter, signozmeter]
  metrics/prometheus:
    receivers: [prometheus]
    processors: [batch]
    exporters: [signozclickhousemetrics, metadataexporter, signozmeter]
  logs:
    receivers: [otlp]
    processors: [batch]
    exporters: [clickhouselogsexporter, metadataexporter, signozmeter]
  metrics/meter:
    receivers: [signozmeter]
    processors: [batch/meter]
    exporters: [signozclickhousemeter]
```

### Ingestion Flow per Signal

**Traces:**
1. OTLP spans received via gRPC/HTTP
2. `signozspanmetrics/delta` processor extracts RED metrics from spans (these go to the metrics pipeline)
3. `batch` processor batches spans (10k batch size, 10s timeout)
4. `clickhousetraces` exporter writes to `signoz_traces` database
5. `metadataexporter` writes attribute metadata to `signoz_metadata`
6. `signozmeter` connector extracts usage metrics

**Metrics:**
1. OTLP metrics received via gRPC/HTTP
2. `batch` processor batches metrics
3. `signozclickhousemetrics` exporter writes to `signoz_metrics` database
4. `metadataexporter` writes attribute metadata
5. `signozmeter` connector extracts usage metrics

**Logs:**
1. OTLP logs received via gRPC/HTTP
2. `batch` processor batches logs
3. `clickhouselogsexporter` exporter writes to `signoz_logs` database
4. `metadataexporter` writes attribute metadata
5. `signozmeter` connector extracts usage metrics

### Batching and Backpressure

- Batch processor: 10,000 items per batch, max 11,000, 10s timeout
- Meter batch: 20,000 items, max 25,000, 1s timeout (more aggressive for metering)
- The signozclickhousemeter exporter has `sending_queue.enabled: false` (synchronous writes)
- Other exporters use default sending queues
- ClickHouse connection timeout: configurable via `SIGNOZ_OTEL_COLLECTOR_TIMEOUT=10m`

### Schema Migrations

The collector binary handles ClickHouse schema migrations:
```bash
# Bootstrap (create databases)
signoz-otel-collector migrate bootstrap

# Synchronous migrations (blocking, must complete before startup)
signoz-otel-collector migrate sync up

# Asynchronous migrations (can run in background)
signoz-otel-collector migrate async up

# Check sync status
signoz-otel-collector migrate sync check
```

The `signoz-telemetrystore-migrator` container in docker-compose runs migrations before the collector starts.

---

## 5. Query Patterns

### 5.1 Trace Search and Filtering

Queries go against `signoz_traces.distributed_signoz_index_v3`. The query builder generates ClickHouse SQL using `go-sqlbuilder`.

**Basic trace list query:**
```sql
SELECT timestamp AS `timestamp`, span_id AS `span_id`, ...
FROM signoz_traces.distributed_signoz_index_v3
WHERE timestamp >= <start_ns> AND timestamp < <end_ns>
  AND ts_bucket_start >= <start_bucket> AND ts_bucket_start <= <end_bucket>
  AND resource_fingerprint GLOBAL IN (SELECT fingerprint FROM __resource_filter)
ORDER BY timestamp DESC
LIMIT 100
```

**Attribute filtering** uses Map column access:
- String attributes: `attributes_string['http.method'] = 'GET'`
- Number attributes: `attributes_number['http.status_code'] > 400`
- Materialized attributes: `resource_string_service$$name = 'my-service'` (direct column access, much faster)

**Trace ID optimization:** When filtering by trace_id, the query builder first looks up the trace's time range from the `trace_summary` table, then narrows the time window to avoid full scans.

### 5.2 Service Maps / Dependency Graphs

Queried from `distributed_dependency_graph_minutes_v2` -- a pre-aggregated materialized view:

```sql
WITH
    quantilesMergeState(0.5, 0.75, 0.9, 0.95, 0.99)(duration_quantiles_state) AS duration_quantiles_state,
    finalizeAggregation(duration_quantiles_state) AS result
SELECT
    src as parent,
    dest as child,
    result[1] AS p50,
    result[2] AS p75,
    result[3] AS p90,
    result[4] AS p95,
    result[5] AS p99,
    sum(total_count) as callCount,
    sum(total_count)/ @duration AS callRate,
    sum(error_count)/sum(total_count) * 100 as errorRate
FROM signoz_traces.distributed_dependency_graph_minutes_v2
WHERE toUInt64(toDateTime(timestamp)) >= @start AND toUInt64(toDateTime(timestamp)) <= @end
GROUP BY src, dest
```

This uses ClickHouse's `AggregatingMergeTree` engine with `quantilesState` to store pre-computed quantile sketches. The `quantilesMergeState` + `finalizeAggregation` pattern merges partial aggregates at query time.

### 5.3 Latency Percentiles (p50, p95, p99)

For direct span queries: `quantile(0.99)(duration_nano) as p99`

For service overview, RED metrics are pre-computed by `signozspanmetrics/delta` processor with histogram buckets, then stored as metrics in `signoz_metrics`. The histogram quantile calculation uses a custom UDF:
```
histogramQuantile(groupArray(le), groupArray(value), 0.99)
```
This is a custom ClickHouse user-defined function (`histogram-quantile` binary fetched during init).

### 5.4 Error Rates

```sql
SELECT count(*) as numErrors
FROM signoz_traces.distributed_signoz_index_v3
WHERE resource_string_service$$name = @serviceName
  AND name IN @names
  AND timestamp >= @start AND timestamp <= @end
  AND statusCode = 2
```

### 5.5 Metrics Dashboards

Metrics queries use a multi-table strategy:

1. **Time series resolution** -- select the appropriate pre-aggregated table based on time range
2. **Samples table** -- select raw vs 5m vs 30m aggregated based on time range
3. **Pipeline query** -- CTE-based pipeline:
   - Inner query: per-series aggregation from samples table
   - Window functions for rate/increase (using `lagInFrame` over a `rate_window`)
   - Outer query: space aggregation (avg, sum, min, max across series)

Rate calculation for cumulative metrics:
```sql
multiIf(
    row_number() OVER rate_window = 1, nan,
    (per_series_value - lagInFrame(per_series_value, 1) OVER rate_window) < 0,
        per_series_value / (ts - lagInFrame(ts, 1) OVER rate_window),
    (per_series_value - lagInFrame(per_series_value, 1) OVER rate_window)
        / (ts - lagInFrame(ts, 1) OVER rate_window)
)
```

### 5.6 Log Search

Queries against `signoz_logs.distributed_logs_v2`:
```sql
SELECT timestamp, id, severity_text, body, ...
FROM signoz_logs.distributed_logs_v2
WHERE timestamp >= <start> AND timestamp < <end>
  AND ts_bucket_start >= <start_bucket> AND ts_bucket_start <= <end_bucket>
ORDER BY timestamp DESC, id DESC
LIMIT 100
```

**JSON body search** (with `body_v2` column):
```sql
dynamicElement(body_v2.`status`, 'String')
```

### 5.7 Query Builder / SQL Generation

SignOz has a sophisticated query builder system (`pkg/querybuilder/`, `pkg/telemetry{traces,metrics,logs}/`):

- **Field Mapper** -- Maps logical field names to ClickHouse column expressions. Handles:
  - Intrinsic fields (direct column access)
  - Attribute fields (Map column access or materialized column)
  - Resource fields (JSON column with fallback to Map column)
  - Collision handling (same field name in multiple contexts)

- **Condition Builder** -- Converts filter expressions to ClickHouse WHERE clauses with proper type handling

- **Statement Builder** -- Signal-specific builders that compose the full SQL:
  - `traceQueryStatementBuilder` -- handles list, time series, scalar, trace query types
  - `MetricQueryStatementBuilder` -- handles the multi-table pipeline pattern
  - `logQueryStatementBuilder` -- handles log-specific queries with JSON body support

- **Expression Rewriter** -- Rewrites aggregation expressions for ClickHouse compatibility

- **ANTLR Grammar** -- Custom query language parsed by ANTLR4 (in `grammar/` directory)

### Pre-aggregation Strategy

| Signal | Pre-aggregation |
|--------|----------------|
| Metrics samples | 5m, 30m rollups (AggregatingMergeTree) |
| Metrics time series | 1h, 6h, 1d, 1w rollups |
| Meter samples | 1d rollup |
| Trace dependency graph | Minute-level rollups with quantile sketches |
| Top-level operations | ReplacingMergeTree (latest per operation/service) |

---

## 6. Architecture

### Services / Components

SignOz consists of **4 deployable components**:

1. **`signoz` binary** (query service + frontend + API server)
   - Serves the React frontend as embedded web assets
   - REST API on port 8080
   - Internal API on port 8085 (alertmanager)
   - OpAMP WebSocket on port 4320
   - Debug/pprof on port 6060

2. **`signoz-otel-collector`** (telemetry ingestion)
   - Custom OpenTelemetry Collector build
   - OTLP gRPC on port 4317
   - OTLP HTTP on port 4318
   - Health check on port 13133
   - Handles ClickHouse schema migrations
   - Managed via OpAMP from the signoz service

3. **ClickHouse** (telemetry data store)
   - Version 25.5.6
   - Clustered setup with ZooKeeper
   - Custom histogram quantile UDF

4. **ZooKeeper** (ClickHouse coordination)
   - Required for ClickHouse replication/clustering

### Internal Architecture of `signoz` Binary

```
cmd/
  community/     -- community edition entry point
  enterprise/    -- enterprise edition entry point (adds licensing, SSO, etc.)

pkg/signoz/      -- Main SigNoz struct, wires everything together
pkg/apiserver/   -- HTTP server setup
pkg/gateway/     -- Request gateway/middleware

pkg/query-service/
  app/
    http_handler.go    -- All REST API route handlers
    server.go          -- HTTP server setup
    clickhouseReader/  -- ClickHouse query implementation (7000+ lines)
    traces/            -- Trace query builders (v3, v4, smart, tracedetail)
    metrics/           -- Metric query builders (v3, v4, cumulative, delta)
    logs/              -- Log query builders (v3, v4)
    querier/           -- Query orchestration (v1, v2)
    services/          -- Service overview queries
  rules/               -- Alert rule engine
  interfaces/          -- Reader/Querier interfaces

pkg/querier/           -- New query engine (v5)
pkg/telemetrytraces/   -- Trace SQL statement builders
pkg/telemetrymetrics/  -- Metric SQL statement builders
pkg/telemetrylogs/     -- Log SQL statement builders
pkg/telemetrymeter/    -- Meter SQL statement builders
pkg/telemetrymetadata/ -- Cross-signal metadata queries
pkg/telemetrystore/    -- ClickHouse connection management
pkg/querybuilder/      -- Shared query building utilities

pkg/alertmanager/      -- Embedded Prometheus Alertmanager
pkg/prometheus/        -- Embedded Prometheus PromQL engine
pkg/authn/             -- Authentication
pkg/authz/             -- Authorization (OpenFGA)
pkg/cache/             -- Caching layer (Redis, in-memory)
pkg/modules/           -- Feature modules (dashboard, org, user, etc.)
```

### Communication Between Components

- **signoz <-> ClickHouse:** Direct TCP connection (clickhouse-go driver)
- **signoz <-> signoz-otel-collector:** OpAMP WebSocket (for dynamic collector configuration)
- **signoz <-> Redis:** TCP (optional caching)
- **signoz <-> SQLite/PostgreSQL:** Direct connection (application state)
- **Applications <-> signoz-otel-collector:** OTLP gRPC/HTTP

### Query Service vs Ingestion Service

They are **completely separate processes**:
- The `signoz` binary handles all queries, API requests, alerting, and UI serving
- The `signoz-otel-collector` binary handles all telemetry ingestion and ClickHouse writes
- They share no state except ClickHouse (and optionally OpAMP for configuration)

### Scaling

- **ClickHouse:** Horizontally scalable via sharding/replication (distributed tables with `_local` counterparts)
- **signoz:** Single instance (stateless queries, can be replicated behind a load balancer)
- **signoz-otel-collector:** Can be scaled horizontally (multiple collectors writing to same ClickHouse cluster, with `signoz.collector.id` dimension for deduplication)
- **HA mode:** `docker-compose.ha.yaml` available for high-availability deployment

### Self-hosted Deployment (Docker Compose)

```
signoz-zookeeper-1     (ZooKeeper for ClickHouse)
signoz-clickhouse      (ClickHouse server)
signoz-init-clickhouse (Downloads histogram UDF binary)
signoz-telemetrystore-migrator (Runs ClickHouse migrations, exits)
signoz-otel-collector  (Telemetry ingestion)
signoz                 (Query service + UI)
```

Volumes: `signoz-clickhouse` (data), `signoz-sqlite` (metadata), `signoz-zookeeper-1` (coordination)

---

## 7. Trace Model

### Span Representation

A distributed trace is a collection of spans sharing the same `trace_id` (FixedString(32)). Each span has:

| Field | Type | Description |
|-------|------|-------------|
| trace_id | FixedString(32) | 128-bit trace identifier (hex) |
| span_id | String | 64-bit span identifier |
| parent_span_id | String | Parent span ID (empty for root spans) |
| trace_state | String | W3C tracestate header |
| name | LowCardinality(String) | Operation name |
| kind | Int8 | Span kind (1=Internal, 2=Server, 3=Client, 4=Producer, 5=Consumer) |
| kind_string | String | Human-readable kind |
| duration_nano | UInt64 | Duration in nanoseconds |
| status_code | Int16 | 0=Unset, 1=Ok, 2=Error |
| status_code_string | String | Human-readable status |
| status_message | String | Status description |
| timestamp | DateTime64(9, 'UTC') | Start time (nanosecond precision) |
| flags | UInt32 | Trace flags |
| events | Array(String) | Span events (serialized) |
| links | String | Span links (serialized) |
| has_error | Bool | Derived: status_code == 2 |

### Trace Waterfall Reconstruction

The waterfall view is built by:
1. Fetching all spans for a `trace_id` from `signoz_index_v3`
2. Building a tree structure using `parent_span_id` relationships
3. Pre-order traversal with collapsible nodes
4. Sorting children by `timestamp` (then by `name` for ties)
5. Rendering spans with their relative positions on a timeline
6. Supporting pagination (500 spans per request for large traces)
7. Selected span ID tracking with path-to-root expansion

The flamegraph view uses the same span data but renders it as a flame chart grouped by service.

### Span Attribute Indexing

- **Map columns** (`attributes_string`, `attributes_number`, `attributes_bool`): All attributes are stored in typed maps. Querying uses `map['key']` syntax.
- **Materialized columns**: High-cardinality frequently-queried attributes are extracted into dedicated columns (e.g., `resource_string_service$$name`). These are created via ClickHouse `MATERIALIZED` column expressions.
- **Skip indices**: Bloom filter indices on attribute columns for efficient existence checks.
- **Attribute keys table** (`span_attributes_keys`): Tracks all observed attribute keys with their type (tag/resource/spanfield) and data type. Used for autocomplete/suggestions.

### Service Map Generation

Service maps are derived from span data by the OTel Collector's custom processor, which:
1. Identifies client-server span pairs (using span kind and parent relationships)
2. Extracts source service (`src`) and destination service (`dest`) from `service.name` resource attributes
3. Pre-aggregates duration quantiles using ClickHouse `quantilesState` AggregateFunction
4. Stores in `dependency_graph_minutes_v2` at minute granularity
5. Query-time: `quantilesMergeState` merges partial aggregates and `finalizeAggregation` produces final percentiles

---

## 8. Alerting

### Alert Architecture

SignOz embeds a full Prometheus Alertmanager for notification routing and an alert evaluation engine:

- **Rule Engine** (`pkg/query-service/rules/`): Evaluates alert rules on a schedule
- **Alertmanager** (`pkg/alertmanager/`): Handles notification routing, grouping, silencing
- **Rule Store**: Alert rules stored in SQLite/PostgreSQL (not ClickHouse)
- **State History**: Alert state transitions stored in ClickHouse (`rule_state_history_v0`)

### Alert Types

1. **Threshold Rules** (`ThresholdRule`): Compares query results against thresholds
   - Works with all signals: metrics, traces, logs
   - Supports query builder v3/v4/v5 queries
   - Condition evaluation: compares aggregated values against configurable thresholds

2. **PromQL Rules** (`PromRule`): Standard Prometheus alerting rules
   - Uses the embedded Prometheus engine for evaluation
   - Compatible with existing Prometheus alerting rules

### Alert Rule Structure

Each rule has:
- **Condition**: Query definition + comparison operator + threshold
- **Eval window**: Time range for query evaluation
- **Eval delay**: Delay before evaluation (accounts for data arrival lag, default 2m)
- **Hold duration**: How long condition must be true before firing
- **Labels/Annotations**: Metadata for notification templates
- **Preferred channels**: Notification channel routing

### Alert Evaluation Flow

1. Rule manager schedules evaluation at configured interval
2. Rule evaluates its query (via ClickHouse reader or v5 querier)
3. Result compared against threshold
4. State transitions tracked (pending -> firing -> resolved)
5. Firing alerts sent to Alertmanager
6. Alertmanager routes to configured receivers (Slack, PagerDuty, webhook, email, etc.)

### Alert Channels / Route Policies

- Channels (receivers): Configurable notification targets
- Route policies: Rules for routing alerts to specific channels based on labels
- Downtime schedules: Suppress alerts during maintenance windows
- Test notifications: Validate channel configuration before saving

---

## 9. SDK / Instrumentation

### No Custom SDKs

SignOz relies **entirely on OpenTelemetry SDKs**. It does not ship its own instrumentation libraries. This is a core architectural decision -- being "OTLP-native" means:

- Any language with an OTel SDK works out of the box
- No vendor lock-in at the instrumentation layer
- Automatic compatibility with the OTel ecosystem

### Recommended Setup

1. Install the OpenTelemetry SDK for your language
2. Configure the OTLP exporter to point at the SignOz collector (`<collector-host>:4317` for gRPC or `:4318` for HTTP)
3. Instrument your application using OTel auto-instrumentation or manual instrumentation
4. Traces, metrics, and logs all flow through the same OTLP endpoint

### Collector Configuration (OpAMP)

SignOz uses OpAMP (Open Agent Management Protocol) to remotely manage collector configuration:
- The `signoz` service runs an OpAMP WebSocket server on port 4320
- The collector connects to this server on startup
- Configuration changes (e.g., log parsing pipelines) are pushed via OpAMP
- This enables dynamic configuration without collector restarts

---

## 10. API Design

### Query APIs

All APIs are served on port 8080 under `/api/v1` or `/api/v2` paths.

#### Traces
| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/traces/{traceId}` | Search spans for a trace |
| POST | `/api/v2/traces/waterfall/{traceId}` | Waterfall view data |
| POST | `/api/v2/traces/flamegraph/{traceId}` | Flamegraph view data |
| GET | `/api/v2/traces/fields` | List trace field definitions |
| POST | `/api/v2/traces/fields` | Update trace field |

#### Services / APM
| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/v2/services` | Get services with RED metrics |
| POST | `/api/v2/service/top_operations` | Top operations per service |
| POST | `/api/v1/service/top_level_operations` | Top-level operations |
| POST | `/api/v1/dependency_graph` | Service map / dependency graph |
| GET | `/api/v1/services/list` | Simple service name list |

#### Metrics
| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/query_range` | PromQL range query |
| GET | `/api/v1/query` | PromQL instant query |

#### Dashboards
| Method | Path | Description |
|--------|------|-------------|
| GET/POST | `/api/v1/dashboards` | List / create dashboards |
| GET/PUT/DELETE | `/api/v1/dashboards/{id}` | Get / update / delete dashboard |
| POST | `/api/v2/variables/query` | Query dashboard variables |

#### Alerts
| Method | Path | Description |
|--------|------|-------------|
| GET/POST | `/api/v1/rules` | List / create alert rules |
| GET/PUT/DELETE/PATCH | `/api/v1/rules/{id}` | CRUD for individual rules |
| POST | `/api/v1/testRule` | Test alert rule |
| GET | `/api/v1/alerts` | Get active alerts |
| CRUD | `/api/v1/channels` | Notification channels |
| CRUD | `/api/v1/route_policies` | Alert routing policies |
| CRUD | `/api/v1/downtime_schedules` | Maintenance windows |

#### Logs
| Method | Path | Description |
|--------|------|-------------|
| WebSocket | LiveTail endpoint | Real-time log streaming |

#### Query Builder (v3)
| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/v3/query_range` | Unified query builder for all signals |

#### Infrastructure
| Method | Path | Description |
|--------|------|-------------|
| Various | `/api/v1/hosts/*` | Host metrics |
| Various | `/api/v1/pods/*` | Pod metrics |
| Various | `/api/v1/nodes/*` | Node metrics |
| Various | `/api/v1/namespaces/*` | Namespace metrics |

### Ingestion APIs

SignOz does NOT expose ingestion APIs on the query service. All telemetry ingestion goes through the OTel Collector's standard OTLP endpoints (gRPC 4317, HTTP 4318).

### Authentication Model

- **JWT tokens** for user sessions
- **API Keys** (Personal Access Tokens) for programmatic access
- **OIDC / SAML** for SSO (enterprise)
- **Access levels**: OpenAccess (health, version), ViewAccess (read), EditAccess (write), AdminAccess (settings)
- **OpenFGA-based RBAC** for fine-grained permissions (enterprise)

---

## 11. Strengths and Weaknesses

### Strengths

1. **OTLP-native architecture**: No proprietary agents. Full OpenTelemetry compatibility. This is the right long-term bet as OTel becomes the industry standard.

2. **ClickHouse as single data store**: One database for all signals. Simplifies operations. ClickHouse's columnar storage is excellent for observability workloads.

3. **Pre-aggregation strategy**: Multi-level rollup tables (5m, 30m, 6h, 1d, 1w) with automatic table selection based on query time range. This is well-engineered and gives good performance across time ranges.

4. **Resource fingerprinting**: Deduplicating resource attributes into a separate table with fingerprint-based joins is a smart storage optimization. Resources (service.name, host.name) repeat across millions of spans.

5. **Materialized columns**: Extracting hot attributes (service.name, http.route) into dedicated columns avoids Map lookups. The `$$` naming convention is ugly but functional.

6. **Single binary**: The query service serves both the API and the frontend. Simple deployment.

7. **Embedded Prometheus engine**: PromQL compatibility for metrics queries without running a separate Prometheus instance.

8. **OpAMP integration**: Remote collector management is forward-thinking.

9. **Comprehensive query builder**: The v5 query builder with ANTLR grammar and expression-based aggregations is sophisticated.

10. **Service map with pre-aggregated quantiles**: Using ClickHouse's AggregatingMergeTree with quantilesState for the dependency graph is an efficient pattern.

### Weaknesses

1. **Massive reader.go**: The ClickHouseReader is 7000+ lines of hand-crafted SQL queries. This is a maintenance burden and makes it hard to reason about query correctness.

2. **Schema sprawl**: 5 databases, dozens of tables, multiple schema versions (v2, v3, v4). The migration path is complex and the collector-managed schema is a coupling point.

3. **No message queue**: Direct collector-to-ClickHouse writes mean data loss risk during ClickHouse outages. No buffering layer for backpressure beyond the collector's internal queue.

4. **Separate collector binary**: The schema migrations live in a completely different repository (`signoz-otel-collector`). This creates version coupling issues. The actual CREATE TABLE statements are not visible in the main repo.

5. **Query builder versioning**: v3, v4, v5 query builders coexist. The code has multiple paths for the same query type depending on version.

6. **ZooKeeper dependency**: Even for single-node deployments, ZooKeeper is required for ClickHouse's distributed tables. This adds operational complexity.

7. **Limited log parsing at ingest**: Log parsing pipelines are managed via OpAMP to the collector, which is indirect. The query service cannot modify ingestion behavior directly.

8. **No built-in data sampling**: No tail sampling or head sampling configuration in the default setup.

9. **Enterprise feature gating**: Anomaly detection, SSO, advanced RBAC are enterprise-only. The community edition is functional but limited for larger teams.

### Design Decisions Worth Copying

1. **Resource fingerprinting** -- Absolutely copy this. Hashing resource attributes and storing them in a separate table is a massive storage and query optimization.

2. **Multi-level pre-aggregation** -- The automatic table selection based on query time range is elegant. Worth implementing for metrics.

3. **Materialized columns for hot attributes** -- When you know `service.name` is queried in 90% of queries, extracting it to a top-level column is the right call.

4. **`ts_bucket_start` for partition pruning** -- A coarse timestamp bucket in the primary key helps ClickHouse skip irrelevant partitions.

5. **OTLP as the sole ingestion protocol** -- Simplifies the ingestion layer enormously. No need to support proprietary formats.

6. **Embedded Prometheus engine** -- Good for PromQL compatibility without additional infrastructure.

7. **AggregatingMergeTree for dependency graph** -- Pre-computing quantile sketches at ingest time and merging at query time is the correct ClickHouse pattern.

### Design Decisions to Avoid

1. **7000-line reader file** -- Keep query code modular from the start. Separate by signal and query type.

2. **Schema migrations in a separate repo** -- Keep schema definitions close to the query code that uses them.

3. **ZooKeeper requirement** -- Use ClickHouse Keeper (built-in) or avoid distributed tables for single-node deployments.

4. **Multiple query builder versions** -- Design the query builder to be extensible rather than creating new versions.

5. **JSON-encoded labels in metrics** -- The `labels` column in `time_series_v4` is a JSON string that requires `JSONExtractString()` for every group-by. This is slower than using Map columns (which traces/logs use).

### Comparison with PostHog and Sentry's ClickHouse Usage

**vs PostHog:**
- PostHog uses ClickHouse for events (product analytics), not observability. Different workload pattern (wide events vs time series).
- PostHog uses a single `events` table with a `properties` JSON column. SignOz's typed Map columns are more query-efficient.
- PostHog has materialized columns too, but for product analytics fields.

**vs Sentry:**
- Sentry uses ClickHouse for error/event storage (Snuba query layer).
- Sentry has a more complex multi-storage architecture (Kafka -> Snuba consumers -> ClickHouse). SignOz skips Kafka entirely.
- Sentry's schema is highly specialized for error grouping. SignOz's schema is more generalized for all observability signals.
- Sentry uses Kafka as a buffer. SignOz has no buffer, which is simpler but riskier.

### Is the OTLP-first Approach the Right Call?

**Yes, unequivocally.** OpenTelemetry is becoming the industry standard for instrumentation. By being OTLP-native:
- Zero proprietary agent lock-in
- Automatic compatibility with the growing OTel ecosystem
- Users can switch between SignOz and other OTLP-compatible backends
- Reduces engineering burden (no custom SDKs to maintain per language)

The trade-off is that SignOz depends on OTel SDK quality and feature pace, but the OTel project is mature enough that this is not a meaningful risk in 2026.

For Teploy Observe, OTLP-first is the correct strategy. The ingestion pipeline should accept OTLP and nothing else. Focus engineering effort on the query/visualization layer rather than instrumentation.
