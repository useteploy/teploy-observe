# Teploy Observe — Master Plan

**Goal:** Build the best self-hostable observability tool — beating PostHog, Umami, Sentry,
and SignOz on simplicity, data quality, performance, and cross-signal correlation.

**Single binary + single database.** That's the pitch. While competitors require 5-10 services
(ClickHouse + PostgreSQL + Redis + Kafka + ZooKeeper + workers), Observe is one Go binary
talking to one Nucleus instance. Two processes. That's it.

---

## Part 1: Where Competitors Are Weak (Our Opportunities)

### Deployment complexity (ALL of them)
- PostHog: 10+ Docker services, 4 programming languages
- Sentry: 10+ Docker services, Snuba + Relay + Symbolicator
- SignOz: requires ZooKeeper even for single-node
- Umami: simple, but caps out on performance

**Our advantage:** Two processes. `docker-compose up` and you're running.

### Data silos (PostHog, Sentry, SignOz)
- Analytics in ClickHouse, metadata in PostgreSQL, cache in Redis, queue in Kafka
- Cross-signal correlation requires application-level joins across databases
- "Show me the user session that led to this error" is a multi-system query

**Our advantage:** One database. Cross-model queries in a single transaction.

### Write buffering (Umami, SignOz)
- Umami: synchronous INSERT per pageview, saturates at ~100 req/s
- SignOz: no message queue, data loss if ClickHouse is down

**Our advantage:** In-memory ring buffer with batch INSERT and backpressure.

### Schema sprawl (PostHog, SignOz)
- PostHog: 3 session table versions coexisting
- SignOz: schemas in a separate repo from query code, 5 ClickHouse databases

**Our advantage:** One schema, one repo, one migration path.

### Feature bloat (PostHog)
- Games, product tours, visual review, MCP store, conversations
- 252+ feature flags in Sentry's codebase

**Our advantage:** Focused features that work perfectly.

---

## Part 2: What Users Actually Pay For (Must-Match Features)

| Product | Conversion driver | Our must-match |
|---------|-------------------|----------------|
| Umami | Privacy-compliant GA replacement without cookies | Cookie-free sessions (DONE) |
| PostHog | Session replay + product analytics correlation | Session browser + activity timeline + cross-signal correlation |
| Sentry | Error grouping + volume (free tier caps events) | Solid error grouping + unlimited self-hosted |
| SignOz | OTLP-native tracing + ClickHouse performance | OTLP ingestion + fast queries on Nucleus MergeTree |

---

## Part 3: Nucleus Model Usage (Earn Your Place)

Only use a specialized Nucleus model when SQL/MergeTree can't do the job or
would be measurably worse. Every choice below is justified.

### Models we USE:

| Model | Use case | Why not just SQL? |
|-------|----------|-------------------|
| **MergeTree** | Events, spans, error events | Columnar compression, zone map pruning, sorted storage. This IS the analytics engine. |
| **AggregatingMergeTree** | Session summaries | Incremental merge of pageview counts. SQL table would require full recomputation. |
| **ReplacingMergeTree** | Hourly/daily rollups | Idempotent re-computation via version column. |
| **JSONB columns** | Event properties, span attributes, error stack traces | Flexible schema on MergeTree tables. `->`, `->>`, `@>` operators in SQL. |
| **KV with TTL** | Grouphash→issue cache, rate limiting, session presence | Sub-ms lookups on hot path (every ingest). SQL round-trip per event is too slow. |
| **HyperLogLog** (KV) | Approximate unique visitor counts for rollups | O(1) merge across time windows. COUNT DISTINCT on raw events won't scale. |
| **FTS** | Error message search | BM25 ranked search across error messages. SQL LIKE can't rank by relevance. Genuine differentiator — no competitor has this. |

### Models we DO NOT USE (yet):

