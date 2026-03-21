# PostHog -- Comprehensive Competitor Research

**Date:** 2026-03-20
**Source:** PostHog monorepo (`posthog/posthog` on GitHub, cloned locally)
**Purpose:** Reference document for building Teploy Observe

---

## 1. Tech Stack

### Languages
- **Python 3.12** -- Primary backend language. Django monolith.
- **TypeScript (Node.js 24)** -- Plugin server / ingestion pipeline ("nodejs/" directory). Also the entire frontend.
- **Rust** -- High-performance capture service, feature flags service, session replay capture, error tracking (cymbal), person management (personhog), property definitions, and various workers. Lives in `rust/`.
- **Go** -- Livestream service only (`livestream/`). Real-time event streaming via WebSocket.

### Frameworks
- **Django 4.2** with Django REST Framework 3.16 -- Core web framework. Uses `drf-spectacular` for OpenAPI generation.
- **React 18** with TypeScript -- Frontend SPA. Uses **Kea** (not Redux) as state management. Vite for bundling.
- **Axum** (Rust) -- HTTP framework for the capture service.
- **Celery 5.3** -- Async task queue (workers).
- **Temporal 1.26** -- Workflow orchestration for batch exports, person deletion, data pipelines.
- **Dagster** -- Data pipeline orchestration (newer addition, for DAG-based ETL).

### Runtime
- **Granian** (Python ASGI server with uvloop) -- Replaces gunicorn/uvicorn for the web server. Configured with 2 workers for hobby deploys.
- **NGINX Unit 1.33** -- Container-level process manager in the Dockerfile.

### Databases

| System | Role |
|--------|------|
| **PostgreSQL 15** | Django models (teams, users, organizations, feature flags, cohort definitions, dashboards, annotations, API keys, billing state). Also used by Temporal for workflow state. Persons are now in a separate Postgres (partitioned by team_id, 64 hash partitions). |
| **ClickHouse 25.12** | ALL analytics data: events, sessions, persons (replicated), groups, heatmaps, performance events, session replay metadata, error tracking, app metrics, log entries, web pre-aggregated stats. THE analytics engine. |
| **Redis 7.2** | Caching, Celery broker, rate limiting, feature flag caching, cookieless salt storage, session recording API state, overflow redirect state, team cache, CDP state. Maxmemory 200MB with allkeys-lru. |
| **Kafka (Redpanda v25.1)** | Event bus between all services. Redpanda is used as a Kafka-compatible broker. |
| **MinIO** | Object storage (S3-compatible) for session recordings, exports, AI blobs. |
| **SeaweedFS** | Second S3-compatible store specifically for session recording v2 blobs. |
| **Elasticsearch 7.17** | Used only by Temporal for workflow visibility. NOT used for analytics search. |
| **ZooKeeper 3.7** | ClickHouse replication coordination. |

### Build System / Deployment
- **pnpm 10.29** with **Turbo** -- Frontend monorepo build system.
- **uv 0.10** -- Python dependency management (replaced pip/poetry).
- **Docker Compose** -- Hobby/self-hosted deployment (docker-compose.hobby.yml).
- **Helm** -- Was used for K8s, now **sunsetted** for self-hosted K8s.
- **Caddy** -- Reverse proxy in front of all services. Routes `/e`, `/capture`, `/batch` to Rust capture; `/s` to replay capture; `/flags` to Rust feature-flags; `/livestream` to Go livestream.
- **Depot** -- Container build service (depot.json present).
- **Multi-stage Dockerfile** -- Frontend build -> sourcemap upload -> Python deps -> GeoIP fetch -> final image.

---

## 2. Core Features (2026 Feature List)

### Product Analytics (Core)
- **Trends** -- Time-series event counts, property breakdowns, formulas between series, compare to previous period
- **Funnels** -- Multi-step conversion funnels, time-to-convert, correlation analysis, funnel trends over time
- **Retention** -- Cohort-based retention tables (returning/first time)
- **Paths** -- User flow visualization between events/pages
- **Stickiness** -- How often users perform an action over time
- **Lifecycle** -- New/returning/resurrecting/dormant user breakdown
- **Calendar Heatmap** -- Event frequency by day visualization
- **SQL Access (HogQL)** -- Full SQL query editor with ClickHouse-backed HogQL language
- **Dashboards** -- Multi-insight dashboards with filters, auto-refresh, sharing
- **Annotations** -- Time-based annotations on charts

### Session Replay (Core)
- **Session Recording** -- Full DOM replay with rrweb
- **Session Replay Events** -- ClickHouse-backed metadata for filtering
- **Desktop Recording** -- Desktop app session capture
- **Session Summaries** -- AI-generated session summaries
- **Console logs** -- Captured alongside replay
- **Network requests** -- Performance waterfall

### Feature Flags (Core)
- **Boolean flags** -- Simple on/off
- **Multivariate flags** -- Multiple variants with percentages
- **Targeting** -- By person properties, cohorts, groups
- **Local evaluation** -- SDK-side evaluation without server roundtrip
- **Rust feature-flags service** -- Dedicated high-performance flag evaluation service
- **Early Access Features** -- Feature flag gating for beta features

### Experiments / A/B Testing (Core)
- **A/B tests** -- Built on feature flags
- **Experiment exposure tracking** -- Pre-aggregated experiment exposure tables
- **Statistical analysis** -- Bayesian and frequentist
- **Funnel experiments** -- Conversion-based experiments
- **Trend experiments** -- Count-based experiments

### Surveys
- **In-app surveys** -- Configurable survey types
- **Targeting** -- By URL, user properties, events
- **Multiple question types** -- NPS, rating, open text, multiple choice

### Web Analytics
- **Web overview** -- Traffic, sources, pages
- **Web stats tables** -- Detailed breakdown tables
- **Web trends** -- Time-series web metrics
- **Web goals** -- Conversion tracking
- **Session attribution** -- Full UTM/referrer attribution
- **Pre-aggregated tables** -- Hourly/daily rollups for fast queries (web_pre_aggregated_stats, web_pre_aggregated_bounces)

