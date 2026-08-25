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

## Options

| name | default | description |
|------|---------|-------------|
| `endpoint` | — | Required. Observe base URL. |
| `siteId` | `"default"` | Site identifier. |
| `apiKey` | — | API key for server-side ingest paths (errors, logs). |
| `disableAutoPageview` | `false` | Skip auto pageview on init. |
| `batchSize` | `50` | Buffer size before auto-flush. |
| `flushIntervalMs` | `2000` | Time-based flush interval. |

## License

MIT
