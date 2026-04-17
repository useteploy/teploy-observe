# Teploy Observe

All-in-one observability platform. Analytics, error tracking, APM, logs, session replay, monitoring, feature flags, experiments, and more -- in a single binary.

Two processes. 100MB idle. Runs anywhere.

## Quick Start

### Docker Compose (recommended)

```bash
git clone https://github.com/useteploy/observe.git
cd observe
docker-compose up
```

Open `http://localhost:3000`. Default login: `admin` / `observe`.

### Manual Setup

Download the Observe binary and a Nucleus binary, then:

```bash
# Start Nucleus
./nucleus start --host 0.0.0.0 --no-tls --max-memory 512

# Start Observe
export OBSERVE_NUCLEUS_URL="postgres://localhost:5432/observe"
export OBSERVE_JWT_SECRET="your-secret-here"
export OBSERVE_ADMIN_USER="admin"
export OBSERVE_ADMIN_PASSWORD="your-password"
./observe
```

Open `http://localhost:3000`.

## Features

### Analytics
- Pageviews, visitors, sessions, bounce rate, duration
- Top pages, referrers, UTM tracking, channel classification
- Browser, OS, device, country, language breakdowns
- Custom events with property drill-down
- Funnels, retention cohorts, user journeys
- Goals and conversion tracking
- Real-time active visitors
- Cookie-free, GDPR-compliant

### Error Tracking
- Automatic error grouping (MD5 of type + in-app frames)
- Stack trace viewer with source map support
- Full-text search across error messages (BM25)
- Issue management (open / resolved / ignored)
- Release health tracking
- Breadcrumb timeline
- Error-to-session cross-correlation

### APM / Distributed Tracing
- OTLP HTTP/JSON trace ingestion
- Service list with RED metrics (rate, errors, duration)
- Trace waterfall and flamegraph views
- Service dependency map
- Latency percentiles (p50/p95/p99)

### Logs
- Log ingestion with level, service, trace correlation
- Full-text search
- Log pipelines (JSON parse, regex extract, field rename, masking, sampling)

### Session Replay
- DOM snapshot + mouse/click/scroll/mutation recording
- Playback with timeline scrubbing
- Error correlation

### Monitoring
- Uptime HTTP monitors with response time tracking
- Cron heartbeat monitors with missed check detection
- Background checker with configurable intervals

### LLM / AI Observability
- Track model calls (prompt/completion tokens, cost, latency)
- Auto cost estimation for GPT-4, Claude, etc.
- Model breakdown and usage stats

### Infrastructure
- Host metrics (CPU, memory, disk, network, load)
- Agent report endpoint

### Product Tools
- Feature flags (boolean + multivariate, rollout %, user targeting)
- A/B experiments (statistical significance, chi-squared testing)
- Surveys (text, rating, NPS, multiple choice)
- Custom dashboards with metric panels

### Platform
- Multi-user with roles (admin, editor, viewer)
- Alerting (threshold-based on any metric, cooldown)
- Integrations (Jira, GitHub, PagerDuty, Slack, email)
- Webhooks on alert trigger
- SSO / SAML
- Email report digests (daily/weekly)
- Data export (CSV/JSON)
- SQL query explorer
- Tracked links and pixel tracking
- User feedback widget
- Saved views

## JS SDKs

```html
<!-- Analytics (pageviews, custom events, web vitals) -->
<script defer src="https://your-observe.com/t/observe.js"
  data-site-id="YOUR_SITE_ID"></script>

<!-- Error tracking -->
<script defer src="https://your-observe.com/t/observe-errors.js"
  data-site-id="YOUR_SITE_ID"></script>

<!-- Session replay -->
<script defer src="https://your-observe.com/t/observe-replay.js"
  data-site-id="YOUR_SITE_ID"></script>

<!-- User feedback widget -->
<script defer src="https://your-observe.com/t/observe-feedback.js"
  data-site-id="YOUR_SITE_ID"></script>
```

### Analytics API