### Data Platform
- **Data Warehouse** -- External data source ingestion (Stripe, Hubspot, Postgres, S3, BigQuery, Snowflake, etc.)
- **Batch Exports** -- Export to S3, BigQuery, Snowflake, Postgres, Redshift via Temporal workflows
- **Data Modeling** -- Views, joins, saved queries
- **Data Pipeline (CDP)** -- Real-time event transformations, destinations (webhooks, Slack, etc.)
- **Hog Functions** -- Custom transformation functions in "Hog" language

### Error Tracking
- **Exception ingestion** -- Dedicated Kafka topic and Rust cymbal service
- **Stack frame symbolication** -- Source map/symbol set processing
- **Issue grouping** -- Fingerprint-based with override rules
- **Document embeddings** -- Vector similarity for issue deduplication
- **Spike detection** -- Configurable anomaly detection

### LLM Analytics / Observability
- **AI capture endpoint** -- Dedicated `/i/v0/ai` route with S3 blob storage
- **Trace/span visualization** -- OpenTelemetry-compatible
- **LLM model tracking** -- Token usage, cost, latency
- **Evaluation framework** -- Dataset management, evaluation configs, scoring
- **Clustering** -- Automatic topic clustering of LLM interactions
- **Sentiment analysis** -- Automated sentiment scoring
- **Review queues** -- Human review workflow

### Logs
- **Log ingestion** -- Dedicated `capture-logs` Rust service on OTLP protocol (port 4318)
- **Log attributes** -- Materialized views for attribute indexing
- **Resource attributes** -- Separate MV for resource-level attributes

### Other Products
- **Cohorts** -- Static and dynamic (behavioral, property-based, real-time). Stored in Postgres with ClickHouse materialized membership tables.
- **Persons** -- Full person profiles with property merging, distinct ID management
- **Groups** -- Multi-level entity grouping (companies, teams, etc.) -- up to 5 group types
- **Actions** -- Named event combinations for reuse
- **Notebooks** -- Collaborative analysis documents
- **Alerts** -- Threshold-based alerting on insights
- **Heatmaps** -- Click/scroll heatmaps with screenshots
- **Revenue Analytics** -- Revenue tracking and analysis
- **Marketing Analytics** -- Campaign performance tracking
- **Conversations** -- In-app messaging
- **Product Tours** -- Guided user onboarding
- **Games** -- Gamification features
- **MCP Store** -- Model Context Protocol tool marketplace
- **Signals** -- Event-driven signal pipeline
- **Visual Review** -- Visual diff review system

---

## 3. Data Model / Database Schema

### What Lives Where

**PostgreSQL (Django ORM):**
- Organizations, Teams, Users, API Keys
- Feature flag definitions, experiments
- Cohort definitions (filters/rules -- NOT membership)
- Dashboard definitions, insights, annotations
- Plugins, hog functions, integrations
- Batch export configurations
- Person profiles (now in separate partitioned DB: `posthog_person_new` with 64 hash partitions by team_id)
- PersonDistinctId mappings
- PersonOverride records

**ClickHouse (Analytics):**
- ALL event data
- Person records (replicated from Postgres via Kafka)
- PersonDistinctId2 (replicated from Postgres via Kafka)
- PersonDistinctIdOverrides (for merge tracking)
- Groups
- Sessions (aggregated from events)
- Session replay metadata
- Heatmaps
- Performance events
- App metrics
- Ingestion warnings
- Error tracking fingerprints + embeddings
- Log entries
- Web pre-aggregated stats
- Query log archive
- Cohort membership (materialized)

### ClickHouse Events Table

The core events table uses **ReplacingMergeTree** with version column `_timestamp`:

```sql
CREATE TABLE sharded_events ON CLUSTER '...'
(
    uuid UUID,
    event VARCHAR,
    properties VARCHAR CODEC(ZSTD(3)),        -- JSON blob, ZSTD compressed
    timestamp DateTime64(6, 'UTC'),
    team_id Int64,
    distinct_id VARCHAR,
    elements_chain VARCHAR,
    created_at DateTime64(6, 'UTC'),
    person_id UUID,
    person_created_at DateTime64,
    person_properties VARCHAR CODEC(ZSTD(3)),  -- denormalized at ingestion time
    group0_properties VARCHAR CODEC(ZSTD(3)),  -- up to 5 group property columns
    group1_properties VARCHAR CODEC(ZSTD(3)),
    group2_properties VARCHAR CODEC(ZSTD(3)),
    group3_properties VARCHAR CODEC(ZSTD(3)),
    group4_properties VARCHAR CODEC(ZSTD(3)),
    group0_created_at DateTime64,
    group1_created_at DateTime64,
    group2_created_at DateTime64,
    group3_created_at DateTime64,
    group4_created_at DateTime64,
    person_mode Enum8('full' = 0, 'propertyless' = 1, 'force_upgrade' = 2),
    historical_migration Bool,
    -- 10 each of: dmat_string_0..9, dmat_numeric_0..9, dmat_bool_0..9, dmat_datetime_0..9
    -- (dynamically materialized columns for property extraction)
    _timestamp DateTime,   -- Kafka metadata
    _offset UInt64,
    inserted_at Nullable(DateTime64(6, 'UTC')) DEFAULT NOW64(),
    consumer_breadcrumbs Array(String)
)
ENGINE = ReplicatedReplacingMergeTree(...)
PARTITION BY toYYYYMM(timestamp)
ORDER BY (team_id, toDate(timestamp), event, cityHash64(distinct_id), cityHash64(uuid))
SAMPLE BY cityHash64(distinct_id)
```

