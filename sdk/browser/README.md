# @teploy/observe-browser

Browser SDK for [Observe](https://github.com/useteploy/teploy-observe) — self-hosted analytics, errors, logs, sessions.

## Install

```
npm install @teploy/observe-browser
```

## Usage

```ts
import { init, track, captureException } from "@teploy/observe-browser";

init({
  endpoint: "https://observe.example.com",
  siteId: "default",
});

// Custom event. Props are sent nested under `properties`, which is the only
// place the server reads them from.
track("signup", { plan: "pro", source: "hacker_news" });

// Error capture (auto-captured from `window.onerror` / unhandledrejection too)
try {
  risky();
} catch (err) {
  captureException(err as Error, { release: "v1.4.2" });
}
```

## API

| fn | purpose |
|----|---------|
| `init(options)` | Initialize. Starts auto-pageview + buffered flush. |
| `pageview(pathname?)` | Record a pageview. |
| `track(type, props?)` | Record a custom event. Buffered. Props go to `properties` (max 50); the keys the server reads as fields — `url`, `referrer`, `title`, `language`, `screen`, `distinct_id`, `release` — stay top-level. |
| `identify(userId, traits?)` | Associate user id. |
| `reset()` | Clear user (on logout). |
| `captureException(err, ctx?)` | Send an error with stack trace. |
| `log(entry)` | Send a log line. |
| `flush()` | Force-flush buffered events. |
| `getStats()` | O11 diagnostics: delivered/retry/loss counters by reason, live queue depth, and the undelivered snapshot taken at pagehide. Null before init. |

## Delivery, loss, and shutdown semantics

- **Bounded queue, explicit overflow policy.** The queue is capped in both
  event count (200) and bytes (8 MiB) across buffered + pending. At
  admission overflow the **newest** event is dropped; at the pending
  retention cap (200 frozen requests) the **oldest** requests are dropped.
  Every drop is reported through `onError` and counted in `getStats()`.
- **Retry.** Failed batches retry with doubling backoff (default 5
  attempts, 1 s base, 60 s cap) and then count as `retry_exhausted` losses.
  Per-record rejections inside an accepted batch (`rejected` in the server
  ack) are counted as `server_rejected` and not retried — the accepted
  neighbors must not be resent.
- **Shutdown.** `visibilitychange(hidden)` and `pagehide` trigger a
  keepalive flush. At pagehide, still-queued events are snapshotted into
  `getStats().undeliveredAtUnload` and reported via `onError` — the honest
  upper bound, because the page may die before delivery confirmation.
- `kill`-class crashes lose the in-memory queue and its counters with the
  page; no durable spool is claimed.

## Options

| name | default | description |
|------|---------|-------------|
| `endpoint` | — | Required. Observe base URL. |
| `siteId` | `"default"` | Site identifier. |
| `apiKey` | — | API key for server-side ingest paths (errors, logs). |
| `disableAutoPageview` | `false` | Skip auto pageview on init. |
| `batchSize` | `50` | Buffer size before auto-flush. |
| `flushIntervalMs` | `2000` | Time-based flush interval. |
| `maxRetryAttempts` | `5` | Automatic retries per failed batch before it is dropped and reported via `onError`. |
| `retryBackoffMs` | `1000` | Base delay for the retry backoff; doubles per attempt up to 60 s. |
| `requestTimeoutMs` | `10000` | Per-request deadline for every flush send (clamped 250 ms–10 min). A slow endpoint aborts and retries rather than pinning the flush owner. |
| `onError` | — | Called with every delivery failure or drop (the SDK never throws). |
| `onRetry` | — | Called each time a failed batch is scheduled for an automatic retry. |

## Breadcrumbs

Breadcrumbs are attached to `captureException` and `captureMessage`
payloads (wire fields: `type`, `category`, `message`, `data`, `timestamp` in
Unix ms, `level`). Automatic capture is **opt-in**; nothing is patched
unless you ask.

```ts
init({
  endpoint, apiKey,
  breadcrumbs: {                    // or `true` for all defaults
    maxBreadcrumbs: 100,            // ring buffer size (1..1000)
    console: true, clicks: true, navigation: true, fetch: true, xhr: true,
    clickText: false,               // click data never includes text unless true
    beforeBreadcrumb: (c) => (c.category === "console" ? null : c), // edit or drop
  },
});
addBreadcrumb({ category: "checkout", message: "coupon applied", data: { code: "X" } });
await captureMessage("slow render", { level: "warning" });
```

- console: `log/info/warn/error/debug`; arguments are stringified (256 chars).
  Console output can contain personal data: scrub with `beforeBreadcrumb`.
- clicks: `tag#id.class` only. Element text is excluded by default.
- navigation: `pushState`, `replaceState`, `popstate`, `hashchange`; URLs are origin+path.
- fetch / XHR: method, status, URL without query or fragment. The SDK's own
  endpoint is never recorded.
- Patches are reversible (restored on re-init), never throw into host code,
  call the original with its original `this` and arguments, and are skipped
  when there is no `window` (SSR). A `beforeBreadcrumb` that throws drops the crumb.
- `addBreadcrumb` / `clearBreadcrumbs` work without automatic capture.

## Feature flags

```ts
const r = await evaluateFlag("new-checkout", {
  attributes: { plan: "pro" },     // sent as strings; userId defaults to identify()
  default: { enabled: false },     // returned on ANY failure
});
if (r.enabled) { /* r.variant, r.source: "server" | "cache" | "default" */ }
const all = await evaluateFlags(["a", "b"], { default: { enabled: false } });
```

Calls `POST /api/v1/flags/evaluate` (public route, rate limited). Results are
cached in memory (30 s, `init({ flags: { ttlMs, timeoutMs } })`), identical
in-flight calls are coalesced, and requests time out after 3 s. It never
rejects: network errors, timeouts, non-2xx, malformed bodies and server
fail-safe answers (`reason` `unavailable`/`invalid`) resolve to your `default`
with `source: "default"` and `error` set, and are not cached. **No exposure is
recorded** unless you pass `exposure: true` (one `flag_exposure` event per
flag/user/variant) or call `track()` where the variant is rendered. Use
`createFlagClient({ endpoint, siteId })` for a standalone client. See
`docs/sdk/BREADCRUMBS_FLAGS.md`.

## License

MIT
