# Changelog

All notable changes to Observe are recorded here.

## [Unreleased]

### Added

- **OTLP protobuf ingest, and the logs signal.** `/v1/traces` and `/v1/metrics`
  previously accepted only `application/json` and answered protobuf with a 415,
  and there was no `/v1/logs` at all — so a stock OpenTelemetry SDK or Collector
  pointed at Observe exported nothing unless it was reconfigured with
  `OTEL_EXPORTER_OTLP_PROTOCOL=http/json`, and dropped logs entirely. All three
  signals now accept both `application/x-protobuf` (the exporters' default) and
  JSON. Protobuf is translated into the same request the JSON path builds, so
  ingest, validation and storage stay single-sourced. OTLP logs land in the same
  store as `/api/v1/logs`, so pipelines, search and the UI work on them with no
  extra wiring; severity maps to Observe's levels, and `service.name` plus the
  resource attributes are carried onto every record. Verified end to end against
  the real OpenTelemetry JS exporters. gRPC remains unsupported — use HTTP
  transport or a Collector.

- **Cross-product dashboard switcher.** A top-left dropdown (in the sidebar) lets you jump between the deployed Teploy dashboards — Dash, Observe, and Ship. Configure the sibling URLs with `TEPLOY_NAV_DASH_URL` and `TEPLOY_NAV_SHIP_URL` (same env convention across all three products); the switcher only appears once at least one sibling URL is set. Exposed via `/api/v1/config`.
- **Single sign-on (OIDC).** Observe can act as an OpenID Connect relying party: delegate login to your own identity provider (Okta, Azure AD/Entra, Google Workspace, Keycloak, Authentik — "generic OIDC") or to Teploy Platform acting as the IdP for Cloud. The IdP authenticates the user; Observe verifies the signed ID token (authorization-code flow with PKCE, state, and nonce) and maps a claim to the same admin/editor/viewer roles it already uses — a `teploy_role` claim wins, otherwise a group claim is matched to configured admin/editor/viewer groups, otherwise a configurable default (viewer). It then mints Observe's normal JWT, so the SPA and every downstream check treat an SSO session identically; the role is re-read from the token on every login, keeping the IdP authoritative. Password login stays available as the break-glass path. Enable with `OBSERVE_OIDC_ISSUER` + `OBSERVE_OIDC_CLIENT_ID` (see README for the full variable list). When SSO is configured, the first-run open-access grace period is disabled (authentication becomes required). The login page shows an SSO button when it's enabled.

## v0.1.7 — 2026-07-15

Versions v0.1.1 through v0.1.6 shipped without a changelog entry each — not reconstructed here. This entry covers everything since v0.1.6.

### Added

- **Compliance/audit trail subsystem**: an append-only, admin-only audit log (`GET/POST /api/v1/audit`) covering CLI, dash, and Ship as well as Observe's own admin actions. Every mutating admin API call is recorded automatically via middleware (actor, action, target, result, source IP/UA — denied 401/403 attempts included), not just login. Each record carries a keyed HMAC hash chain (`prev_hash`/`hash`) so tampering (modification, deletion, insertion) is detectable via `GET /api/v1/audit/verify`; the signing key lives outside the database. A compliance control-status view (`GET /api/v1/compliance`) reports which controls Observe can verify (audit logging, tamper-evidence, auth, RBAC, write protection). New admin Audit UI view.
- **Signed self-downloading upgrade**: `observe upgrade` fetches, SHA256-verifies, and Ed25519-signature-verifies a release before a health-gated swap; refuses downgrades and rolls back on a failed post-upgrade health check. Systemd-owned lifecycle where installed; Homebrew/container-managed binaries point at the right channel instead of attempting an in-place upgrade. **Load-bearing: this release must be the one downloaded by any instance still on ≤ v0.1.6, since only this and later releases carry the `checksums.txt.sig` asset the new verifier requires.**
- **MinIO accessory** as a self-hosted backup + restore-test target — no external S3 dependency required.
- `/api/v1/logs/batch` — batched log ingest (mirrors the existing `/events/batch` shape, capped at 200 entries, per-entry failure isolation) for real structured-logging clients that flush in batches rather than one request per line.
- Exact trace↔error correlation via `trace_id`/`span_id` (previously a timestamp-window heuristic that could cross-contaminate overlapping traces). Go SDK gains `WithSpan()`; browser and Python SDKs gain passthrough fields.
- Site-scoped error full-text search (each error indexed with a `site_id` facet, search scoped via Nucleus's faceted FTS) so a busy site's results no longer crowd out other sites' search hits.
- `getSessionId()` on the browser SDK, so a separately-loaded script (e.g. the replay tracker) can correlate its recording with the analytics session instead of defaulting to an orphaned empty session ID.
- **Source maps are now retained per release instead of forever.** Every upload
  was stored with no expiry and no delete path anywhere in the service — one
  entry per site x release x file, up to 10 MB each. A production instance
  accumulated 4.8 GB of them, which was enough to push its database past its
  memory limit and make it reject writes for 32 hours. Observe now keeps the
  10 most recently uploaded releases per site (`OBSERVE_SOURCEMAP_KEEP_RELEASES`),
  ordered by last upload so a release stays current while it is still being
  published to. Existing installations reclaim their space on the next upload:
  releases predating the age index sort oldest and prune first. Not implemented
  as a TTL on purpose — Nucleus keeps entries carrying an expiry resident in
  memory, so a TTL would have pinned every source map in RAM until it expired.
- Sourcemap upload now also accepts the site-scoped ingest API key (not just an editor JWT), so CI can upload build-time sourcemaps without a stored interactive-login credential.

### Fixed

- **Browser SDK events were silently dropped entirely**: Go 1.22's stricter method routing rejected the CORS preflight `OPTIONS` request before the CORS middleware ever ran, so every browser `fetch()` with an API key 405'd. Explicit `OPTIONS` handler added ahead of the routing change.
- **Session replay could never actually send data on any site with an API key configured** (the normal state past initial setup) — `sendBeacon` can't carry custom headers and the XHR fallback never set one either, so every replay batch and rage-click report 401'd silently. Now uses `fetch(..., {keepalive:true})` with the key attached when one is configured.
- **OTLP ingest rejected real exporters.** Tracing and metrics ingest didn't decompress gzip bodies (`@vercel/otel`, `OTEL_EXPORTER_OTLP_COMPRESSION=gzip` both 400'd) and didn't accept bare-JSON-number `intValue` attributes (only quoted-string, per a stricter spec reading than what `@vercel/otel`/Next.js actually emit) — real-world exporters couldn't land a single span or metric. Both fixed with size-bounded gzip decompression and a lenient int type.
- **Several JSONB ingest paths silently dropped rows.** Nucleus discards an `INSERT` outright when a JSONB column receives an empty string rather than `null` or a real value — the request reported success (and rollups still wrote) but the underlying row never persisted. Fixed across event properties, errors/llm/logs/replays/surveys ingest, and span attributes/resource/events; audited and fixed at every other JSONB write site in the codebase (view/dashboard config, flags, surveys, SSO, log pipelines) so a lenient-era `''` value can no longer reach the database again. A companion backup-restore fix lands lenient-era `''` values already in old archives as `NULL` on restore against current Nucleus, which rejects them outright.
- `events`/`events_recent` tables dropped inserts from new connections after their post-creation `ALTER TABLE ADD COLUMN` migrations, due to a Nucleus MergeTree engine bug — recreated as plain OLTP tables (data loss accepted; the rows were never actually persisting).
- Timestamp range filters (`WHERE timestamp >= $n`) across stats/goals/funnel/retention/correlation/journeys always returned zero rows — a Go string timestamp was encoding as a quoted text literal that Nucleus can't implicitly cast to `BIGINT`. Now passed as raw `int64`.
- Performance-issue detection counts no longer collapse to 1 on every write — persistence now accumulates via an atomic KV counter instead of relying on read-time dedup of a `replacing_mergetree`, which was overwriting the running count on every detection.
- Cron monitor creation via the HTTP API never set the monitor's `slug` (the input struct was missing the field), so no heartbeat could ever match a monitor created that way — likely never exercised end-to-end before now.

### Security

- `/api/v1/llm/ingest` had no auth middleware at all despite a comment claiming otherwise — anyone could POST arbitrary usage/cost data into any site. The already-existing, correctly-authenticated handler was dead code, never wired to a route; it's wired in now.
- A `teploy.deploy-test.yml` with real secrets inline (session salt, JWT secret, admin password) was sitting untracked in this publicly-mirrored repo. Removed, and `teploy.*.yml` (except the base file) is now gitignored.

### Docs

- `teploy.yml` now documents the persistent-secret pattern (`teploy secret set`) for deploy env instead of the shell-export pattern, which silently ran with empty JWT secret / session salt / admin password whenever nobody remembered to re-export the vars.

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
