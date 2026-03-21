# Teploy Observe -- Consolidated Reference Document

**Date:** 2026-03-20
**Source:** Research analysis of Umami v3.0.3, PostHog (2026), Sentry (2026), SignOz (2026)
**Purpose:** Inform all architectural and product decisions for Teploy Observe

---

## 1. Tech Stack Comparison

| | Umami | PostHog | Sentry | SignOz |
|---|---|---|---|---|
| **Backend** | TypeScript (Next.js 15) | Python (Django 4.2) + Rust + Node.js + Go | Python (Django 5.2) + Rust | Go 1.25 |
| **Frontend** | React 19 (Next.js) | React 18 (Kea, Vite) | React (Emotion) | React (Ant Design, Vite) |
| **Analytics DB** | PostgreSQL or ClickHouse | ClickHouse 25.12 | ClickHouse (via Snuba) | ClickHouse 25.5.6 |
| **Metadata DB** | PostgreSQL (Prisma) | PostgreSQL 15 | PostgreSQL | SQLite or PostgreSQL |
| **Message Queue** | Kafka (optional) | Kafka (Redpanda) | Kafka | None |
| **Cache** | Redis (optional) | Redis 7.2 | Redis | Redis (optional) |
| **Task Queue** | None | Celery + Temporal | Celery | None |
| **Deployment** | Single Docker container | Docker Compose (10+ services) | Docker Compose (10+ services) | Docker Compose (4 services) |
| **Binary model** | Node.js app | Multi-service | Multi-service | 2 binaries (signoz + collector) |
| **Languages** | 1 (TypeScript) | 4 (Python, TS, Rust, Go) | 2 (Python, Rust) | 1 (Go) |

### Key Takeaway

Complexity scales with scope. Umami is simplest (1 language, 1 app) but does least. SignOz is the sweet spot for Observe's goals: Go single binary for the query/API service, separate ingestion process, ClickHouse as the sole analytics store. PostHog and Sentry are cautionary tales of complexity growth.

---

## 2. Feature Matrix -- What Users Actually Need

### Analytics (Umami territory)

| Feature | Umami | PostHog | Observe v0.1 | Priority |
|---|---|---|---|---|
| Pageviews, visitors, sessions | Yes | Yes | **Yes** | P0 |
| Time-series charts | Yes | Yes | **Yes** | P0 |
| Top pages, referrers, browsers | Yes | Yes | **Yes** | P0 |
| Geographic breakdown | Yes | Yes | **Yes** | P0 |
| Custom events + properties | Yes | Yes | **Yes** | P0 |
| UTM tracking | Yes | Yes | **Yes** | P0 |
| Real-time dashboard | Yes (polling) | Yes | **Yes** | P1 |
| Funnels | Yes | Yes | Defer | P2 |
| Retention | Yes | Yes | Defer | P2 |
| User identification | Yes | Yes | Defer | P2 |
| Session replay | No | Yes | No | -- |
| Feature flags | No | Yes | No | -- |
| A/B testing | No | Yes | No | -- |

### Error Tracking (Sentry territory)

| Feature | Sentry | PostHog | Observe v0.2 | Priority |
|---|---|---|---|---|
| Crash reports + stack traces | Yes | Yes | **Yes** | P0 |
| Error grouping (fingerprinting) | Sophisticated | Basic | **MVP** | P0 |
| Issue status (open/resolved/ignored) | Yes | Yes | **Yes** | P0 |
| Error frequency + first/last seen | Yes | Yes | **Yes** | P0 |
| Release tracking | Yes | No | Defer | P2 |
| Source map support | Yes | Yes | Defer | P2 |
| Regression detection | Yes | No | Defer | P3 |
| Custom fingerprinting rules | Yes (DSL) | Yes | Defer | P2 |

### APM / Tracing (SignOz territory)