**Key design decisions:**
- **Properties are a JSON VARCHAR column**, not individual columns. Queried via `JSONExtractRaw()`.
- **Materialized columns** extract frequently-queried properties at write time (e.g., `$group_0` through `$group_4`, `$window_id`, `$session_id`). These use `MATERIALIZED` expressions with `JSONExtractRaw`.
- **Dynamically materialized columns** (`dmat_string_0` through `dmat_datetime_9`) -- 40 slots for customer-specific property materialization. These are written at ingestion time by the plugin server.
- **Person and group properties are denormalized** onto each event at ingestion time. This avoids JOINs at query time.
- **ZSTD(3) compression** on properties columns.
- **Partitioned by month** (`toYYYYMM(timestamp)`).
- **Ordered by** `(team_id, toDate(timestamp), event, cityHash64(distinct_id), cityHash64(uuid))` -- team_id first for multi-tenant isolation.
- **SAMPLE BY** `cityHash64(distinct_id)` for approximate queries.
- **events_recent** table -- A separate TTL-based table (7 day TTL) partitioned by `toStartOfDay(inserted_at)` for fast recent-data queries. Events are fanned out to this table via a materialized view on the sharded_events table.

### Events Table Pattern (Kafka -> ClickHouse)

For each data type, PostHog uses a 4-table pattern:
1. **kafka_\<table\>** -- Kafka engine table that reads from a topic
2. **\<table\>_mv** -- Materialized view that transforms kafka table data
3. **writable_\<table\>** -- Distributed table for writing (routes to sharded table)
4. **sharded_\<table\>** -- The actual ReplicatedMergeTree/AggregatingMergeTree table

Plus a `<table>` distributed table for reading across all shards.

### Persons Table

```sql
CREATE TABLE person ON CLUSTER '...'
(
    id UUID,
    created_at DateTime64,
    team_id Int64,
    properties VARCHAR,       -- JSON blob
    is_identified Int8,
    is_deleted Int8,
    version UInt64,           -- for ReplacingMergeTree dedup
    last_seen_at Nullable(DateTime64)
)
ENGINE = ReplicatedReplacingMergeTree(..., version)
ORDER BY (team_id, id)
```

### Person Distinct ID Mapping

```sql
CREATE TABLE person_distinct_id2 ON CLUSTER '...'
(
    team_id Int64,
    distinct_id VARCHAR,
    person_id UUID,
    is_deleted Int8,
    version Int64
)
ENGINE = ReplicatedReplacingMergeTree(..., version)
ORDER BY (team_id, distinct_id)
SETTINGS index_granularity = 512
```

### Person Distinct ID Overrides

Used during person merges. When two persons merge, the old distinct_id -> person_id mapping needs updating. Overrides track these pending changes:

```sql
-- Same schema as person_distinct_id2
-- WHERE version > 0 -- only stores UPDATED rows, not newly inserted ones
-- Used by a dictionary for fast lookup during query time
CREATE DICTIONARY person_distinct_id_overrides_dict (
    team_id Int64,
    distinct_id String,
    person_id UUID
)
PRIMARY KEY team_id, distinct_id
SOURCE(CLICKHOUSE(
    query 'SELECT team_id, distinct_id, argMax(person_id, version) AS person_id
           FROM person_distinct_id_overrides GROUP BY team_id, distinct_id'
))
LAYOUT(complex_key_hashed())
LIFETIME(MIN 3600 MAX 18000)  -- reloads every 1-5 hours
```

### Sessions Table (V1 - AggregatingMergeTree)

Legacy table, only for grandfathered team IDs. Uses AggregatingMergeTree:

```sql
-- Key columns:
session_id VARCHAR,
team_id Int64,
distinct_id SimpleAggregateFunction(any, String),
min_timestamp SimpleAggregateFunction(min, DateTime64(6, 'UTC')),
max_timestamp SimpleAggregateFunction(max, DateTime64(6, 'UTC')),
urls SimpleAggregateFunction(groupUniqArrayArray, Array(String)),
entry_url AggregateFunction(argMin, String, DateTime64(6, 'UTC')),
exit_url AggregateFunction(argMax, String, DateTime64(6, 'UTC')),
-- UTM parameters, ad click IDs, etc. as AggregateFunction(argMin, ...)
event_count_map SimpleAggregateFunction(sumMap, Map(String, Int64)),
pageview_count SimpleAggregateFunction(sum, Int64),
autocapture_count SimpleAggregateFunction(sum, Int64),
```

### Raw Sessions Table (V2 - Current)

Much richer schema, also AggregatingMergeTree:

```sql
-- Uses UInt128 session_id_v7 instead of VARCHAR
-- Adds device info: browser, OS, device_type, viewport
-- Adds GeoIP: country_code, subdivision, city, timezone
-- Adds all attribution params
-- Adds pageview_uniq/autocapture_uniq/screen_uniq (AggregateFunction(uniq, Nullable(UUID)))
-- Adds page_screen_autocapture_uniq_up_to (for bounce detection)
-- Adds vitals_lcp (web vitals)
-- Adds maybe_has_session_replay (Bool)
```

### Groups Table

```sql
CREATE TABLE groups (
    group_type_index UInt8,   -- 0-4 for up to 5 group types
    group_key VARCHAR,
    created_at DateTime64,
    team_id Int64,
    group_properties VARCHAR  -- JSON
)
ENGINE = ReplicatedReplacingMergeTree(..., _timestamp)
ORDER BY (team_id, group_type_index, group_key)
```

### Cohort Storage

Cohort **definitions** live in PostgreSQL (`posthog_cohort` Django model). They store filter rules as JSON.

Cohort **membership** is materialized in ClickHouse for query-time use:

```sql
CREATE TABLE cohort_membership (
    team_id Int64,
    cohort_id Int64,
    person_id UUID,
    version Int64
)
```

Static cohorts use `person_static_cohort` table in ClickHouse.

---

## 4. Ingestion Pipeline

This is the most complex part of PostHog's architecture. Here is the complete flow:

### Stage 1: SDK -> Capture Service (Rust)

**Endpoints:**
- `/e`, `/capture`, `/track`, `/engage`, `/i/v0/e` -- Analytics events (2MB body limit)
- `/batch` -- Batch events (20MB body limit)
- `/s` -- Session recordings (25MB body limit)
- `/i/v0/ai` -- AI/LLM events (25MB+)
- `/flags` -- Feature flag evaluation (separate Rust service)

