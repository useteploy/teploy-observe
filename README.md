# Teploy Observe

Self-hosted observability in one binary. Analytics, error tracking, APM,
logs, session replay, monitoring, feature flags, experiments — and three
things no competitor bundles:

1. **AI query assistant** on the SQL explorer. English in, SQL out (your
   LLM key, your cost). Every call logged back to the LLM-tracing table.
2. **Incident-mode markers.** When an alert fires, every time-series
   chart overlays a translucent vertical band for the window.
3. **Scheduled SQL exports** to any S3-compatible bucket (S3, R2, MinIO).

Two processes. ~100MB idle. Runs on a $5 VPS.

## Install

### Homebrew (macOS, Linux)

```bash
brew install useteploy/tap/observe
```

### Scoop (Windows)

```bash
scoop bucket add useteploy https://github.com/useteploy/scoop-bucket
scoop install observe
```

### Docker

```bash
git clone https://github.com/useteploy/teploy-observe.git
cd teploy-observe
docker compose up
```

Open `http://localhost:3000`. Default login: `admin` / `observe`.

### Install script

```bash
curl -sL https://raw.githubusercontent.com/useteploy/teploy-observe/main/scripts/install.sh | sh
```

### Build from source

```bash
git clone https://github.com/useteploy/teploy-observe.git
cd teploy-observe
go build ./cmd/observe        # neutron-go is vendored; no network setup
```

You also need a Nucleus database binary — see the Docker compose file for
the exact image and version.

## Features

### Analytics
- Pageviews, visitors, sessions, bounce rate, duration.
- Top pages, referrers, UTM tracking, channel classification.
- Browser, OS, device, country, language breakdowns.
- Custom events with property drill-down.
- Funnels, retention cohorts, user journeys, goals.
- Real-time active visitors.
- Cookie-free, GDPR-compliant.

### Error tracking
- Automatic grouping (MD5 of type + in-app frames).
- Stack trace viewer with source-map support.
- Full-text search across messages (BM25).
- Issue status (open / resolved / ignored), release health, breadcrumbs.
- Error-to-session cross-correlation.

### APM / distributed tracing
- OTLP HTTP/JSON ingest.
- Service list with RED metrics, waterfall + flame-graph views,
  dependency map, p50/p95/p99 latency.

### Logs
- Level, service, trace-id correlation, full-text search.
- Pipelines (JSON parse, regex extract, rename, mask, sample).

### Session replay
- DOM snapshot + mouse / click / scroll / mutation recording.
- Playback with timeline scrubbing and error correlation.

### Monitoring
- Uptime HTTP monitors with response-time tracking.
- Cron heartbeat monitors with missed-check detection.

### LLM observability
- Track model calls (tokens, cost, latency).
- Cost estimation for GPT / Claude / Gemini.
- The AI query assistant dogfoods this — every generated-SQL call writes
  a row.

### Product tools
- Feature flags (boolean + multivariate, rollout %, user targeting).
- A/B experiments (frequentist p-value + Bayesian probability-to-beat).
- Surveys, custom dashboards with panels.

### Platform
- **RBAC** enforced — JWT carries a role claim (`admin` / `editor` /
  `viewer`). Writes require editor or admin; destructive config routes
  require admin.
- **Ingest is WAL-backed** — every event is durably written to
  `$OBSERVE_QUEUE_DIR` before in-memory buffering. SIGKILL replays on
  restart.
- **Per-site rate limiting** — each site has its own token bucket. One
  noisy site can't starve a quiet one. Admin-editable via
  `PUT /api/v1/sites/{id}/ratelimit`.
- Alerting (threshold per metric, cooldown, silence). Alert-fire
  auto-opens an incident marker.
- Integrations (Jira, GitHub, PagerDuty, Slack, email) + webhooks.
- SSO / SAML, email digests, data export (CSV/JSON).
- SQL query explorer with lexer-guarded read-only enforcement
  (rejects `/* comment */ INSERT ...` and stacked statements).
- `POST /api/v1/query/explain` returns the Nucleus plan.

## Tracker install

```html
<!-- Analytics -->
<script defer src="https://your-observe.com/t/observe.js"
  data-site-id="YOUR_SITE_ID"></script>

<!-- Error tracking -->
<script defer src="https://your-observe.com/t/observe-errors.js"
  data-site-id="YOUR_SITE_ID"></script>

<!-- Session replay -->
<script defer src="https://your-observe.com/t/observe-replay.js"
  data-site-id="YOUR_SITE_ID"></script>

<!-- Feedback widget -->
<script defer src="https://your-observe.com/t/observe-feedback.js"
  data-site-id="YOUR_SITE_ID"></script>
```

### Analytics API

```javascript
observe.track("signup", { plan: "pro" });
observe.revenue(49.99, "USD", { product: "annual" });
observe.trackVitals();
```

### Error API

```javascript
observeErrors.captureException(error);
observeErrors.captureMessage("Something went wrong");
observeErrors.addBreadcrumb({ type: "user", category: "click", message: "Button" });
```

### Server-side SDKs