| Feature | SignOz | Sentry | Observe v0.3 | Priority |
|---|---|---|---|---|
| Distributed traces (OTLP) | Yes | Yes | **Yes** | P0 |
| Service map | Yes | No | **Yes** | P1 |
| Latency percentiles | Yes | Yes | **Yes** | P0 |
| Error rates per endpoint | Yes | Yes | **Yes** | P0 |
| Trace waterfall view | Yes | Yes | **Yes** | P1 |
| Metrics (gauges, counters, histograms) | Yes | No | Defer | P2 |
| Log ingestion + search | Yes | Partial | Defer | P3 |

---

## 3. Recommended Phasing

### v0.1 -- Analytics (Umami competitor)
Ship a lightweight, self-hostable web analytics tool. This proves the ingestion pipeline, database choice, and dashboard framework.

**Features:** Pageviews, sessions, unique visitors, top pages, referrers, browsers/OS/devices, geographic breakdown, custom events with properties, UTM tracking, real-time active visitors, time-series charts, basic API.

**Architecture:** Go backend + embedded SPA frontend, single binary. ClickHouse (or Nucleus) for event storage. Lightweight JS tracker script.

**What to copy from Umami:**
- Deterministic cookie-free session IDs (UUID v5 from hash of site + IP + UA + monthly salt)
- Visit ID concept (30-min inactivity timeout)
- `x-umami-cache` JWT header pattern for avoiding redundant lookups
- Tracker script `data-*` attribute configuration
- Channel classification logic at query time

**What to improve over Umami:**
- Write buffering (in-memory ring buffer, batch INSERTs)
- Query caching (Redis or in-memory LRU)
- Rate limiting on ingestion endpoint
- Store custom properties as JSON/Map columns, not EAV
- Proper batch endpoint (actual batch INSERT, not a loop)
- Configurable data retention with automatic rollups

### v0.2 -- Error Tracking (Sentry-lite)
Layer error tracking on top of the existing ingestion pipeline.

**Features:** Error event ingestion (stack traces, breadcrumbs, contexts), MVP error grouping (hash of error_type + in-app frame filenames/functions, fallback to parameterized message, user-overridable fingerprint), issue management (open/resolved/ignored), error frequency charts, first/last seen.

**What to copy from Sentry:**
- GroupHash mapping table (hash -> group)
- MD5 hash of contributing values
- Frame normalization (basename for filenames, strip codegen markers)
- Message parameterization (replace UUIDs, IPs, numbers, URLs with placeholders)
- Envelope wire format for efficient event submission

**What to simplify vs Sentry:**
- Skip app/system variants -- just use in-app frames
- Skip config versioning/transitions
- Skip Snuba -- query ClickHouse directly
- Skip Relay -- accept events directly, add edge proxy later
- One grouping algorithm, not a strategy chain

### v0.3 -- APM / Tracing (SignOz-lite)
Add distributed tracing with OTLP ingestion.

**Features:** OTLP gRPC + HTTP ingestion for traces, span storage, trace waterfall view, service map, latency percentiles (p50/p95/p99), error rates per service/endpoint, trace-to-error correlation.

**What to copy from SignOz:**
- OTLP as the sole tracing ingestion protocol
- Typed Map columns for span attributes (string, number, bool)
- Resource fingerprinting (hash resource attributes, store separately)
- Materialized columns for hot attributes (service.name, http.route)
- `ts_bucket_start` for partition pruning
- AggregatingMergeTree with quantilesState for dependency graph
- Multi-level pre-aggregation (5m, 30m, 1d rollups) with auto table selection
- RED metrics derived from spans (via processing at ingest)

**What to simplify vs SignOz:**
- Schema definitions in the main repo, not a separate repo
- Modular query code from the start (not a 7000-line reader file)
- Use ClickHouse Keeper or avoid ZooKeeper for single-node
- One query builder version, designed for extensibility

---

## 4. Database Patterns to Adopt

### Storage Architecture (from all four tools)

**ClickHouse is the universal choice for observability analytics.** Every tool that hit real scale migrated to it or started with it. PostHog migrated from PostgreSQL when analytics queries couldn't keep up. Sentry migrated from PostgreSQL for the same reason. SignOz started with ClickHouse. Even Umami added it as an option.