```javascript
// Track custom event
observe.track("signup", { plan: "pro" });

// Track revenue
observe.revenue(49.99, "USD", { product: "annual" });

// Track web vitals
observe.trackVitals();
```

### Error API

```javascript
// Manual capture
observeErrors.captureException(error);
observeErrors.captureMessage("Something went wrong");
observeErrors.addBreadcrumb({ type: "user", category: "click", message: "Button" });
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `OBSERVE_ADDR` | `:3000` | Listen address |
| `OBSERVE_NUCLEUS_URL` | `postgres://localhost:5432/observe` | Nucleus connection |
| `OBSERVE_JWT_SECRET` | (random) | JWT signing secret |
| `OBSERVE_ADMIN_USER` | `admin` | Default admin username |
| `OBSERVE_ADMIN_PASSWORD` | `observe` | Default admin password |
| `OBSERVE_SESSION_SALT` | `observe-default-salt` | Session hashing salt |
| `OBSERVE_RATE_LIMIT` | `1000` | Requests per second per IP |
| `OBSERVE_BUFFER_SIZE` | `100000` | Max buffered events |
| `OBSERVE_FLUSH_SIZE` | `500` | Flush at this many events |
| `OBSERVE_FLUSH_INTERVAL_MS` | `2000` | Flush interval (ms) |
| `OBSERVE_RAW_RETENTION_DAYS` | `30` | Raw event retention |
| `OBSERVE_HOURLY_RETENTION_DAYS` | `365` | Hourly rollup retention |
| `OBSERVE_SMTP_HOST` | | SMTP server for email reports |
| `OBSERVE_SMTP_PORT` | `587` | SMTP port |
| `OBSERVE_SMTP_USER` | | SMTP username |
| `OBSERVE_SMTP_PASS` | | SMTP password |
| `OBSERVE_SMTP_FROM` | | From email address |

## API Endpoints

### Ingestion (API Key auth via `X-API-Key` header)

| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/v1/events` | Ingest analytics event |
| POST | `/api/v1/events/batch` | Ingest batch of events |
| POST | `/api/v1/errors` | Ingest error event |
| POST | `/api/v1/logs` | Ingest log entry |
| POST | `/v1/traces` | OTLP trace ingestion |
| POST | `/api/v1/llm/ingest` | Ingest LLM trace |
| POST | `/api/v1/infra/report` | Report host metrics |
| POST | `/api/v1/replays` | Ingest session replay |
| POST | `/api/v1/feedback` | Submit user feedback |

### Dashboard Queries (JWT auth via `Authorization: Bearer` header)

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/stats/overview` | Overview metrics |
| GET | `/api/v1/stats/timeseries` | Time series chart data |
| GET | `/api/v1/stats/pages` | Top pages |
| GET | `/api/v1/stats/referrers` | Top referrers |
| GET | `/api/v1/stats/journeys` | User journey paths |
| GET | `/api/v1/stats/correlations` | Property correlations |
| GET | `/api/v1/stats/retention` | Retention cohorts |
| POST | `/api/v1/stats/funnel` | Funnel analysis |
| GET | `/api/v1/issues` | Error issue list |
| GET | `/api/v1/traces/services` | Service list with RED metrics |
| GET | `/api/v1/traces/search` | Search traces |
| GET | `/api/v1/logs/search` | Search logs |
| GET | `/api/v1/llm/stats` | LLM usage stats |
| GET | `/api/v1/infra/hosts` | Monitored hosts |
| POST | `/api/v1/query` | SQL query explorer |

## Architecture

```
Browser/SDKs --> Observe (Go, 23MB) --> Nucleus (Rust, 32MB)
                    |                       |
                    |-- Analytics buffer     |-- MergeTree (events)
                    |-- Error buffer         |-- KV (cache, HLL)
                    |-- Background jobs      |-- FTS (search)
                    |-- JWT/API key auth     |-- ReplacingMergeTree (rollups)
                    |-- Rate limiter         |
                    |-- Embedded UI          |
```

Two processes. No Redis. No Kafka. No ClickHouse. No ZooKeeper.

## License

MIT