**Capture service** (`rust/capture/`):
- Written in Rust with Axum
- Receives HTTP POST, decompresses (gzip/lz4/snappy supported)
- Extracts API token from payload or query params
- Validates token against Redis
- Applies rate limiting (per-token, per-token+distinct_id, global)
- Applies quota limiting (billing limits checked via Redis)
- Applies event restrictions (allowlist/blocklist via Redis)
- Classifies events by type: `AnalyticsMain`, `AnalyticsHistorical`, `ExceptionMain`, `HeatmapMain`, `ClientIngestionWarning`
- Reroutes historical events (older than threshold) to historical topic
- Produces to Kafka topic `events_plugin_ingestion` (or overflow/historical variants)

**Kafka Topics from Capture:**
- `events_plugin_ingestion` -- Main analytics events
- `events_plugin_ingestion_overflow` -- Overflow events (rate-limited tokens)
- `events_plugin_ingestion_historical` -- Historical backfill events
- `session_recording_snapshot_item_events` -- Session recordings
- `exceptions_ingestion` -- Error tracking events
- `heatmaps_ingestion` -- Heatmap data
- `logs_ingestion` -- OTLP logs

### Stage 2: Plugin Server / Ingestion Consumer (Node.js)

The Node.js "plugin server" (`nodejs/`) consumes from Kafka and runs the ingestion pipeline:

**Pipeline Steps (in order):**
1. **Pre-team preprocessing** -- Initial parsing
2. **Normalize event** -- Standardize event shape, extract properties
3. **Normalize process person flag** -- Determine if person processing is needed
4. **Hog transform event** -- Run customer-defined Hog transformations (CDP)
5. **Disable person processing** (conditional) -- Skip person creation for `$process_person_profile=false`
6. **Check heatmap opt-in** (conditional)
7. **Extract heatmap data** (conditional)
8. **Process groups** -- Create/update group entities, denormalize group properties
9. **Process persons** -- Create/update person, handle merges, denormalize person properties
10. **Prepare event** -- Final event preparation
11. **Create event** -- Construct the `ProcessedEvent` object
12. **Emit event** -- Produce to `clickhouse_events_json` Kafka topic
13. **Flush batch stores** -- Flush buffered person/group writes

**Person Processing (process-persons-step.ts):**
- Uses `PersonEventProcessor`, `PersonPropertyService`, `PersonMergeService`
- Handles `$identify` events (merge anonymous -> identified)
- Handles `$alias` events
- Manages merge modes: sync batch merge, async merge
- Writes person updates to Postgres and produces to `clickhouse_person` Kafka topic
- Writes distinct ID mappings to Postgres and produces to `clickhouse_person_distinct_id` Kafka topic

**Cookieless Tracking (cookieless-manager.ts):**
- Hash-based distinct IDs: `hash(daily_salt + team_id + ip + root_domain + user_agent)`
- Daily salt stored in Redis (128-bit random value per calendar day per timezone)
- Salt automatically deleted after expiry -- impossible to reverse
- Two modes: stateless (one session per day) and stateful (with session timeout via Redis)

### Stage 3: Kafka -> ClickHouse

ClickHouse has **Kafka engine tables** that directly consume from topics:

```
kafka_events_json (Kafka engine)
  -> events_json_mv (Materialized View)
    -> writable_events (Distributed)
      -> sharded_events (ReplicatedReplacingMergeTree)
```

Each MV does a `SELECT ... FROM kafka_table` and inserts into the target table. The MVs also fan data to the `events_recent` table for fast recent-data queries.

**Consumer groups** are configured per-table (e.g., `clickhouse-events-json`, `clickhouse-person-distinct-id-overrides`).

`kafka_skip_broken_messages = 100` is set to prevent poison pills from blocking ingestion.

### Stage 4: Background Workers

- **Celery workers** -- Handle async tasks (cohort calculations, exports, cleanup)
- **Temporal workers** -- Handle batch exports, person deletion, data pipelines
- **Property-defs-rs** (Rust) -- Consumes from Kafka, writes property definitions to Postgres
- **Cymbal** (Rust) -- Error tracking: consumes `exceptions_ingestion`, does stack frame symbolication
- **Cyclotron-janitor** (Rust) -- Cleans up CDP job queue in Postgres

### Backpressure Handling

- **Overflow mechanism**: When a token exceeds rate limits, events are redirected from `events_plugin_ingestion` to `events_plugin_ingestion_overflow`. A separate consumer processes overflow with lower priority.
- **Kafka buffering**: Kafka producer has configurable queue size (400 MiB default), message timeout (20s), and batch settings.
- **ClickHouse materialized views**: If ClickHouse slows down, Kafka consumers pause (built-in backpressure from Kafka engine tables).
- **Dead letter queue**: Failed events go to `events_dead_letter_queue` topic and a corresponding ClickHouse table.
- **S3 fallback**: Capture service can fall back to writing events to S3 if Kafka is unavailable.

---

## 5. Query Patterns / Query Engine

### HogQL -- PostHog's Query Language

PostHog built **HogQL**, a custom SQL dialect that compiles to ClickHouse SQL. It provides:

- A complete SQL parser (ANTLR4 grammar in `posthog/hogql/grammar/`)
- An AST representation (`posthog/hogql/ast.py`)
- A resolver that maps table/column references to the actual ClickHouse schema
- A printer that outputs ClickHouse SQL
- Support for variables, placeholders, filters
- Pre-aggregated table transformations (automatic routing to rollup tables when applicable)

**Key files:**
- `posthog/hogql/parser.py` -- Parse HogQL string to AST
- `posthog/hogql/resolver.py` -- Resolve references, type-check
- `posthog/hogql/printer.py` -- AST to ClickHouse SQL string
- `posthog/hogql/database/` -- Virtual schema definitions (maps HogQL table names to actual ClickHouse tables)
- `posthog/hogql/query.py` -- Execute HogQL queries (also supports direct Postgres connections)

### Query Runners

Each insight type has a dedicated query runner in `posthog/hogql_queries/insights/`:

**TrendsQueryRunner** (`trends/trends_query_runner.py`):
- Builds HogQL AST for time-series aggregation
- Uses `TrendsQueryBuilder` to construct the SELECT
- Supports breakdowns, formulas between series, compare periods
- Executes via `execute_hogql_query()` which prints to ClickHouse SQL and runs

