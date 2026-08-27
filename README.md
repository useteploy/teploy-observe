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

### Docker

```bash
git clone https://github.com/useteploy/teploy-observe.git
cd teploy-observe
docker compose up
```

The Compose file runs the published image. To test local source changes, build
the root `Dockerfile` explicitly.

Open `http://localhost:3000`. First visit lands on the setup wizard — there's
no default password to change later, since none is set until you choose one
there.

### Install script

Downloads the installer from the latest release (not the mutable `main`
branch) and verifies its SHA-256 against the release's `checksums.txt`
before executing it — the script itself already checksum+signature-verifies
the `observe` binary it installs, but nothing verified the script itself
until now:

```bash
(
  set -e
  curl -fsSLO https://github.com/useteploy/teploy-observe/releases/latest/download/install.sh
  curl -fsSLO https://github.com/useteploy/teploy-observe/releases/latest/download/checksums.txt
  grep " install.sh\$" checksums.txt > checksum.txt
  if command -v sha256sum >/dev/null 2>&1; then sha256sum -c checksum.txt || exit 1; else shasum -a 256 -c checksum.txt || exit 1; fi
  sh install.sh
)
```

The script generates a random admin password and prints it on completion;
it is also stored in `/etc/observe/observe.env` and rotatable from
**Settings → Users**.

The direct installer verifies the release's SHA256 through an Ed25519-signed
`checksums.txt` before installing anything. It fails closed if the signature
or archive hash is invalid. `OBSERVE_HEALTH_URL` is read by the install script
only, for the health poll it runs after restarting the existing service
(default `http://127.0.0.1:3000/healthz`). It does not configure
`observe upgrade`, which derives its readiness URL from `OBSERVE_ADDR`; pass
`--health-url` instead for a non-default endpoint.

## Upgrade

Use the manager that installed Observe:

```bash
# Direct Linux/systemd install
sudo observe upgrade
sudo observe upgrade --version v1.2.3
# --service <unit>     systemd unit name (default: observe.service)
# --health-url <url>   readiness URL for custom service configuration,
#                      derived from OBSERVE_ADDR by default

# Homebrew
brew upgrade useteploy/tap/observe

# Docker Compose
docker compose pull && docker compose up -d
# Pin a release with OBSERVE_VERSION=1.2.3
```

