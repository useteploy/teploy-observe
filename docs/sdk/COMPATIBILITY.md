# SDK compatibility tables (O11)

Precise, per-API compatibility for Observe's SDKs against the libraries
people migrate from. Every entry is one of four statuses:

- **Supported** — same call, same intent, same observable result.
- **Changed semantics** — the call works and does something useful, but the
  contract differs from the original in a way you must know about.
- **Intentional no-op** — accepted so call sites do not break, does nothing,
  emits a one-time diagnostic (where safe) pointing back to this file.
- **Unsupported** — not accepted or not present; using it fails visibly
  (compile error / runtime error), never silently.

The sentry-shim replaces silent no-ops with actionable diagnostics where it
is safe to do so: `console.warn` **once per no-op class** (not per call), with
an anchor into this document. Warnings never fire when `debug: true` (dry
run).

## Sentry (`@sentry/node` → `@teploy/observe-sentry-shim`)

### capture / scope

| Sentry API | Status | Notes |
|---|---|---|
| `init({ dsn })` | Supported | Observe-style `https://host/__observe__/<site>` and classic `https://key@host/<project>` both parsed. |
| `init({ endpoint, siteId })` | Supported | Observe-native alternative to a DSN. |
| `init({ apiKey })` | Supported | **Required in practice**: Observe ingest rejects keyless requests with 401. Site-scoped telemetry key (`Settings > API keys`). |
| `init({ release, environment })` | Supported | Stamped on every envelope. |
| `init({ beforeSend })` | Supported | Async; `null` return drops the event. Errors in the hook fall back to the unmutated event. |
| `init({ maxBreadcrumbs })` | Supported | Default 100 per scope. |
| `init({ attachStacktrace })` | Supported | Default `true`. |
| `init({ tracesSampleRate })` | Intentional no-op | Not a sampling knob here; spans are always recorded when created. Warns once. See [#sentry-noop](#sentry-intentional-no-ops). |
| `init({ profilesSampleRate })` | Intentional no-op | No profiler. Warns once. |
| `init({ integrations })` | Intentional no-op | Accepted, ignored — the shim has no integration machinery. Warns once. |
| `init({ autoSessionTracking })` | Intentional no-op | Release health is computed server-side from `release` tags. Warns once. |
| `captureException(err, hint?)` | Supported | `hint.mechanism` / `hint.level` honored. Returns the event id the server will dedupe on. |
| `captureMessage(msg, level?)` | Changed semantics | Routed to **logs** (`/api/v1/logs`), not an issue-forming event. Group by message happens in the logs view, not the issues UI. |
| `captureEvent(event)` | Unsupported | Not implemented. |
| `captureFeedback(...)` | Unsupported | Not implemented. |
| `lastEventId()` | Unsupported | Not exported. `captureException`'s return value is the event id. |
| `setUser(user)` / `setUser(null)` | Changed semantics | `user.id` maps to `distinct_id`, which the server **hashes with the per-site salt** before storage — the raw id does not persist (unless the site opts into `raw_distinct_id`). Whole user object rides in `contexts.user`. |
| `setTag` / `setTags` | Supported | Stored inside `contexts.tags`. |
| `setContext(key, ctx)` | Supported | Merged into `contexts`. |
| `setExtra` / `setExtras` | Supported | Merged into `extra`. |
| `setFingerprint(parts)` | Supported | Passed through; Observe's grouping consumes it. |
| `setLevel(level)` | Supported | Applies to the next captured event. |
| `addBreadcrumb(crumb)` | Supported | Bounded ring per scope (`maxBreadcrumbs`). |
| `withScope(fn)` | Supported | Fork; mutations do not leak out. **Synchronous stack-based only** — see [request-local scope](#node-request-local-scope). |
| `configureScope(fn)` | Supported | Mutates the active scope. |
| `getCurrentHub()` / `getCurrentScope()` | Supported | Minimal hub/scope objects. |

### transport / lifecycle

| Sentry API | Status | Notes |
|---|---|---|
| `flush(timeout?)` | Changed semantics | The shim does not batch; `flush` awaits in-flight POSTs (bounded by `timeout`, default 2 s) and reports what could not be confirmed as a loss through `getStats()`. |
| `close(timeout?)` | Changed semantics | Awaits in-flight like `flush`, then clears config. |
| `addEventProcessor(fn)` | Unsupported | Use `beforeSend`. |
| `createTransport` / `makeNodeTransport` | Unsupported | The shim owns its transport (plain `fetch`, no redirects, `X-API-Key` header). |
| `Sentry.Handlers.requestHandler / errorHandler / tracingHandler` | Unsupported | Express/Connect handlers are not provided. Use `withRequestScope` for per-request isolation (see below) and your own error middleware. |
| `Sentry.logger` / `Sentry.metrics` | Unsupported | Use the dedicated Go/Python SDKs or the logs API. |

### tracing

| Sentry API | Status | Notes |
|---|---|---|
| `startTransaction({ name, op })` | Changed semantics | Returns a span-like `Span`. On `finish()` it POSTs **one structured log entry** (`span: <name>` with `duration_ms`, `op`, status, data) — it does **not** create an OTLP trace. Trace search will not show it; logs search will. |
| `startSpan({ name, op }, fn)` | Changed semantics | Same log-based stub as above, callback style. |
| `Span.startChild(...)` | Changed semantics | Returns an independent stub (no parent/child linkage on the server). |
| `Span.setData / setStatus / setTag` | Supported | Ride the log entry's attributes. |
| `continueTrace(...)` | Unsupported | No distributed-context propagation. |
| Profiling APIs | Unsupported | — |

### sessions / cron / release health

| Sentry API | Status | Notes |
|---|---|---|
| `startSession()` / `endSession()` | Intentional no-op | Warns once. Sessions are inferred server-side. |
| `captureCheckIn(...)` / crons | Unsupported | Observe cron monitoring is configured server-side (`/api/v1/checkin/`), not through this shim. |
| Release health APIs | Intentional no-op | Computed from `release` tags + event volume, not client session tracking. |

#### Sentry intentional no-ops

The classes that warn once per process (when not in `debug` dry-run):
`tracesSampleRate`, `profilesSampleRate`, `integrations`,
`autoSessionTracking`, `startSession`/`endSession`. Each warning names the
option and points here.

### Node request-local scope

The module-level scope is **shared by default** (like Sentry's global scope).
Per-request isolation without thread-locals would leak one request's user
into another's events — exactly the failure the O11 spec calls out. The shim
therefore ships `withRequestScope(fn)`, backed by `node:async_hooks`
`AsyncLocalStorage` when available:

```ts
import * as Sentry from "@teploy/observe-sentry-shim";

// once per request (Express example; any framework works):
app.use((req, res, next) => Sentry.withRequestScope(() => next()));
```

Inside a request scope, `setUser`/`setTag`/`withScope`/captures see a fork
of the global scope; concurrent requests cannot observe each other's
mutations. Without AsyncLocalStorage (browsers, odd runtimes) the shim falls
back to the shared global scope and **warns once** — in that mode, attribute
per-request data through `withScope` (synchronous) or capture-call hints
instead.

## PostHog (`posthog-js` → `@teploy/observe-browser`)

| posthog-js API | Status | Notes |
|---|---|---|
| `init(api_key, opts)` | Changed semantics | Takes `{ endpoint, siteId, apiKey }`. The key is a **site-scoped telemetry key**, not a project API key; ingest rejects keyless requests (401). |
| `capture(event, props)` | Supported | `track(eventType, props)` — same shape; props must nest under `properties` (server reads only those). |
| `capture($pageview)` | Supported | `pageview()` — origin+path only; see URL contract below. |
| `identify(id, traits)` | Supported | Emits `$identify`; traits ride as event properties minus identity-shaped keys. |
| `identify` persistence | Changed semantics | PostHog persists identity in localStorage across visits; the Observe SDK keeps it in memory only — call `identify` again after page reload (recommended: after your auth/session bootstrap). |
| `distinct_id` storage | Changed semantics | Hashed server-side with the per-site salt before storage (PostHog stores raw by default). Opt-in `raw_distinct_id` per site flips this. |
| `reset()` | Supported | Clears user, rotates session id. |
| `autocapture` | Unsupported | No automatic element-click capture. Record explicit `track()` calls. |
| Feature flags (`onFeatureFlags`, `getFeatureFlag`) | Unsupported | Not in the browser SDK; flags are served by the server API (`/api/v1/flags/...`) for trusted backends. |
| `posthog.sessionId` | Supported | `getSessionId()` — also correlates replay recordings. |
| Toolbar / session replay recording controls | Unsupported | Replay is a separate script (`observe-replay.js`); this SDK only attaches `replay_id` to errors when that script is present. |
| Surveys | Unsupported | — |
| Groups (`group(...)`) | Unsupported | No group analytics yet. |
| `alias(...)` | Unsupported | Identity merging is not part of the analytics contract. |
| People (`people.set(...)`) | Unsupported | Use `identify` traits. |
| Persistence (`persistence: localStorage`) | Intentional no-op | No durable client state by design; queue is in-memory with explicit loss counters. |

### URL/privacy contract differences

The browser SDK sends **origin+path only** — query strings and fragments
never leave the page. Attribution rides explicit `utm_*` fields extracted
from an allowlist. Code that relied on PostHog's full-URL capture must read
attribution from the utm fields.