| Model | Why not |
|-------|---------|
| **TimeSeries** | Wrong shape for observability. Single float64 value per point. RED metrics are multi-column (rate, errors, duration per endpoint per bucket). AggregatingMergeTree handles this better. Re-evaluate if we add prometheus-style metrics ingestion. |
| **Graph** | Service dependency maps are simple pair tables (src_service, dst_service, call_count, latency). SQL handles this fine. Graph traversal would only matter if we needed multi-hop path queries ("what's the critical path from gateway to database?"). Add when users actually ask for it. |
| **Vector** | Similar-trace search is a cool idea but not an MVP feature. No user has asked for "find traces that look like this one." Add when trace volume is high enough that manual search fails. |
| **Document** | JSONB columns on MergeTree tables cover all structured data needs. A separate document store would split data across two access patterns for no benefit. |
| **Geo** | We already have a custom embedded IP→country binary lookup. Nucleus R-tree would be needed for polygon queries ("show me all events from this metro area"). Not needed for country-level analytics. |
| **PubSub** | Current 2-second SSE polling on events_recent works fine for real-time dashboards. PubSub would save ~2s latency but adds complexity. Revisit if users need sub-second real-time. |
| **Blob** | No large binary objects in observability. Source maps would be the use case — defer until v0.2 source map support. |

---

## Part 4: Feature Completeness Checklist

### Analytics (v0.1 — MOSTLY DONE)

| Feature | Status | Priority |
|---------|--------|----------|
| Pageviews, sessions, unique visitors | DONE | -- |
| Time-series charts | DONE | -- |
| Top pages, referrers, browsers, OS, devices, countries, languages | DONE | -- |
| UTM tracking | DONE | -- |
| Custom events with properties | DONE (counts only) | -- |
| Real-time active visitors | DONE | -- |
| Cookie-free sessions | DONE | -- |
| Channel classification | DONE | -- |
| Date range picker + comparison | DONE | -- |
| Interactive filtering | DONE | -- |
| Entry/exit pages | DONE | -- |
| Live event stream (SSE) | DONE | -- |
| Session browser + activity timeline | NOT DONE | P0 |
| Custom event property drill-down | NOT DONE | P1 |
| Funnels | NOT DONE | P1 |
| Retention cohort analysis | NOT DONE | P1 |
| Bounce rate in rollups (currently hardcoded 0) | NOT DONE | P0 |
| Visit duration in rollups (currently hardcoded 0) | NOT DONE | P0 |
| HyperLogLog for visitor counts in rollups | NOT DONE | P1 |
| Data export (CSV/JSON) | NOT DONE | P2 |

### Error Tracking (v0.2 — NOT STARTED)

| Feature | Priority | Nucleus model |
|---------|----------|---------------|
| Error event ingestion (POST /api/v1/errors) | P0 | MergeTree |
| Stack trace capture + storage | P0 | JSONB column on MergeTree |
| Error grouping (MD5 of type + in-app frames, parameterized message fallback) | P0 | SQL + KV cache |
| Issue list (grouped errors with count, status, first/last seen) | P0 | ReplacingMergeTree |
| Issue detail (latest event, stack trace viewer, breadcrumb timeline) | P0 | SQL + JSONB |
| Issue status management (open/resolve/ignore) | P0 | SQL UPDATE |
| Error rate time-series on overview dashboard | P0 | MergeTree aggregation |
| Error JS SDK (window.onerror, unhandledrejection, breadcrumbs) | P0 | -- (client-side) |
| Full-text search across error messages | P1 | FTS (BM25) |
| Release tracking (which release introduced the error) | P1 | SQL column |
| Custom fingerprinting (user-set fingerprint override) | P1 | SDK + SQL |
| Source map support | P2 | SQL/Blob (later) |
| Error→session correlation ("show me the user session leading to this error") | P1 | Cross-table SQL JOIN (same DB!) |

### APM / Tracing (v0.3 — NOT STARTED)