The direct updater authenticates and stages the release while the current
server remains online. It then asks systemd to stop Observe gracefully,
atomically replaces the binary, and requires three healthy responses from the
exact new version. A failed start or readiness check restores and restarts the
previous version automatically. There is a brief restart window; telemetry
senders should retain their normal retry policy.

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
- OTLP ingest over HTTP for all three signals — traces, metrics and logs — in
  both wire formats (`application/x-protobuf`, which is what OTLP exporters send
  by default, and `application/json`). gRPC is not served; point an exporter at
  HTTP transport or put a Collector in front.
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
- **Ingest is WAL-backed** — accepted events are mirrored to
  `$OBSERVE_QUEUE_DIR` when the queue is available. Graceful shutdown fsyncs
  the queue; crash recovery replays records since the last checkpoint.
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
| `OBSERVE_ADDR` | `:3000` | Listen address. Keep on localhost/tailnet when publishing ingest. |
| `OBSERVE_WEBHOOK_ALLOW_CIDRS` | (unset) | Networks webhook delivery may reach despite being private, as CIDRs (`100.64.0.0/10, 10.0.0.0/8`); a bare IP means that address alone. Self-hosted fleets live on a tailnet, which the SSRF guard blocks by design — without this an alert can never reach a self-hosted receiver. Applies to webhook delivery ONLY, never to integrations or uptime monitoring. Link-local (169.254.169.254 cloud metadata), multicast and the unspecified address stay blocked whatever you declare. Hostnames are refused: allowing by name would hand back DNS rebinding. |
| `OBSERVE_INGEST_ADDR` | (unset) | Optional second bind address serving ONLY telemetry-write endpoints (e.g. `:3001`). This is the port to expose publicly; the dashboard does not listen on it. |
| `OBSERVE_PUBLIC_URL` | (unset) | External base URL (`https://observe.example.com`) used for SSO metadata and generated links. Falls back to the request's Host header, which a client can spoof — set it whenever the instance is reachable by a name. |
| `OBSERVE_NUCLEUS_URL` | `postgres://localhost:5432/observe` | Nucleus connection. |
| `OBSERVE_JWT_SECRET` | (random) | JWT signing secret — set in prod to persist sessions across restarts. |
| `OBSERVE_SECRET_KEY` | (unset) | Master key for encrypting stored secrets (LLM API key, S3/R2 credentials) at rest; required to configure those features. |
| `OBSERVE_ADMIN_USER` | `admin` | Bootstrap admin username. |
| `OBSERVE_ADMIN_PASSWORD` | (unset) | Bootstrap admin password; unset means no default — the `/setup` wizard creates the account on first visit. |
| `OBSERVE_SESSION_SALT` | (random) | Session-ID hashing salt — set it to keep session/visitor IDs stable across restarts. |
| `OBSERVE_DEMO_MODE` | (unset) | Set to `true` to lock the deployment to a read-only public demo (write ops on `/api/v1/*` return 403). |
| `OBSERVE_SEED_DEMO` | (unset) | Set to `true` for first-boot demo seeding (off by default; also on when demo mode is set). |
| `OBSERVE_RATE_LIMIT` | `1000` | Default per-site events/sec. Per-site overrides via API. |
| `OBSERVE_TRUSTED_PROXIES` | (unset) | Comma-separated CIDRs/IPs whose `X-Forwarded-For` / `X-Real-Ip` are trusted for client-IP extraction. Empty trusts none (peer address) so clients can't spoof their IP to evade per-IP rate limiting. |
| `OBSERVE_BUFFER_SIZE` | `100000` | Max buffered events in memory. |
| `OBSERVE_FLUSH_SIZE` | `500` | Flush threshold (events). |
| `OBSERVE_FLUSH_INTERVAL_MS` | `2000` | Flush threshold (time). |
| `OBSERVE_DATA_DIR` | `./data` | Root dir for WAL, queue, local state. |
| `OBSERVE_QUEUE_DIR` | `$OBSERVE_DATA_DIR/queue` | Ingest WAL directory. |
| `OBSERVE_REQUIRE_WAL` | (unset) | Set to `true` (or `1`) to refuse to start when WAL-backed ingestion durability is unavailable, instead of degrading to memory-only. |
| `OBSERVE_RAW_RETENTION_DAYS` | `30` | Raw event retention. Also the window over which visitor counts are exact from raw events; past it they are counted from the `sessions` table (90 days), and past both the dashboard says which window the figure covers. |
| `OBSERVE_HOURLY_RETENTION_DAYS` | `365` | Hourly rollup retention. |
| `OBSERVE_LOG_ROUTES` | `0` | Set to `1` to print route table at boot. |
| `OBSERVE_SMTP_HOST` | | SMTP server for email reports. |
| `OBSERVE_SMTP_PORT` | `587` | SMTP port. |
| `OBSERVE_SMTP_USER` | | SMTP username. |
| `OBSERVE_SMTP_PASS` | | SMTP password. |
| `OBSERVE_SMTP_FROM` | | From email address. |
| `TEPLOY_NAV_DASH_URL` | | URL of your Teploy Dash dashboard. When set, it appears in the top-left cross-product switcher. |
| `TEPLOY_NAV_SHIP_URL` | | URL of your Teploy Ship dashboard. When set, it appears in the top-left cross-product switcher. |

### Single sign-on (OIDC)

Optional. When `OBSERVE_OIDC_ISSUER` and `OBSERVE_OIDC_CLIENT_ID` are set, the
login page offers an SSO button and Observe acts as an OpenID Connect relying
party (authorization-code flow with PKCE), minting its normal JWT after the IdP
authenticates the user. Password login stays available as the break-glass path.
Register `https://<your-observe-host>/api/v1/auth/oidc/callback` as the redirect
URI with your provider. When SSO is enabled, the first-run open-access grace
period is disabled (authentication becomes required).