**FunnelsQueryRunner** (`funnels/funnels_query_runner.py`):
- Uses `FunnelUDF` (ClickHouse User Defined Function) for funnel computation
- UDF written in Rust (`funnel-udf/`) and loaded into ClickHouse as `user_scripts`
- Also has `FunnelTrendsUDF` and `FunnelTimeToConvertUDF`
- Uses `MAX_BYTES_BEFORE_EXTERNAL_GROUP_BY` to prevent OOM

**RetentionQueryRunner**, **PathsQueryRunner**, **StickinessQueryRunner**, **LifecycleQueryRunner** -- Each with their own HogQL AST generation.

### Unique User Counting

- **Exact counting** by default: `count(DISTINCT person_id)` or `uniqExact(person_id)` for precise counts
- **HyperLogLog available**: ClickHouse `uniq()` uses HyperLogLog internally when called
- **Preaggregation**: The `preaggregation_results` table stores `AggregateFunction(uniqExact, UUID)` states for intermediate results
- **Bounce detection**: Uses `uniqUpTo(1)` aggregate function -- counts unique values up to threshold, perfect for "had less than 2 events" check

### Time-Series Aggregations

- Events are partitioned by month, ordered by `(team_id, toDate(timestamp), event, ...)`
- Trends queries generate a date axis using `numbers()` function, then LEFT JOIN with actual data
- ClickHouse's `toStartOfHour`, `toStartOfDay`, `toStartOfWeek`, `toStartOfMonth` for bucketing
- Formula support via `FormulaAST` class that evaluates mathematical expressions across multiple series

### Materialized Views / Pre-aggregation

- **Sessions tables** (AggregatingMergeTree) -- Materialized from events via MV. Use `argMin`/`argMax` state functions for first/last values, `SimpleAggregateFunction(sum, ...)` for counts.
- **Raw sessions v2/v3** -- More detailed aggregations including device, geo, attribution
- **Web pre-aggregated stats** -- Daily/hourly rollup tables for web analytics. Built by Dagster pipelines, using `REPLACE PARTITION` for atomic updates.
- **Experiment exposures** -- Pre-aggregated for fast experiment queries
- **Query log archive** -- MV from ClickHouse system query_log for query monitoring

### ClickHouse-Specific Features Used

- **ReplacingMergeTree** -- Events, persons, distinct IDs (dedup on version)
- **AggregatingMergeTree** -- Sessions, web pre-aggregated tables
- **CollapsingMergeTree** -- Legacy person_distinct_id table
- **Distributed tables** -- Cross-shard reads/writes
- **Dictionaries** -- `person_distinct_id_overrides_dict` (hashed, reloads hourly), `channel_definition`, `exchange_rate`
- **Materialized columns** -- Extracted from JSON properties at write time
- **ZSTD compression** -- On properties columns (level 3)
- **Minmax indexes** -- On group IDs, session IDs, window IDs
- **SAMPLE BY** -- For approximate queries on large datasets
- **Kafka engine** -- Direct Kafka-to-ClickHouse consumption
- **UDFs** -- Rust-based funnel computation loaded as user scripts

---

## 6. The ClickHouse Migration Story

### Why They Migrated

PostHog originally stored all events in PostgreSQL. The migration to ClickHouse happened around 2020-2021. Key reasons found in the codebase:

1. **Event volume scaling** -- PostgreSQL couldn't handle the write throughput needed for analytics events. The sheer volume of `INSERT` statements overwhelmed PostgreSQL.

2. **Query performance** -- Analytical queries (COUNT, GROUP BY over millions of rows with time ranges) are fundamentally better suited to columnar storage. PostgreSQL row-oriented storage required scanning entire rows even when only accessing 1-2 columns.

3. **Compression** -- Event properties as JSON in PostgreSQL are storage-expensive. ClickHouse's columnar compression (ZSTD) provides 10-20x better compression ratios.

4. **Multi-tenant isolation** -- With `ORDER BY (team_id, ...)`, ClickHouse can skip entire data blocks for other teams. PostgreSQL indexes work differently and don't provide the same locality.

### What They Kept in PostgreSQL

PostHog kept **all OLTP data** in PostgreSQL:
- User accounts, organizations, teams
- Feature flag definitions (but evaluation moved to Rust service reading from Postgres)
- Cohort definitions
- Dashboard/insight configurations
- Person profiles (recently moved to a dedicated partitioned Postgres instance)

The key insight: **PostgreSQL is kept for entities that are read/written individually** (CRUD operations). **ClickHouse is used for entities that are written in bulk and queried as aggregates**.

### ClickHouse Features That Were Critical

1. **ReplacingMergeTree** -- Allows "upsert" semantics for persons (newer version replaces older). Events use it for idempotent ingestion.
2. **AggregatingMergeTree** -- Pre-aggregates sessions from raw events. The merge process combines partial aggregation states.
3. **Kafka engine** -- Eliminates the need for a separate consumer service for most tables. ClickHouse itself consumes directly from Kafka.
4. **Columnar compression** -- `properties VARCHAR CODEC(ZSTD(3))` provides massive storage savings.
5. **Materialized views** -- Automatic data transformation at write time (events -> sessions, events -> heatmaps, etc.)
6. **Distributed tables** -- Horizontal scaling with sharding.
7. **Approximate functions** -- `uniq()` (HyperLogLog), `uniqUpTo()`, `SAMPLE BY` for fast approximate queries.

### Relevance to Nucleus vs. ClickHouse Decision

If building a ClickHouse competitor (Nucleus), the features that MUST be matched:
- Columnar storage with per-column compression
- Efficient JSON property extraction (or a better property storage model)
- AggregatingMergeTree-equivalent (incremental aggregation during compaction)
- ReplacingMergeTree-equivalent (version-based deduplication)
- Kafka-native ingestion (or equivalent streaming ingestion)
- Materialized views (automatic write-time transformations)
- Distributed query execution across shards

---

## 7. Architecture

### Service Topology