| Feature | Priority | Nucleus model |
|---------|----------|---------------|
| OTLP HTTP/JSON trace ingestion (POST /v1/traces) | P0 | MergeTree |
| Span storage (OTLP-shaped schema) | P0 | MergeTree + JSONB attributes |
| Trace waterfall view (reconstruct tree from spans) | P0 | SQL query + frontend |
| Service list with RED metrics (rate, errors, duration) | P0 | AggregatingMergeTree |
| Latency percentiles (p50/p95/p99) | P0 | SQL percentile functions or pre-computed |
| Error rates per service/endpoint | P0 | SQL aggregation |
| Service dependency map | P1 | SQL pair table (src, dst, metrics) |
| Trace search with filters (service, operation, duration, status) | P1 | MergeTree scan with zone map pruning |
| Trace→error correlation ("errors that happened during this trace") | P1 | Cross-table SQL JOIN |
| Trace→session correlation ("the user session that triggered this request") | P2 | Cross-table SQL JOIN |
| OTLP gRPC ingestion | P2 | Same pipeline, different transport |
| Metrics ingestion (OTLP metrics) | P3 | Re-evaluate TimeSeries model here |

### Infrastructure / Platform

| Feature | Status | Priority |
|---------|--------|----------|
| JWT auth for dashboard | DONE | -- |
| API key auth for ingestion | DONE | -- |
| Site CRUD API | DONE | -- |
| Share links (public dashboards) | DONE | -- |
| Dockerfile | DONE | -- |
| docker-compose.yml | DONE | -- |
| .env.example | DONE | -- |
| Password hashing (upgrade SHA-256 to bcrypt) | NOT DONE | P0 |
| Rate limiting on ingestion endpoint | NOT DONE | P1 |
| Input validation (payload size, property count limits) | NOT DONE | P1 |
| Multi-user support (invite users, roles) | NOT DONE | P2 |
| Alerting (threshold-based on any metric) | NOT DONE | P2 |
| Webhook integrations (Slack, email on alert) | NOT DONE | P3 |

---

## Part 5: Data Quality Standards

### What we collect and how we ensure accuracy