| Variable | Default | Description |
|----------|---------|-------------|
| `OBSERVE_OIDC_ISSUER` | | IdP issuer URL (discovery base, e.g. `https://your-org.okta.com`). Required to enable SSO. |
| `OBSERVE_OIDC_CLIENT_ID` | | OAuth client ID. Required to enable SSO. |
| `OBSERVE_OIDC_CLIENT_SECRET` | | OAuth client secret. Omit for a public (PKCE-only) client. |
| `OBSERVE_OIDC_REDIRECT_URL` | (derived) | Callback URL. Derived from the request Host when unset; set explicitly behind a proxy that rewrites Host. Must end in `/api/v1/auth/oidc/callback`. |
| `OBSERVE_OIDC_SCOPES` | `openid profile email` | Space/comma-separated scopes (`openid` always included). Add `groups` for group-based role mapping. |
| `OBSERVE_OIDC_LABEL` | `Single sign-on` | Text on the SSO button. |
| `OBSERVE_OIDC_USERNAME_CLAIM` | `preferred_username` | Claim used as the username (falls back to `email`, then `sub`). |
| `OBSERVE_OIDC_ROLE_CLAIM` | `teploy_role` | Claim carrying the role directly (`admin`/`editor`/`viewer`). Checked first. |
| `OBSERVE_OIDC_GROUPS_CLAIM` | `groups` | Claim listing the user's groups, used when no direct role claim matches. |
| `OBSERVE_OIDC_ADMIN_GROUP` | | Group whose members become `admin`. |
| `OBSERVE_OIDC_EDITOR_GROUP` | | Group whose members become `editor`. |
| `OBSERVE_OIDC_VIEWER_GROUP` | | Group whose members become `viewer`. |
| `OBSERVE_OIDC_DEFAULT_ROLE` | `viewer` | Role for an authenticated user matching no role claim or group (least privilege). |

Role resolution order: a recognized `teploy_role` claim wins; otherwise groups
are matched (admin > editor > viewer); otherwise the default role. SSO users are
not stored in the `admin_users` table — their role comes fresh from the IdP on
every login.

#### Self-hosted identity providers

Any OIDC provider works. Two are worth calling out because if you already run
Teploy you probably already run one of them, so SSO costs you no new software.

**Forgejo** (or Gitea) is a full OIDC provider. Its discovery document
advertises `openid profile email groups` and a `groups` claim.

1. Register an OAuth2 application — Site Administration → Applications for an
   org-wide one, or user Settings → Applications for a personal one. Set the
   redirect URI to `https://<your-observe-host>/api/v1/auth/oidc/callback`.
2. Point Observe at it:

```bash
OBSERVE_OIDC_ISSUER=https://forgejo.example.com
OBSERVE_OIDC_CLIENT_ID=<client id>
OBSERVE_OIDC_CLIENT_SECRET=<client secret>
OBSERVE_OIDC_SCOPES="openid profile email groups"
OBSERVE_OIDC_ADMIN_GROUP=platform:owners
OBSERVE_OIDC_EDITOR_GROUP=platform:deployers
```

- Request `groups` explicitly. It is not in the default scopes, and without it
  no group matches, so every user lands on `OBSERVE_OIDC_DEFAULT_ROLE`.
- Forgejo emits one entry per org (`platform`) and one per team
  (`platform:deployers`). Group comparison is exact and case-sensitive, so copy
  the names as Forgejo spells them.
- Forgejo cannot mint a custom claim, so leave `ROLE_CLAIM` at its default and
  map roles by group.
- Each dashboard needs its own OAuth2 application because the redirect URIs
  differ, but all three can map against the same orgs and teams.

**OpenBao** also serves OIDC (`identity/oidc/provider`), which is convenient if
you already run it for `teploy secret --provider openbao`. Create a provider,
an assignment, and a client, then use the provider's discovery URL as the
issuer:

```bash
OBSERVE_OIDC_ISSUER=https://openbao.example.com/v1/identity/oidc/provider/teploy
```

Map roles with a scope template that emits a `groups` array (matched as above),
or one that emits a `teploy_role` string — OpenBao can produce a custom claim,
so the direct role claim is available here and takes precedence over groups.

## API

### Ingestion (API key auth via `X-API-Key` header)

| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/v1/events` | Ingest analytics event. |
| POST | `/api/v1/events/batch` | Ingest batch of events. |
| POST | `/api/v1/errors` | Ingest error event. |
| POST | `/api/v1/logs` | Ingest log entry. |
| POST | `/v1/traces` | OTLP trace ingestion (protobuf or JSON). |
| POST | `/v1/metrics` | OTLP metric ingestion (protobuf or JSON). |
| POST | `/v1/logs` | OTLP log ingestion (protobuf or JSON). |
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
