# @observe/sentry-shim

Drop-in replacement for `@sentry/browser` that sends to Observe.

## Install

```
npm install @observe/sentry-shim @observe/browser
```

## Use

```diff
- import * as Sentry from "@sentry/browser";
+ import * as Sentry from "@observe/sentry-shim";

Sentry.init({
-  dsn: "https://abc@sentry.io/1234",
+  dsn: "https://observe.example.com/__observe__/default",
   release: "v1.4.2",
});

Sentry.captureException(err);
Sentry.setUser({ id: "u_42", email: "alice@example.com" });
Sentry.captureMessage("cache miss", "warning");
```

The DSN is parsed as `<observe-endpoint>/__observe__/<site_id>`. Classic Sentry
DSNs also work — the origin becomes the endpoint and the trailing path segment
becomes the site id.

## Supported surface

| Sentry API                    | Shim behavior                            |
|-------------------------------|------------------------------------------|
| `init()`                       | Forwards to Observe `init`.              |
| `captureException(err)`        | Forwards with current release.           |
| `captureMessage(msg, level)`   | Emits a `message` event.                 |
| `setUser({id,email})`          | Calls Observe `identify` / `reset`.      |
| `setTag`, `setContext`, `setExtra` | No-op (use per-event props).        |
| `addBreadcrumb`                | No-op (automatic).                       |
| `withScope(fn)`                | Runs `fn` with a stub scope.             |

Not supported: performance transactions, `BrowserTracing`, `Replay` (use
Observe's own replay tracker), `Integrations` array (ignored).

## License

MIT
