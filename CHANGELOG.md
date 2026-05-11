# Changelog

All notable changes to Observe are recorded here.

## v0.1.0-rc1 — 2026-05-10

First public release candidate.

### Product surface

- **Analytics** — pageviews, sessions, visitors, bounce, duration, top
  pages/referrers/UTM, channel classification, browser/OS/device/country
  breakdowns, custom events with property drill-down, funnels, retention
  cohorts, user journeys, goals, real-time active visitors. Cookie-free.
- **Error tracking** — automatic grouping (MD5 of type + in-app frames),
  stack viewer + source maps, BM25 full-text search, status workflow
  (open / resolved / ignored), release health, breadcrumbs,
  error-to-session correlation.
- **APM / distributed tracing** — OTLP HTTP/JSON ingest, service list +
  RED metrics, waterfall + flame graphs, dependency map, p50/p95/p99
  latency, performance-issue detectors (N+1, slow DB, consecutive DB,
  slow HTTP).
- **Logs** — OTLP HTTP ingest, structured search, level/service/host
  filters, log → trace correlation.
- **Session replay** — rrweb capture, privacy masking, replay viewer
  with network/console/breadcrumb tracks.
- **Heatmaps** — click + scroll + dead-click tracking with overlay UI.
- **Monitoring** — uptime checks (HTTP/TCP), alert rules
  (event count, error rate, p95 latency, custom SQL), Slack/email/webhook
  routing, incident markers overlaid on every time-series chart.
- **Feature flags + experiments** — boolean / multivariate, sticky
  bucketing, multi-touch attribution (first / last / linear).
- **Surveys + LLM tracing + custom dashboards + insights builder.**
- **Persons + cohorts** — aggregate events by `distinct_id`, build
  rule-based cohorts, filter analytics by cohort.
- **Multi-site / multi-tenant** — site switcher with per-site rate
  limits, RBAC, and `session_salt` HMAC for distinct-id privacy.

### Differentiators (no competitor ships these together)

- **AI query assistant** on the SQL explorer. English in, SQL out (your
  LLM key, your cost). Every call logged back to the LLM-tracing table.
- **Incident-mode markers** overlay translucent vertical bands on every
  time-series chart for the alert window.
- **Scheduled SQL exports** to any S3-compatible bucket (S3, R2, MinIO).

### Hardening (Phase 1)

- Disk-backed ingest WAL (`internal/ingest/queue.go`) — events survive
  `SIGKILL` and replay on restart.
- Per-site rate limiting with per-site caps (`sites.ratelimit_per_second`).
- Role-based access control: JWT carries a `role` claim; writes require
  `admin` or `editor`.  Viewer is reads-only.
- Tokenising lexer replaces the read-only-guard regex on the SQL
  explorer; comment and stacked-statement bypasses are rejected.
- `?token=` query-string auth accepted only on `GET` and only on the
  streaming / download route allowlist.
- Ingest batch writes use parameterized multi-row `INSERT` via Nucleus'
  SimpleProtocol; `escapeSQL` and the string-concatenated path removed.

### SDKs (MIT, separately versioned)

- `sdk/go`, `sdk/browser` (npm), `sdk/sentry-shim` (npm, drop-in
  `@sentry/node` replacement), `sdk/python` (PyPI). Counter / Gauge /
  Histogram + OTLP traces + slog handler in the Go SDK.

### Distribution

- Homebrew tap (`useteploy/tap/observe`) and Scoop bucket
  (`useteploy/scoop-bucket`) built by goreleaser on every `v*` tag.
- One-line installer at `scripts/install.sh` (POSIX shell — Alpine/
  Debian/macOS).
- Container image at `ghcr.io/useteploy/teploy-observe`.

### Known limitations

- Cross-connection `UPDATE` visibility lag of ~1s due to Nucleus's
  eventual-consistency model (Nucleus #2).
- `CAST(<aggregate> AS TEXT)` and `GROUP BY ... HAVING COUNT(*) >= N`
  have Observe-side workarounds pending Nucleus #24 / #28 fixes.
- Public demo instance not yet hosted; self-host via Docker compose
  or `scripts/install.sh`.
