# Migrating from Sentry

Observe's error tracking covers the same core surface as Sentry: grouped issues,
stack traces with source maps, breadcrumbs, releases, and webhook alerts.
This guide walks through the SDK-level changes and the concept mapping.

> **TL;DR** - Stock Sentry SDKs (`sentry-sdk`, `@sentry/browser`,
> `@sentry/node`, `sentry-go`) can point their DSN at Observe:
> `https://<observe_api_key>@<host>/<site_id>`. Error events are ingested;
> transactions, sessions, replays, profiles, attachments, check-ins and logs
> are acknowledged with 200 and dropped. For the full Observe feature set
> (or if you prefer a smaller dependency) swap to an Observe SDK or the
> `@teploy/observe-sentry-shim` package. The only Sentry concept Observe lacks
> is "organization" (Observe uses sites instead).
>
> The key must be an Observe API key with the telemetry capability, bound to
> the site named in the DSN. Details: [Sentry wire protocol](../sdk/COMPATIBILITY.md#sentry-wire-protocol-stock-sentry-sdks-no-shim).

## Concept mapping

| Sentry                       | Observe                                         |
|------------------------------|-------------------------------------------------|
| Project                      | Site                                            |
| DSN                          | `https://<api_key>@host/<site_id>` (wire-compatible), or endpoint URL + API key |
| Issue                        | Issue                                           |
| Event                        | Error event (`error_events` table)              |
| Release                      | Release tag (string field)                      |
| Environment                  | Environment tag (string field)                  |
| Breadcrumb                   | Breadcrumb (JSON array on each event; see [Breadcrumbs](#breadcrumbs)) |
| Source map                   | Source map (upload via `/api/v1/sourcemaps/upload`) |
| Organization                 | (none — each Observe deployment is one tenant)  |
| Alert rule                   | Alert rule (`/alerts`)                          |
| Webhook integration          | Webhook (alert delivery); `/integrations` entries are not fired by alerts today |

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

To keep Sentry call sites, use the `@teploy/observe-sentry-shim` package
(`sdk/sentry-shim`) instead of writing your own `window.Sentry` object. It
exports the Sentry-named functions and posts to Observe's API
(`captureException` to `/api/v1/errors`, `captureMessage` to `/api/v1/logs`):

```ts
import * as Sentry from "@teploy/observe-sentry-shim";

Sentry.init({
  endpoint: "https://observe.example.com",
  siteId: "default",
  apiKey: process.env.OBSERVE_API_KEY, // required: keyless ingest gets 401
  release: "v1.4.2",
});
Sentry.captureException(err);
```

`init({ dsn })` is accepted, but only the shim parses it (an Observe-style
`https://host/__observe__/<site>` or a classic `https://key@host/<project>`
whose last path segment is used as the site id). That is the shim reading a
string, not Observe accepting Sentry traffic; the shim still sends Observe's
own payloads and still needs `apiKey`.

`setTag`, `setUser`, `setContext`, `setExtra` and `addBreadcrumb` are
**implemented**, not no-ops: tags ride in `contexts.tags`, breadcrumbs are kept
in a bounded per-scope ring (`maxBreadcrumbs`, default 100) and sent with the
event. Some APIs are no-ops or unsupported (`tracesSampleRate`, `startSession`,
`captureEvent`, `addEventProcessor`, ...). The per-API status table is
[docs/sdk/COMPATIBILITY.md](../sdk/COMPATIBILITY.md); treat it as the authority.

#### Breadcrumbs

Automatic breadcrumb capture (clicks, navigation, console, fetch/XHR) exists
only in the browser tracker script `observe-errors.js`
(`cmd/observe/tracker/observe-errors.js`, bounded by `data-max-breadcrumbs`,
default 30). The Python and Go SDKs and the `@teploy/observe-browser` package
do **not** collect breadcrumbs automatically, and the Python and Go SDKs have
no breadcrumb API at all. The shim and the tracker's `addBreadcrumb` let you
add them by hand.

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
# Generate and upload. Source-map upload is a PUBLISH capability, not
# telemetry: use a key created for CI with scopes ["publish"] (R07) — the
# browser-exposed telemetry key is rejected here.
#   POST /api/v1/sites/{site_id}/keys  {"label":"ci","scopes":["publish"]}
curl -X POST https://observe.example.com/api/v1/sourcemaps/upload \
  -H "X-API-Key: $OBSERVE_PUBLISH_KEY" \
  -F release=v1.4.2 \
  -F filename=app.js \
  -F sourcemap=@dist/app.js.map
```

The multipart form fields are read by `srcmapUploadHandler` in
`cmd/observe/main.go`: `release` and `filename` are required text fields, and
the map itself must be in the file field named `sourcemap` (a field named
`file` is rejected with `sourcemap file required`). With an API key the site
comes from the key; `site_id` is only read when authenticating with an
editor/admin JWT. The whole body is capped at 12 MiB and the map at 10 MiB.

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

- **Performance / tracing on the Sentry side** — transactions and spans sent by
  a stock Sentry SDK are acknowledged and dropped; use Observe's OTLP trace
  endpoint instead.
- **Release-health sessions, profiles, crons, attachments, user feedback** —
  acknowledged and dropped (stock SDK path).
- **Sentry Replay** — dropped on the Sentry wire; Observe has its own replay
  format (`observe-replay.js`) rather than rrweb.
- **Event timestamps** — events are timestamped at ingest.
- **Organization / team permissions** — Observe is single-tenant per deployment.

## Checklist

- [ ] Swap SDK init + `captureException` calls (or use `@teploy/observe-sentry-shim`).
- [ ] Upload source maps for your current release.
- [ ] Confirm errors appear at `/errors` with resolved stack frames.
- [ ] Set up alert rules at `/alerts` for your critical thresholds.
- [ ] Delete the Sentry DSN from your env and repo secrets.