```
                    +-----------+
                    |   Caddy   |  (reverse proxy)
                    +-----+-----+
                          |
          +-------+-------+-------+-------+--------+
          |       |       |       |       |        |
     capture  replay  capture  feature  plugins   web
     (Rust)  capture    -ai    -flags  (Node.js) (Django)
              (Rust)  (Rust)  (Rust)
          |       |       |       |       |        |
          +-------+---+---+-------+-------+--------+
                      |
                   Kafka
                  (Redpanda)
                      |
          +-----------+-----------+
          |           |           |
     ClickHouse    Plugin      Workers
     (Kafka engine) Server    (Celery)
                  (Node.js)
                      |           |
                   Postgres    Temporal
                   + Redis    (workflows)
```

### Monolith vs. Microservices

PostHog is a **modular monolith with extracted services**:

- **Django monolith** -- Core web app, API, query execution. Single deployable.
- **Node.js plugin server** -- Extracted event processing pipeline. Runs as a separate process but shares the same repo.
- **Rust services** -- Multiple independent binaries compiled from a shared Cargo workspace:
  - `capture` -- Event capture (runs in 3 modes: events, recordings, AI)
  - `capture-logs` -- OTLP log ingestion
  - `feature-flags` -- Feature flag evaluation
  - `property-defs-rs` -- Property definition syncing
  - `cymbal` -- Error tracking symbolication
  - `cyclotron-janitor` -- CDP job cleanup
  - `personhog-leader/replica/router` -- Person management service (newer architecture)
- **Go livestream** -- WebSocket-based live event streaming

### How Real-Time Works

1. **Live events** -- The Go `livestream` service consumes from Kafka and streams events to connected WebSocket clients. Filtered by team.
2. **Session replay** -- Recording data goes through `replay-capture` (Rust) -> Kafka -> Node.js session recording consumer -> SeaweedFS (S3). Playback is served from S3 via the Node.js plugin server.
3. **Feature flags** -- Evaluated by the Rust `feature-flags` service reading directly from Postgres and Redis. Sub-millisecond evaluation.

### Caching Layer (Redis Usage)

Redis serves multiple roles:
- **Celery broker** -- Task queue for background jobs
- **Team cache** -- Cached team configurations for fast lookup in capture/ingestion
- **Feature flag cache** -- Pre-computed flag states (dedicated Redis DB recommended)
- **Rate limiting** -- Token-level and token+distinct_id rate counters
- **Cookieless salts** -- Daily cryptographic salts per timezone
- **Session recording state** -- Session metadata for the recording API
- **Overflow state** -- Tracks which tokens are in overflow mode
- **CDP state** -- Customer data pipeline runtime state
- **Query caching** -- Insight result caching

### Horizontal Scaling

- **Capture service** -- Stateless, scale by adding instances behind load balancer
- **Plugin server** -- Kafka consumer groups, scale by adding consumer instances (partition-based)
- **ClickHouse** -- Sharded via Distributed tables. `sipHash64(distinct_id)` sharding key ensures all events for a user land on the same shard.
- **PostgreSQL** -- Person table uses 64 hash partitions by team_id. Read replicas for flag evaluation.
- **Redis** -- Separate instances for different concerns (flags, rate limiting, general cache). Reader replicas for read-heavy paths.

---

## 8. SDK / Client Libraries

### JavaScript SDK

PostHog's JS SDK (`posthog-js`) is the primary client library. Key behaviors:

**Automatic Capture:**
- **Autocapture** -- Automatically captures clicks, form submissions, and changes on interactive elements (`a`, `button`, `form`, `input`, `select`, `textarea`, `label`). Stores an `elements_chain` that encodes the DOM path.
- **Pageview** (`$pageview`) -- Automatic on SPA route changes
- **Pageleave** (`$pageleave`) -- On page unload
- **Performance events** -- Web Vitals (LCP, FID, CLS)
- **Session recording** -- Full DOM replay via rrweb integration
- **Heatmap data** -- Mouse position, clicks, scrolls

**Manual Events:**
- `posthog.capture('event_name', { properties })` -- Custom events
- `posthog.identify('user_id', { properties })` -- Link anonymous to identified
- `posthog.alias('alias_id')` -- Create identity alias
- `posthog.group('company', 'company_id', { properties })` -- Group identification
- `posthog.setPersonProperties({ ... })` -- Update person properties

**Batching and Retry:**
- Events are queued in memory and sent in batches
- Default flush interval: 500ms (configurable)
- Batch endpoint: `/batch/` (20MB limit)
- Retry with exponential backoff on failure
- Beacon API used for page unload events (no retry possible)

**Feature Flags:**
- Evaluated via `/flags` endpoint (goes to Rust service)
- Cached locally
- Auto-refresh on identify

**Cookieless Mode:**
- When enabled, no cookies are set
- Server-side generates distinct_id from hash of IP + UA + domain + daily salt
- Session ID also generated server-side

### Elements Chain

Autocapture events include `elements_chain`, a semicolon-delimited string encoding the DOM path:

```
a.nav-link.active:attr_id="home":href="/":nth-child="1":nth-of-type="1":text="Home";
div.nav-item:nth-child="1":nth-of-type="1"
```

ClickHouse has materialized columns that extract:
- `elements_chain_href` -- Extracted href
- `elements_chain_texts` -- Array of text values
- `elements_chain_ids` -- Array of IDs
- `elements_chain_elements` -- Array of element types (Enum)

---

## 9. Privacy / Compliance

### GDPR Handling

**Data Deletion Pipeline:**
- Temporal workflow `delete_persons_workflow` handles person deletion
- Deletes from PostgreSQL: `posthog_persondistinctid`, `posthog_personoverride`, `posthog_cohortpeople`, `posthog_person`
- ClickHouse deletion uses async deletion system (`posthog/models/async_deletion/`):
  - `delete_events.py` -- Marks events for deletion
  - `delete_person.py` -- Marks person records for deletion
  - `delete_cohorts.py` -- Marks cohort membership for deletion
  - Uses `ALTER TABLE DELETE` in ClickHouse (lightweight deletes since ClickHouse 22.8)