### Critical ClickHouse Patterns

**1. Event Table Design (from PostHog)**
```
ENGINE = ReplacingMergeTree(version)
PARTITION BY toYYYYMM(timestamp)
ORDER BY (tenant_id, toDate(timestamp), event_type, cityHash64(session_id), cityHash64(event_id))
SAMPLE BY cityHash64(session_id)
```
- Tenant-first ordering for multi-tenant isolation
- Monthly partitioning balances partition count vs. pruning efficiency
- SAMPLE BY for approximate queries on large datasets

**2. Property Storage (improved from PostHog)**
PostHog stores properties as a JSON VARCHAR with `JSONExtractRaw()`. SignOz uses typed Map columns. For Observe:
- Use typed Map columns: `properties_string Map(LowCardinality(String), String)`, `properties_number Map(LowCardinality(String), Float64)`
- Materialize hot properties as dedicated columns
- This avoids JSON parsing overhead while maintaining schema flexibility

**3. Session Aggregation (from PostHog + Umami)**
```
ENGINE = AggregatingMergeTree
```
- Incrementally build session summaries as events arrive
- Use `argMin(url, timestamp)` for entry page, `argMax(url, timestamp)` for exit page
- Use `SimpleAggregateFunction(sum, Int64)` for pageview counts
- Use `uniqState(UUID)` for unique page counts