| Language | Package | Install |
|----------|---------|---------|
| Python | `teploy-observe` | `pip install teploy-observe` |
| Go | `sdk/go` | `go get github.com/useteploy/teploy-observe/sdk/go` |

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `OBSERVE_ADDR` | `:3000` | Listen address. |
| `OBSERVE_NUCLEUS_URL` | `postgres://localhost:5432/observe` | Nucleus connection. |
| `OBSERVE_JWT_SECRET` | (random) | JWT signing secret — set in prod to persist sessions across restarts. |
| `OBSERVE_ADMIN_USER` | `admin` | Bootstrap admin username. |
| `OBSERVE_ADMIN_PASSWORD` | `observe` | Bootstrap admin password. Change on first login. |
| `OBSERVE_SESSION_SALT` | `observe-default-salt` | Session-ID hashing salt. |
| `OBSERVE_RATE_LIMIT` | `1000` | Default per-site events/sec. Per-site overrides via API. |
| `OBSERVE_BUFFER_SIZE` | `100000` | Max buffered events in memory. |
| `OBSERVE_FLUSH_SIZE` | `500` | Flush threshold (events). |
| `OBSERVE_FLUSH_INTERVAL_MS` | `2000` | Flush threshold (time). |
| `OBSERVE_DATA_DIR` | `./data` | Root dir for WAL, queue, local state. |
| `OBSERVE_QUEUE_DIR` | `$OBSERVE_DATA_DIR/queue` | Ingest WAL directory. |
| `OBSERVE_RAW_RETENTION_DAYS` | `30` | Raw event retention. |
| `OBSERVE_HOURLY_RETENTION_DAYS` | `365` | Hourly rollup retention. |
| `OBSERVE_LOG_ROUTES` | `0` | Set to `1` to print route table at boot. |
| `OBSERVE_SMTP_HOST` | | SMTP server for email reports. |
| `OBSERVE_SMTP_PORT` | `587` | SMTP port. |
| `OBSERVE_SMTP_USER` | | SMTP username. |
| `OBSERVE_SMTP_PASS` | | SMTP password. |
| `OBSERVE_SMTP_FROM` | | From email address. |

## API

### Ingestion (API key auth via `X-API-Key` header)

| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/v1/events` | Ingest analytics event. |
| POST | `/api/v1/events/batch` | Ingest batch of events. |
| POST | `/api/v1/errors` | Ingest error event. |
| POST | `/api/v1/logs` | Ingest log entry. |
| POST | `/v1/traces` | OTLP trace ingestion. |
| POST | `/api/v1/llm/ingest` | Ingest LLM trace. |
| POST | `/api/v1/infra/report` | Host metrics. |
| POST | `/api/v1/replays` | Session replay events. |
| POST | `/api/v1/feedback` | User feedback. |

### Dashboard (JWT auth via `Authorization: Bearer`)

| Method | Path | Role | Description |
|--------|------|------|-------------|
| GET | `/api/v1/stats/*` | any | Overview, timeseries, pages, referrers, journeys, correlations, retention. |
| GET | `/api/v1/issues` | any | Error issue list. |
| GET | `/api/v1/traces/*` | any | Service RED metrics, search, waterfall. |
| GET | `/api/v1/logs/search` | any | Log search. |
| POST | `/api/v1/query` | editor+ | SQL explorer (read-only, lexer-guarded). |
| POST | `/api/v1/query/explain` | editor+ | Return the Nucleus plan. |
| POST | `/api/v1/ai/query` | editor+ | NL → SQL via configured LLM. |
| GET/PUT | `/api/v1/ai/config` | admin | Instance LLM provider / key. |
| GET | `/api/v1/incidents` | any | List / filter incidents. |
| POST | `/api/v1/incidents` | editor+ | Declare manual incident. |
| POST | `/api/v1/incidents/{id}/close` | editor+ | Close. |
| GET/POST/DELETE | `/api/v1/exports/scheduled` | admin | Scheduled SQL exports to S3. |
| POST | `/api/v1/sites` | admin | Create site. |
| DELETE | `/api/v1/sites/{id}` | admin | Delete site. |
| PUT | `/api/v1/sites/{id}/ratelimit` | admin | Set per-site events/sec cap. |
| POST | `/api/v1/platform/*` | admin | Users, alert rules, webhooks. |

## Architecture

```
Browser / SDKs
     |
     v
Observe (Go, ~26MB)  --- JWT / API key auth, RBAC middleware
     |                   per-site rate limiter
     |                   disk-backed ingest WAL
     |                   background jobs (rollups, retention,
     |                                    exports, alerts)
     |                   embedded SPA dashboard
     |                   AI query assistant (admin-supplied LLM key)
     |
     v  pgwire
Nucleus (Rust, ~32MB) --- SQL + multi-model (KV, columnar, FTS,
                          vector, doc, graph, time-series)
                          MergeTree, ReplacingMergeTree
                          WAL, TLS, query cache
```

Two processes. No Redis, no Kafka, no ClickHouse, no ZooKeeper.

## License

**Server**: AGPL-3.0-or-later. See `LICENSE`.

**SDKs** (`sdk/browser`, `sdk/sentry-shim`, `sdk/python`, `sdk/go`):
MIT. Each SDK subdirectory has its own `LICENSE`.

See `CONTRIBUTING.md` for contribution guidelines and `SECURITY.md` for
vulnerability reports.