- Session recording deletion via Temporal workflow `delete_recordings`

**Async Deletion Model:**
Deletion requests are queued in PostgreSQL (`AsyncDeletion` model) and processed by background workers that execute ClickHouse DELETE statements.

### Cookie vs. Cookieless Tracking

**Cookie-based (default):**
- Stores a `distinct_id` cookie in the browser
- Persistent across sessions until cookie expires or is cleared

**Cookieless server-side hashing:**
- No cookies set at all
- Distinct ID = `hash(daily_salt + team_id + IP + root_domain + user_agent + optional_extra)`
- Daily salt is a 128-bit random value stored in Redis
- Salt is rotated daily and deleted after expiry
- Once salt is gone, it's impossible to reverse the hash to get PII
- Supports timezone-aware day boundaries
- Two modes: stateless (one session per calendar day) and stateful (with 30-min session timeout via Redis)

### IP Anonymization

- GeoIP lookup happens at ingestion time (GeoLite2-City database)
- IP can be stripped from properties based on team configuration
- Internally-generated events (from `capture_internal`) have IP forced to `127.0.0.1`

### Property Filtering

- Event restrictions can be configured per-token to allowlist/blocklist specific events or pipelines
- `$process_person_profile=false` flag on events skips all person processing

---

## 10. API Design

### Capture API

**Event Capture:**
```
POST /e  (or /capture, /track, /batch, /i/v0/e)
Content-Type: application/json

{
    "api_key": "phc_...",
    "event": "$pageview",
    "distinct_id": "user123",
    "properties": {
        "$current_url": "https://example.com/page",
        "$browser": "Chrome",
        ...
    },
    "timestamp": "2026-03-20T12:00:00Z"
}
```

**Batch:**
```
POST /batch
{
    "api_key": "phc_...",
    "batch": [
        { "event": "...", "distinct_id": "...", "properties": {...}, "timestamp": "..." },
        ...
    ]
}
```

**Response:**
```json
{
    "status": 1  // or 0 for failure
}
```

**Query Parameters:**
- `v` -- Library version
- `compression` -- gzip/lz4/etc.
- `_` -- Beacon mode flag

### Query API

Insights are queried via HogQL:
```
POST /api/projects/{project_id}/query/
{
    "kind": "TrendsQuery",
    "series": [
        {
            "kind": "EventsNode",
            "event": "$pageview",
            "math": "total"
        }
    ],
    "dateRange": {
        "date_from": "-7d"
    },
    "interval": "day"
}
```

Or raw HogQL:
```
POST /api/projects/{project_id}/query/
{
    "kind": "HogQLQuery",
    "query": "SELECT count() FROM events WHERE event = '$pageview' AND timestamp > now() - INTERVAL 7 DAY"
}
```

### Authentication

**Three key types:**

1. **Project API Key** (`phc_...`) -- Used in SDKs for event capture. Public, included in frontend JS. Only allows event ingestion.

2. **Personal API Key** -- Used for the REST API. Hashed with SHA-256 (migrated from PBKDF2). Stored in `posthog_personalapikey` table. Scoped to user with team-level access control.

3. **OAuth 2.0** -- Django OAuth Toolkit integration for third-party app authorization.

API keys are prefixed for easy identification:
- `phc_` -- Project API keys
- Personal API keys have a `phx_` prefix

---

## 11. Strengths and Weaknesses

### Strengths

1. **Self-contained analytics stack** -- One product replaces Google Analytics, Mixpanel, Amplitude, LaunchDarkly, Hotjar, and parts of Sentry. Massive value proposition.

2. **ClickHouse expertise** -- They've deeply understood and leveraged ClickHouse's strengths. The 4-table pattern (kafka/mv/writable/sharded), AggregatingMergeTree for sessions, materialized columns for hot properties, and the person override dictionary are all well-engineered.

3. **HogQL** -- Building their own SQL dialect that compiles to ClickHouse SQL is brilliant. It lets them abstract away ClickHouse-specific complexity, add virtual tables/columns, and evolve the schema without breaking user queries.

4. **Rust capture service** -- Moving the hot path (event ingestion) to Rust was the right call. The capture service handles all the rate limiting, quota enforcement, and Kafka production with minimal latency.

5. **Person merge system** -- The combination of `person_distinct_id2`, `person_distinct_id_overrides`, and the override dictionary is a sophisticated solution to the anonymous-to-identified merge problem. The version-based ReplacingMergeTree approach handles concurrent updates gracefully.

6. **Cookieless tracking** -- Server-side hash-based tracking with rotating salts is a genuinely clever privacy solution that doesn't require cookies or local storage.

7. **Pre-aggregated tables** -- Sessions, web stats, and experiment exposures are pre-aggregated at write time, making reads fast. The web pre-aggregation system uses partition replacement for atomic updates.

8. **Product architecture** -- The `products/` directory structure with isolated frontend/backend per product, enforced by `tach` import boundaries, is a good modular monolith pattern.

### Weaknesses / Over-Engineering

1. **Massive complexity** -- The codebase is enormous. 50+ products, 3 languages for services (Python/TypeScript/Rust/Go), multiple databases, Kafka, Temporal, Dagster, Celery. The operational overhead of running this is significant.

2. **Events table denormalization** -- Person properties and group properties (5 sets!) are denormalized onto every single event row. This means property changes require either re-processing events or accepting stale data. They compensate with the `person_mode` enum and various override mechanisms, but it's complex.

3. **Multiple session table versions** -- V1, V2, V3 session tables coexist, with a hardcoded `ALLOWED_TEAM_IDS` list for V1. Migration debt is visible.

4. **40 dynamically materialized columns** -- The `dmat_string_0` through `dmat_datetime_9` columns on every event are a workaround for ClickHouse's lack of native JSON column type (pre-25.x). It works but is inelegant.

5. **Dual person storage** -- Persons live in both PostgreSQL (source of truth) and ClickHouse (replicated via Kafka for query-time JOINs). The sync is eventually consistent, and the `person_distinct_id_overrides_dict` dictionary only refreshes every 1-5 hours.

