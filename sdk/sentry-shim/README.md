# @teploy/observe-sentry-shim

Drop-in replacement for `@sentry/node` that ships events to a self-hosted
[Observe](https://github.com/useteploy/teploy-observe) deployment instead of
Sentry. Zero runtime dependencies — the shim builds JSON envelopes and POSTs
them directly to Observe's ingest endpoints.

## Install

```sh
npm install @teploy/observe-sentry-shim
```

## Use

Swap your import. Existing call sites keep working.

```diff
- import * as Sentry from "@sentry/node";
+ import * as Sentry from "@teploy/observe-sentry-shim";

  Sentry.init({
-   dsn: "https://abc@sentry.io/1234",
+   dsn: "https://observe.example.com/__observe__/default",
    release: "v1.4.2",
    environment: "production",
  });

  Sentry.captureException(err);
  Sentry.setUser({ id: "u_42", email: "alice@example.com" });
  Sentry.captureMessage("cache miss", "warning");
```

DSN parsing accepts both shapes:

- Observe-style: `https://<host>/__observe__/<site_id>`
- Classic Sentry: `https://<key>@<host>/<project>` — origin becomes the
  endpoint, the trailing path segment becomes the site id.

You can skip the DSN entirely and pass `endpoint` + `siteId` directly.

## Supported Sentry APIs

| Sentry API                                    | Routed to                          |
|-----------------------------------------------|------------------------------------|
| `init(options)`                               | configures the shim                |
| `captureException(err, hint?)`                | `POST /api/v1/errors`              |
| `captureMessage(msg, level?)`                 | `POST /api/v1/logs`                |
| `setUser(user)` / `setUser(null)`             | scope user                         |
| `setTag(k, v)` / `setTags(map)`               | scope tags                         |
| `setContext(key, ctx)`                        | scope contexts                     |
| `setExtra(k, v)` / `setExtras(map)`           | scope extras                       |
| `setFingerprint(parts)`                       | overrides issue grouping           |
| `setLevel(level)`                             | scope level                        |
| `addBreadcrumb(crumb)`                        | scope breadcrumbs (max 100)        |
| `withScope(fn)`                               | forks scope, restores on return    |
| `configureScope(fn)`                          | mutates active scope               |
| `startTransaction({ name, op })`              | returns a `Span` stub              |
| `startSpan({ name, op }, fn)`                 | callback-style span helper         |
| `flush(timeout?)` / `close(timeout?)`         | await in-flight sends, bounded by `timeout` (default 2 s) |
| `getCurrentHub()` / `getCurrentScope()`       | minimal Hub/Scope stubs            |
| `withRequestScope(fn)`                        | O11: per-request scope isolation (see below) |
| `getStats()`                                  | O11: delivery/loss counters        |

## Request-local scope (concurrent-request safety)

The module-level scope is shared by default (like Sentry's global scope).
In a concurrent server that means `setUser` in one request leaks into every
other request's captures. Wrap each request with `withRequestScope` —
backed by `node:async_hooks` `AsyncLocalStorage`:

```ts
import * as Sentry from "@teploy/observe-sentry-shim";

// Express (any framework — wrap whatever starts a request's async work):
app.use((req, res, next) => Sentry.withRequestScope(() => next()));
```

Inside a request scope, `setUser`/`setTag`/`withScope`/captures see a fork
of the global scope; concurrent requests cannot observe each other's
mutations. Full contract: `docs/sdk/COMPATIBILITY.md#node-request-local-scope`.

## Honest no-ops and visible losses (O11)

Intentional no-ops warn ONCE per class (console.warn with a pointer to
`docs/sdk/COMPATIBILITY.md`) instead of failing silently; warnings are
suppressed in `debug` dry-run mode.

The shim never throws out of `capture*` calls (Sentry compat), but losses
are visible: every failed send (network error, non-2xx) is counted in
`getStats()` and reported through `init({ onError })`. Each capture is one
attempt — no retry, no queue (see the compatibility table); use the
first-party SDKs when you need bounded queues and retry.

`flush(timeout)` awaits in-flight sends bounded by `timeout`; sends still
unconfirmed when it gives up are snapshotted into
`getStats().unconfirmedAtFlush` and reported — the honest upper bound.
Call `await flush()` before process exit on your signal paths.

## Intentional no-ops

Sentry features without a 1-to-1 Observe equivalent are accepted (for compat)
but do nothing:

- `tracesSampleRate`, `profilesSampleRate` — Observe accepts every span;
  sampling lives at the OTLP exporter or ingest rate-limit layer.
- `integrations: [...]` — Sentry's integration array is silently ignored.
  Use Observe's official SDKs (`@teploy/observe-browser`, `observe-sdk` for Python,
  the Go SDK) for framework hooks.
- `autoSessionTracking` / `startSession` / `endSession` — Observe derives
  release health from the `release` tag and event volume server-side.
- `Span.finish()` is fired but transactions are emitted as logs, not as
  Sentry-style envelopes. Wire OTLP via `@opentelemetry/exporter-trace-otlp-http`
  pointed at `/api/v1/v1/traces` for proper distributed tracing.
- `Sentry.Replay` — Observe ships its own `observe-replay.js` (rrweb-free).

## License

MIT
