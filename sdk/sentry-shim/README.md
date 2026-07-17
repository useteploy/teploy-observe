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
| `flush(timeout?)` / `close(timeout?)`         | resolve immediately (no batching)  |
| `getCurrentHub()` / `getCurrentScope()`       | minimal Hub/Scope stubs            |

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