6. **Plugin server complexity** -- The Node.js ingestion pipeline has grown organically. The step-based pipeline (`normalize-event-step`, `process-persons-step`, `emit-event-step`, etc.) has many conditional branches and the "batch pipeline" abstraction adds cognitive overhead.

7. **ZooKeeper dependency** -- Still required for ClickHouse replication coordination, though ClickHouse Keeper is the modern replacement.

8. **Feature sprawl** -- Games, product tours, visual review, conversations, user interviews -- the product surface area is extremely broad. Some of these feel like they dilute focus.

### Design Decisions Worth Copying

1. **Properties as JSON with materialized column extraction** -- Store the full JSON blob, materialize the hot columns. Best of both worlds: schema flexibility + query performance.

2. **4-table Kafka ingestion pattern** -- Kafka engine -> MV -> Distributed -> Sharded. Clean, reliable, leverages ClickHouse native capabilities.

3. **AggregatingMergeTree for sessions** -- Incrementally build session summaries as events arrive. No need for a separate session aggregation job.

4. **Team-first ordering** -- `ORDER BY (team_id, ...)` everywhere. Essential for multi-tenant analytics.

5. **events_recent table with TTL** -- 7-day TTL table ordered by insertion time for fast "last hour" queries. Cheap to maintain, huge UX win.

6. **Person override dictionary** -- Elegant solution for handling person merges at query time without rewriting historical events.

7. **Cookieless hashing** -- Daily rotating salts + server-side hashing. No cookies, no local storage, GDPR-friendly, still gets useful analytics.

8. **Capture API simplicity** -- The capture API is dead simple: POST JSON with `api_key`, `event`, `distinct_id`, `properties`. No complex authentication for ingestion.

### What to Avoid

1. **Don't denormalize person/group properties onto events** -- It creates massive write amplification and staleness issues. Better to JOIN at query time with a fast lookup table.

2. **Don't build 3+ session table versions** -- Design the session schema right from the start with extensibility in mind.

3. **Don't require ZooKeeper** -- Use ClickHouse Keeper or design for shared-nothing architecture.

4. **Don't mix 4 programming languages** -- Pick 2 max (e.g., Rust for hot path, TypeScript for everything else). PostHog's Python/TypeScript/Rust/Go mix creates significant hiring and maintenance burden.

5. **Don't build a custom SQL dialect too early** -- HogQL is powerful but represents a massive investment. Start with raw ClickHouse SQL or a thin query builder.

6. **Don't try to be everything at once** -- PostHog's feature sprawl (games, product tours, visual review, MCP store) dilutes engineering focus. A focused competitor with fewer, better-built features can win.

### Opportunities for a Lighter Alternative

1. **Simpler ingestion pipeline** -- Capture (Rust) -> Kafka -> ClickHouse directly, without the Node.js plugin server middleman for basic analytics. Only invoke processing logic for events that need it.

2. **Better property storage** -- ClickHouse 25.x has experimental JSON column type. Or use a property registry with typed columns instead of JSON extraction.

3. **Single session table design** -- One well-designed AggregatingMergeTree from day one.

4. **Fewer databases** -- Could PostgreSQL + ClickHouse + Redis suffice without Kafka, Temporal, Elasticsearch, ZooKeeper, MinIO, SeaweedFS?

5. **Simpler person model** -- If you don't need anonymous-to-identified merging, the person system becomes dramatically simpler.

6. **Managed ClickHouse** -- ClickHouse Cloud eliminates the need for ZooKeeper, replication management, and shard configuration.

7. **WASM UDFs** -- Instead of Rust UDFs loaded as user scripts, use ClickHouse's WASM UDF support for custom functions.

---

## Appendix: Key File Paths

### Core Schema / SQL
- `/posthog/clickhouse/schema.py` -- Master table creation registry
- `/posthog/models/event/sql.py` -- Events table DDL
- `/posthog/models/person/sql.py` -- Persons + distinct IDs + overrides DDL
- `/posthog/models/sessions/sql.py` -- Sessions V1 DDL
- `/posthog/models/raw_sessions/sessions_v2.py` -- Sessions V2 DDL
- `/posthog/models/raw_sessions/sessions_v3.py` -- Sessions V3 DDL
- `/posthog/models/group/sql.py` -- Groups DDL
- `/posthog/models/web_preaggregated/sql.py` -- Web pre-aggregated tables
- `/posthog/clickhouse/table_engines.py` -- Table engine definitions

### Ingestion Pipeline
- `/rust/capture/src/router.rs` -- Capture service routing
- `/rust/capture/src/v0_endpoint.rs` -- Event/recording handlers
- `/rust/capture/src/events/analytics.rs` -- Analytics event processing
- `/rust/capture/src/config.rs` -- Capture service configuration
- `/nodejs/src/ingestion/ingestion-consumer.ts` -- Main ingestion consumer
- `/nodejs/src/ingestion/event-processing/` -- Pipeline steps
- `/nodejs/src/ingestion/cookieless/cookieless-manager.ts` -- Cookieless tracking
- `/posthog/kafka_client/topics.py` -- All Kafka topic names

### Query Engine
- `/posthog/hogql/` -- HogQL language implementation
- `/posthog/hogql_queries/query_runner.py` -- Base query runner
- `/posthog/hogql_queries/insights/trends/` -- Trends query builder
- `/posthog/hogql_queries/insights/funnels/` -- Funnels query builder
- `/posthog/hogql_queries/web_analytics/` -- Web analytics queries
- `/ee/clickhouse/materialized_columns/columns.py` -- Materialized column management

### Architecture
- `/docker-compose.base.yml` -- Service definitions
- `/docker-compose.hobby.yml` -- Self-hosted deployment
- `/Dockerfile` -- Main application image
- `/docs/internal/monorepo-layout.md` -- Codebase structure
- `/AGENTS.md` -- Development guide

### Privacy / Deletion
- `/posthog/temporal/delete_persons/delete_persons_workflow.py` -- Person deletion
- `/posthog/models/async_deletion/` -- Async ClickHouse deletion
- `/posthog/temporal/delete_recordings/workflows.py` -- Recording deletion