**Analytics events:**
- Timestamp: server-side UTC (don't trust client clocks)
- Session ID: deterministic SHA-256(site + IP + UA + monthly salt) — cookie-free
- Visit ID: rotates on 30-min inactivity + hourly boundary
- GeoIP: server-side lookup from embedded binary (not client-reported)
- UA parsing: server-side (not client-reported navigator.userAgent string parsing)
- Bot filtering: server-side isBot check, silent drop (return 200)
- Referrer cleaning: strip query params, drop self-referrals
- UTM extraction: server-side from URL (not client-reported)
- Properties: JSONB, user-controlled, no size limit enforcement yet (add P1)

**Error events:**
- Stack traces: capture raw frames, normalize at grouping time (basename filenames, strip codegen)
- Message parameterization: replace UUIDs, IPs, numbers, URLs, emails with placeholders before hashing
- Breadcrumbs: chronological trail (console, fetch, clicks, navigation) with timestamps
- Contexts: browser, OS, device, runtime — server-enriched from UA where possible
- Fingerprint: MD5 of contributing values, cached in KV for O(1) lookup

**Trace spans:**
- OTLP-shaped from day one (trace_id, span_id, parent_span_id, attributes)
- Attributes stored as JSONB (flexible schema, queryable via `->>`)
- RED metrics derived at ingest time (not query time) for dashboard performance
- Service name extracted from resource attributes and materialized as a column

### What we DON'T collect (privacy)
- Raw IP addresses (never stored, used only transiently for session hash + geo)
- No cookies, no local storage, no fingerprinting beyond IP+UA
- Session salt rotates monthly — visitors can't be tracked across months
- DNT respected by default (configurable)
- GDPR-compliant without cookie consent banners

---

## Part 6: Performance Targets

| Metric | Target | How |
|--------|--------|-----|
| Ingestion throughput | 5,000 events/sec sustained, 10k burst | In-memory ring buffer, batch INSERT (500 events per flush) |
| Ingestion latency (client) | < 50ms p99 | Accept into buffer immediately, flush async |
| Dashboard query (recent, < 24h) | < 200ms | Query events_recent table (slim, 7-day TTL) |
| Dashboard query (hourly, 1-7d) | < 500ms | Query stats_hourly (ReplacingMergeTree, pre-aggregated) |
| Dashboard query (daily, > 7d) | < 1s | Query stats_daily (ReplacingMergeTree, pre-aggregated) |
| Real-time update | < 3s | SSE polling every 2s on events_recent |
| Error grouping | < 5ms per event | KV cache for fingerprint→issue lookup |
| Trace waterfall render | < 500ms | MergeTree scan by trace_id with zone map pruning |
| Binary size | < 30MB | Go binary with embedded UI assets |
| Memory (idle) | < 50MB | Ingestion buffer is the main consumer |
| Memory (under load) | < 500MB | Buffer cap at 100k events, flush at 500 |

---

## Part 7: The Killer Differentiators

These are features Observe can offer that competitors CANNOT build without
adding new database systems to their stack:

### 1. Cross-signal correlation (structurally impossible for competitors)

"Show me the user's analytics session that led to this error."
"Which traces contain errors from issue #42?"
"Correlate this traffic spike with new errors that appeared at the same time."

All three signals (analytics, errors, traces) in one database = one JOIN.
Competitors need application-level orchestration across ClickHouse + PostgreSQL + Redis.

### 2. Full-text error search (no competitor has this)

BM25-ranked search across all error messages with fuzzy matching.
"Search your errors like you search Google."
Requires FTS engine — ClickHouse can only do LIKE and bloom filters.

### 3. Two-process deployment (massive ops advantage)

Every competitor: 5-10 services minimum.
Observe: Go binary + Nucleus binary. `docker-compose up`.

### 4. Unlimited self-hosted (beats Sentry's pricing model)

Sentry's conversion trigger is hitting the free-tier event cap during an incident.
Observe is free self-hosted with no caps. The managed version (Teploy Platform)
monetizes convenience and scale, not artificial limits.

---

## Part 8: Build Order (Optimized for Impact)

### Phase 1: Polish v0.1 analytics (1-2 sessions)
- Fix bounce/duration in rollups
- Session browser + activity timeline
- Custom event property drill-down
- bcrypt password hashing
- Input validation + rate limiting
- HyperLogLog for rollup visitor counts

### Phase 2: Error tracking (2-3 sessions)
- Schema + migration
- Grouping algorithm (MD5 of type + frames, parameterized message fallback)
- Error ingestion endpoint + JS SDK
- Issue list + detail + status management
- FTS index on error messages (differentiator)
- KV cache for grouphash lookups
- Error→session cross-correlation
- Dashboard error tab

### Phase 3: APM / tracing (2-3 sessions)
- Schema + migration
- OTLP HTTP/JSON ingestion
- Span storage in MergeTree
- RED metrics in AggregatingMergeTree
- Service list + operations
- Trace waterfall view
- Service dependency map (SQL pair table)
- Trace→error→session cross-correlation
- Dashboard APM tab

### Phase 4: Advanced analytics (1-2 sessions)
- Funnels (multi-step conversion)
- Retention cohort analysis
- Data export (CSV/JSON)

### Phase 5: Platform features (1-2 sessions)
- Multi-user support + roles
- Alerting (threshold-based)
- Webhook integrations
- OTLP gRPC support

---

## Part 9: Upstream-First Development

Every phase will stress Nucleus in new ways:

| Phase | Nucleus stress points |
|-------|----------------------|
| Analytics polish | AggregatingMergeTree accuracy, HyperLogLog correctness, complex GROUP BY queries |
| Error tracking | JSONB containment queries (`@>`), KV under write load, FTS indexing throughput, ReplacingMergeTree upsert correctness |
| APM | High-volume MergeTree writes (spans >> events), JSONB attribute queries, cross-table JOINs |
| Funnels | Window functions, correlated subqueries, complex multi-step SQL |

Fix upstream first. Every bug fixed in Nucleus benefits all future Nucleus users.
Same for Neutron Go SDK and Neutron TypeScript.
