# Migrating to Observe (O11)

Tested paths from installation to a **query-visible first event**, plus
dual-write recipes. The rule for every recipe: **never delete your old
integration first.** Run both, compare, then remove the old one when the
Observe data has proven itself. Every step below is exercised by the SDK
test suites and the first-event e2e check (`sdk/e2e/`).

## 1. Credential (all SDKs)

Observe ingest requires a **site-scoped API key**. Keyless requests get 401.

1. Start your Observe server and sign in as admin.
2. `Settings > API keys` (or `POST /api/v1/sites/{site_id}/keys` with an
   admin JWT) — create a key labeled e.g. `sdk-migration`. Default scope is
   `telemetry` (append-only ingest; cannot read or delete anything).
3. The plaintext key (`obs_...`) is shown once — store it in your secret
   manager.

Browser-deployed keys are public by design: scope them to one site,
telemetry-only, and they can only append.

## 2. First event, per SDK

### Browser / Node (`@teploy/observe-browser`)

```ts
import { init, track, flush, getStats } from "@teploy/observe-browser";

init({
  endpoint: "https://observe.example.com",
  siteId: "default",
  apiKey: "obs_...", // site-scoped telemetry key
  release: "v1.4.2",
});
track("migration_probe", { source: "o11-docs" });
await flush();
const stats = getStats(); // deliveredEvents === 1 when the server accepted it
```

Verify query-visible: `GET /api/v1/stats/events?site_id=default` (JWT auth)
lists `migration_probe`.

### Go (`sdk/go`)

```go
client, err := observe.New(observe.Options{
    Endpoint: "https://observe.example.com",
    APIKey:   "obs_...",
    SiteID:   "default",
})
if err != nil { log.Fatal(err) }
defer client.Shutdown(context.Background()) // flush(timeout) semantics

client.Info("migration probe", observe.F("source", "o11-docs"))
if err := client.Flush(context.Background()); err != nil { log.Fatal(err) }
```

Verify: `GET /api/v1/logs/search?site_id=default` returns the line.

### Python (`sdk/python`)

```python
import observe_sdk

client = observe_sdk.init(
    endpoint="https://observe.example.com",
    api_key="obs_...",
    site_id="default",
)
client.info("migration probe", source="o11-docs")
client.flush(timeout=10.0)
client.close(timeout=10.0)
```

Verify: `GET /api/v1/logs/search?site_id=default` returns the line.

## 3. Dual-write: Sentry (Node)

Keep `@sentry/node` installed and **capture into both** during migration:

```ts
import * as Sentry from "@sentry/node";
import * as ObserveShim from "@teploy/observe-sentry-shim";

ObserveShim.init({
  endpoint: "https://observe.example.com",
  siteId: "default",
  apiKey: "obs_...",
  release: process.env.RELEASE,
});

export function capture(err: unknown): void {
  Sentry.captureException(err);            // old path — unchanged
  ObserveShim.captureException(err);       // new path
}
```

- Wrap per-request state with `ObserveShim.withRequestScope` (see
  [COMPATIBILITY](COMPATIBILITY.md#node-request-local-scope)); it does not
  touch Sentry's handlers, so the two run side by side.
- Compare issue counts and affected users for the same release window in
  both dashboards before switching alerting.
- Reversal at any moment: delete the `ObserveShim.*` calls. Nothing else
  changes; Sentry never knew about Observe.

## 4. Dual-write: PostHog (browser)

Keep the PostHog snippet and mirror captures:

```ts
import posthog from "posthog-js";
import { init, track } from "@teploy/observe-browser";

posthog.init("<ph_key>", { api_host: "https://eu.i.posthog.com" });
init({
  endpoint: "https://observe.example.com",
  siteId: "default",
  apiKey: "obs_...",
  release: DOCUMENTED_RELEASE,
});

export function capture(name: string, props: Record<string, unknown>): void {
  posthog.capture(name, props); // old path — unchanged
  track(name, props);           // new path
}
```

- Identity: call both `posthog.identify(id)` and Observe `identify(id)`
  after login. Observe hashes the id server-side; comparison dashboards
  should be built on event volumes, not raw ids.
- Differences that will show up in comparison: no autocapture, no full-URL
  capture (origin+path + utm fields only) — see
  [COMPATIBILITY](COMPATIBILITY.md#posthog-posthog-js--teployobserve-browser).
- Reversal at any moment: remove the `track` wrapper call.

## 5. Shutdown discipline (all SDKs)

Every SDK reports unflushed events as **loss counts** at shutdown instead of
swallowing them:

- Browser: `flush()` before `pagehide` where possible; the SDK flushes on
  `visibilitychange(hidden)` and `pagehide` with keepalive; anything
  undeliverable is counted in `getStats()` and reported via `onError`.
- Go: `Shutdown(ctx)` carries your deadline; leftovers are counted in
  `Stats()`. Recipe: catch SIGINT/SIGTERM, call `Shutdown(ctxWith5s)`, exit.
- Python: `close(timeout=...)`; leftovers are counted in `stats()`.
- Sentry shim: `await flush(timeout)` before process exit.

Kill -9 (or a crash): all SDK queues are in-memory by design; the queue's
contents and their counters are lost with the process. That is the honest
trade-off — durable local spooling is deliberately not claimed.