**4. Pre-Aggregation / Rollups (from SignOz + Umami)**
- Hourly materialized view for dashboard queries (from Umami's `website_event_stats_hourly`)
- Multi-level rollups for metrics: raw -> 5m -> 30m -> 1d (from SignOz)
- Auto table selection based on query time range
- Partition pruning via `ts_bucket_start`

**5. Resource Fingerprinting (from SignOz)**
- Hash resource attributes (service.name, host.name, etc.) into a fingerprint
- Store resource details in a separate table
- Join via fingerprint -- resources change infrequently, so massive dedup savings

**6. Recent Events Table (from PostHog)**
- Separate table with 7-day TTL, ordered by insertion time
- Materialized view fans events to this table at write time
- "Last hour" and "real-time" queries hit this small, fast table instead of scanning months of data

### Nucleus Evaluation Criteria

If choosing Nucleus over ClickHouse, it must handle these patterns:
- [ ] Columnar storage with per-column compression (ZSTD)
- [ ] MergeTree-equivalent with custom sort orders
- [ ] AggregatingMergeTree-equivalent (incremental aggregation during compaction)
- [ ] ReplacingMergeTree-equivalent (version-based deduplication)
- [ ] Materialized views (automatic write-time transformations)
- [ ] Map column type with key-based access
- [ ] Materialized columns extracted from Map/JSON
- [ ] Time-series partitioning with partition pruning
- [ ] HyperLogLog or equivalent approximate distinct counts
- [ ] 1-5k events/sec sustained write throughput
- [ ] Sub-second aggregation queries over 100M+ rows

---

## 5. Ingestion Pipeline Design

### Lessons from each tool

| Tool | Ingestion Pattern | Lesson |
|---|---|---|
| Umami | HTTP -> direct INSERT (no buffer) | Don't do this -- saturates DB at moderate traffic |
| PostHog | Rust capture -> Kafka -> Node.js processing -> Kafka -> ClickHouse | Effective but over-complex for our scale |
| Sentry | SDK -> Relay (Rust) -> Kafka -> Python processing -> Kafka -> Snuba -> ClickHouse | Battle-tested but too many hops |
| SignOz | OTLP -> OTel Collector (batch processor) -> ClickHouse | Simplest. No Kafka. Batch processor provides buffering |

### Recommended Pipeline for Observe

**v0.1 (Analytics):**
```
Browser JS SDK -> POST /api/v1/events (Go HTTP handler)
  -> In-memory ring buffer (batch by time/size)
  -> Batch INSERT into ClickHouse
  -> Materialized views populate rollup tables
```

No Kafka needed at 1-5k events/sec. The Go process holds a ring buffer and flushes batch INSERTs every N seconds or N events. If the process crashes, the buffer is lost -- acceptable for analytics at this scale.

**v0.2 (Errors):**
```
SDK -> POST /api/v1/errors (envelope format)
  -> Normalize + group (in-process)
  -> Batch INSERT into ClickHouse (error events)
  -> Upsert group/issue metadata (PostgreSQL or ClickHouse)
```

Error events flow through the same batch pipeline but with a synchronous grouping step (hash computation, GroupHash lookup).

**v0.3 (Tracing):**
```
App + OTel SDK -> OTLP gRPC/HTTP (embedded OTel receiver)
  -> Batch processor (10k batch, 10s timeout)
  -> Custom ClickHouse exporter
  -> RED metrics derived from spans (at ingest)
```

Embed an OTLP receiver in the binary (or run a lightweight collector sidecar). Follow SignOz's pattern of deriving RED metrics from spans at ingest time.

### Backpressure Strategy

At 5-10k events/sec burst:
1. Ring buffer with configurable max size (default: 100k events)
2. When buffer is full, apply backpressure: return 429 to clients
3. SDK should retry with exponential backoff (document this in SDK)
4. If ClickHouse is down, buffer to disk WAL (append-only file), replay on recovery
5. No Kafka needed until managed multi-tenant version

---

## 6. Data Retention Strategy

None of the four tools handle this well out of the box. Observe should from day one:

| Data Type | Raw Retention | Rollup Retention | Strategy |
|---|---|---|---|
| Analytics events | 30 days | 1 year (hourly), indefinite (daily) | TTL on raw, MV into rollup tables |
| Error events | 90 days | 1 year (daily counts per group) | TTL on raw, aggregate into group metadata |
| Trace spans | 14 days | 30 days (service-level aggregates) | TTL, pre-aggregated RED metrics kept longer |
| Session summaries | 90 days | 1 year | AggregatingMergeTree with TTL |

All retention should be configurable. ClickHouse TTL handles this natively.

---

## 7. SDK Design (Browser JS)

### Lessons learned

| Tool | SDK Approach | Size | Lesson |
|---|---|---|---|
| Umami | Tiny script, `data-*` config, no cookies | ~2KB | Perfect for analytics. Copy this model. |
| PostHog | Full SDK, autocapture, session replay, feature flags | ~100KB+ | Too heavy. But batching + retry is good. |
| Sentry | Error-focused, breadcrumbs, stack traces, envelope format | ~30KB | Good error capture patterns. |
| SignOz | No SDK -- uses OTel SDKs | N/A | Right for APM. Wrong for analytics. |

### Observe SDK Strategy

**Analytics SDK (v0.1):** Umami-style lightweight script (~3KB).
- `data-*` attribute configuration on script tag
- Cookie-free session tracking (deterministic hash)
- Auto pageview + SPA navigation (History API hooks)
- `observe.track(name, properties)` for custom events
- Batch events in memory, flush every 500ms or on page unload (Beacon API)
- `beforeSend` callback for filtering

**Error SDK (v0.2):** Separate opt-in script (~10KB).
- Global error handler (`window.onerror`, `unhandledrejection`)
- Stack trace capture with source context
- Breadcrumb collection (console, fetch, clicks, navigation)
- Envelope format for efficient wire protocol
- `observe.captureException(error)` for manual capture

**APM (v0.3):** Recommend standard OpenTelemetry SDKs pointing at Observe's OTLP endpoint. No custom SDK needed.

---

## 8. Error Grouping -- Minimum Viable Algorithm

Based on Sentry's architecture, simplified:

```
Event arrives with stack trace:
  1. Extract in-app frames (frames where in_app=true, or all if none marked)
  2. For each frame: normalize filename (basename), normalize function name (strip codegen)
  3. Hash = MD5(error_type + frame1.filename + frame1.function + frame2.filename + ...)
  4. Look up hash in GroupHash table
  5. If match -> assign to existing group
  6. If no match -> create new group

Event arrives without stack trace:
  1. Parameterize message (replace UUIDs, IPs, numbers, URLs, emails with placeholders)
  2. Hash = MD5(error_type + parameterized_message)
  3. Same lookup flow

Override:
  - If event has `fingerprint` field set by SDK, use MD5(fingerprint) instead
```

This covers 80%+ of grouping scenarios. Add sophistication (enhancer rules, chained exceptions, AI matching) based on real user feedback.

---

## 9. Architecture Summary

### Self-Hosted (v0.1)

```
                  +------------------+
                  |   Observe Binary |
                  |                  |
 Browser SDK ---->|  HTTP API        |
                  |  Ring Buffer     |
                  |  Query Engine    |
                  |  Embedded SPA    |
                  +--------+---------+
                           |
                    +------+------+
                    |  ClickHouse  |
                    | (or Nucleus) |
                    +-------------+
```

Single binary. No Kafka, no Redis, no PostgreSQL (unless metadata needs it). ClickHouse is the only required dependency.

### Self-Hosted (v0.3 -- Full)

```
                  +------------------+
 Browser SDK ---->|  Observe Binary  |
                  |  Analytics API   |
 Error SDK ------>|  Error API       |
                  |  Query Engine    |
 OTel SDK ------->|  OTLP Receiver   |
                  |  Embedded SPA    |
                  +--------+---------+
                           |
                    +------+------+
                    |  ClickHouse  |
                    | (or Nucleus) |
                    +-------------+
```

Still a single binary. OTLP receiver is embedded. All three signal types (analytics, errors, traces) share the ingestion buffer and ClickHouse connection pool.

### Managed Multi-Tenant (Future -- Teploy Platform)

```
 SDKs ----> Load Balancer ----> Observe Ingestion (N replicas)
                                     |
                                  Kafka
                                     |
                              Observe Workers (N)
                                     |
                              ClickHouse Cluster
                                     |
                              Observe Query (N) <---- Dashboard
```

At multi-tenant scale, add Kafka for durability and horizontal ingestion scaling. The single binary splits into ingestion + query services. ClickHouse gets sharded with tenant-first ordering.

---

## 10. What NOT to Do (Anti-Patterns from Research)

1. **Don't write every query twice for two databases** (Umami). Pick one analytics database.
2. **Don't denormalize person/group properties onto every event row** (PostHog). JOIN at query time.
3. **Don't use EAV for custom properties** (Umami). Use JSON or Map columns.
4. **Don't build 3+ versions of the same table** (PostHog sessions, SignOz query builders). Get the schema right early.
5. **Don't require ZooKeeper** for single-node deployments (SignOz).
6. **Don't build a custom query language too early** (PostHog HogQL). Use raw SQL or a thin builder.
7. **Don't put schema migrations in a separate repo** (SignOz). Keep them with the query code.
8. **Don't skip write buffering** (Umami). Every event as a synchronous INSERT will fail at scale.
9. **Don't use 5+ databases** (Sentry: PG + ClickHouse + Redis + Kafka + NodeStore). Minimize dependencies.
10. **Don't try to ship 50+ features** (PostHog). Ship 5 features that work perfectly.

---

## 11. Individual Research Documents

Full detailed analysis for each tool:

- [UMAMI_RESEARCH.md](./UMAMI_RESEARCH.md) -- 900+ lines covering schema, ingestion, query patterns, full API
- [POSTHOG_RESEARCH.md](./POSTHOG_RESEARCH.md) -- 950+ lines covering ClickHouse schemas, Kafka pipeline, HogQL, migration story
- [SENTRY_RESEARCH.md](./SENTRY_RESEARCH.md) -- 1020+ lines covering error grouping algorithm, Snuba, ingestion pipeline
- [SIGNOZ_RESEARCH.md](./SIGNOZ_RESEARCH.md) -- 1150+ lines covering OTLP pipeline, ClickHouse schemas, Go architecture
