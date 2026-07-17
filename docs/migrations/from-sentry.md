# Migrating from Sentry

Observe's error tracking covers the same core surface as Sentry: grouped issues,
stack traces with source maps, breadcrumbs, releases, and webhook/Slack alerts.
This guide walks through the SDK-level changes and the concept mapping.

> **TL;DR** — Replace your Sentry DSN + SDK with one of Observe's SDKs. The
> payload shapes map almost 1-to-1; the only concept Sentry has that Observe
> doesn't is "organization" (Observe uses sites instead).

## Concept mapping

| Sentry                       | Observe                                         |
|------------------------------|-------------------------------------------------|
| Project                      | Site                                            |
| DSN                          | Endpoint URL + API key (headers, not URL)       |
| Issue                        | Issue                                           |
| Event                        | Error event (`error_events` table)              |
| Release                      | Release tag (string field)                      |
| Environment                  | Environment tag (string field)                  |
| Breadcrumb                   | Breadcrumb (JSON array on each event)           |
| Source map                   | Source map (upload via `/api/v1/sourcemaps/upload`) |
| Organization                 | (none — each Observe deployment is one tenant)  |
| Alert rule                   | Alert rule (`/alerts`)                          |
| Webhook integration          | Integration (`/integrations`)                   |

## JavaScript / TypeScript

**Sentry:**
```ts
import * as Sentry from "@sentry/browser";
Sentry.init({ dsn: "https://xxx@sentry.io/1234", release: "v1.4.2" });
Sentry.captureException(err);
```

**Observe:**
```ts
import { init, captureException } from "@teploy/observe-browser";
init({
  endpoint: "https://observe.example.com",
  siteId: "default",
});
captureException(err, { release: "v1.4.2" });
```

### Drop-in shim

If you want to swap Sentry for Observe without touching call sites, register a
shim on `window.Sentry`:

```ts
import { init, captureException, track, identify } from "@teploy/observe-browser";

init({ endpoint: "https://observe.example.com" });

(globalThis as any).Sentry = {
  init: () => {},                          // already initialized
  captureException,                        // same signature
  captureMessage: (msg: string) => track("message", { message: msg }),
  setUser: (u: { id?: string }) => u?.id && identify(u.id, u),
  setTag: () => {},                        // tags roll into events via props
  addBreadcrumb: () => {},                 // Observe captures breadcrumbs automatically
};
```

Existing code calling `Sentry.captureException(err)` continues to work unchanged.

## Go

**Sentry:**
```go
sentry.Init(sentry.ClientOptions{Dsn: "https://xxx@sentry.io/1234"})
sentry.CaptureException(err)
```

**Observe:**
```go
import observe "github.com/useteploy/teploy-observe/sdk/go"

client, _ := observe.New(observe.Options{
    Endpoint: "https://observe.example.com",
    APIKey:   os.Getenv("OBSERVE_API_KEY"),
})
defer client.Close()

client.CaptureException(err, observe.WithRelease("v1.4.2"))
```

## Python

**Sentry:**
```python
import sentry_sdk
sentry_sdk.init(dsn="https://xxx@sentry.io/1234", release="v1.4.2")
sentry_sdk.capture_exception(exc)
```

**Observe:**
```python
import observe_sdk as observe
observe.init(
    endpoint="https://observe.example.com",
    api_key=os.environ["OBSERVE_API_KEY"],
    release="v1.4.2",
)
observe.capture_exception(exc)
```

## Source maps

Same upload model — bundle with sourcemaps, upload for a release, frames get
symbolicated on read.

```bash
# Generate and upload
npm run build -- --sourcemap
curl -X POST https://observe.example.com/api/v1/sourcemaps/upload \
  -H "X-API-Key: $OBSERVE_API_KEY" \
  -F release=v1.4.2 \
  -F file=@dist/app.js.map \
  -F filename=app.js
```

After upload, events with `release_tag=v1.4.2` get their stack frames resolved
to original file/line/col on the `/errors` page.

## Data import (optional)

Sentry lets you export events as JSON. A small script can POST them at Observe's
ingest:

```bash
# For each event JSON file in a Sentry export:
for f in sentry-export/events/*.json; do
  curl -X POST https://observe.example.com/api/v1/errors \
    -H "X-API-Key: $OBSERVE_API_KEY" \
    -H "Content-Type: application/json" \
    -d @"$f"
done
```

Issues are grouped by `group_hash` on ingest, so duplicates merge automatically.

## What doesn't port cleanly

- **Performance / tracing on the Sentry side** — use Observe's OTLP trace
  endpoint instead; Sentry's performance SDK doesn't map 1-to-1.
- **Sentry Replay** — Observe has its own replay format (`observe-replay.js`)
  rather than rrweb.
- **Organization / team permissions** — Observe is single-tenant per deployment.

## Checklist

- [ ] Swap SDK init + `captureException` calls (or use the shim above).
- [ ] Upload source maps for your current release.
- [ ] Confirm errors appear at `/errors` with resolved stack frames.
- [ ] Set up alert rules at `/alerts` for your critical thresholds.
- [ ] Delete the Sentry DSN from your env and repo secrets.
