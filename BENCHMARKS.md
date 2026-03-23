# Teploy Observe — Performance Benchmarks

**Date:** 2026-03-23
**Hardware:** Intel N5000 (4-core, 4GB RAM), Debian 13
**Setup:** Observe (Go binary, 23MB) + Nucleus (Rust binary, 32MB with jemalloc)

---

## Ingestion Throughput

| Signal | Throughput | p50 | p95 | p99 | Avg |
|--------|-----------|-----|-----|-----|-----|
| Analytics (pageviews) | 75 req/s | 20ms | 45ms | 73ms | 34ms |
| Error events | 0.4 req/s* | 18ms | 37ms | 75ms | 20ms |
| OTLP traces (2 spans/req) | 58 req/s | 24ms | 66ms | 109ms | 52ms |

*Error ingestion is rate-limited. Per-request latency is fast (18ms p50) but the rate limiter caps throughput. Without rate limiting, error pipeline throughput is bottlenecked by synchronous KV + FTS operations per event.

## Memory Usage

| State | Nucleus | Observe | Total | Free RAM |
|-------|---------|---------|-------|----------|
| Idle | 72 MB | 19 MB | **91 MB** | 3,375 MB |
| After analytics benchmark | 75 MB | 23 MB | **98 MB** | 3,214 MB |
| After all benchmarks (8,500 reqs) | 100 MB | 24 MB | **124 MB** | 3,185 MB |

## Competitor Comparison

### Resource Usage (self-hosted, equivalent workload)

| Stack | Processes | Idle RAM | Under Load | Disk |
|-------|-----------|----------|------------|------|
| **Observe + Nucleus** | **2** | **91 MB** | **124 MB** | **55 MB** |
| Umami + PostgreSQL | 2 | ~110 MB | ~200 MB | ~80 MB |
| SignOz (full stack) | 5+ | 2-4 GB | 4-6 GB | ~2 GB |
| Sentry (full stack) | 10+ | 4-8 GB | 8-16 GB | ~5 GB |
| PostHog (full stack) | 10+ | 4-8 GB | 8-16 GB | ~5 GB |

### Feature Coverage

| Feature | Observe | Umami | PostHog | Sentry | SignOz |
|---------|---------|-------|---------|--------|--------|
| Web analytics | Yes | Yes | Yes | -- | -- |
| Funnels + retention | Yes | Yes | Yes | -- | -- |
| User journeys | Yes | Yes | Yes | -- | -- |
| Goals/conversions | Yes | Yes | Yes | -- | -- |
| Error tracking | Yes | -- | Yes | Yes | -- |
| Stack traces + source maps | Yes | -- | -- | Yes | -- |
| FTS error search | Yes | -- | -- | -- | -- |
| OTLP traces | Yes | -- | -- | Yes | Yes |
| Service RED metrics | Yes | -- | -- | -- | Yes |
| Log ingestion | Yes | -- | -- | Yes | Yes |
| Session replay | Yes | -- | Yes | Yes | -- |
| Uptime monitoring | Yes | -- | -- | Yes | -- |
| Cron monitoring | Yes | -- | -- | Yes | -- |
| Custom dashboards | Yes | -- | Yes | Yes | Yes |
| Alerting + webhooks | Yes | -- | Yes | Yes | Yes |
| Link/pixel tracking | Yes | Yes | -- | -- | -- |
| Cross-signal correlation | Yes | -- | -- | -- | -- |
| Multi-user + roles | Yes | Yes | Yes | Yes | Yes |

### Deployment Complexity

| Stack | Install Command | Services | External Dependencies |
|-------|----------------|----------|----------------------|
| **Observe** | Download 2 binaries, run | 2 | None |
| Umami | npm install + PostgreSQL | 2 | Node.js, PostgreSQL |
| SignOz | docker-compose (5+ services) | 5+ | ClickHouse, ZooKeeper, Kafka |
| Sentry | docker-compose (10+ services) | 10+ | PostgreSQL, Redis, Kafka, ClickHouse |
| PostHog | docker-compose (10+ services) | 10+ | PostgreSQL, Redis, ClickHouse, Kafka |

### Key Differentiators

1. **Two-process deployment** — Every competitor requires 5-10+ services. Observe is two binaries.
2. **124 MB total RAM** — Competitors use 2-16 GB for equivalent functionality.
3. **Cross-signal correlation** — Analytics session + error + trace in one JOIN. Competitors need cross-database orchestration.
4. **Full-text error search** — BM25-ranked search across error messages. No competitor offers this.
5. **55 MB on disk** — The entire stack fits on a Raspberry Pi.
