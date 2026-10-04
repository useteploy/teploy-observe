# Replay console and network capture, and replay list filters

## Capture (opt-in, default OFF)

Load `observe-replay-capture.js` after a replay recorder. Nothing is patched
unless the script tag opts in:

```html
<script src="/t/observe-replay-capture.js" data-console="true" data-network="true"></script>
```

- Console: levels log/info/warn/error. Arguments are stringified with depth,
  key and item limits, scrubbed against a secret denylist (password, token,
  api key, authorization, bearer, JWT, long hex/opaque tokens), cut to 1 KiB,
  capped at 200 per session (`data-max-console` can lower it).
- Network (fetch and XHR): method, URL with query, fragment and credentials
  removed (opaque path segments masked), status (0 = failed), duration and
  size from `Content-Length`. No bodies, no headers. Capped at 500 per session.
- Patches are reversible (`window.observeReplayCapture.stop()`), never throw
  into the host page, and skip Observe's own `/api/v1/` traffic.
- Records ride the v2 replay batches as `console` / `network` events through
  `window.observeReplay.pushEvent`. The server (`internal/replays/capture.go`)
  rejects the whole batch on an unknown key, level, oversize message, bad URL
  or out-of-range number, and re-scrubs accepted records.

`observe-replay.js` exposes `pushEvent`. The prebuilt `observe-replay-delta.js`
bundle does not yet: its source (`replay-delta/src/recorder.mjs`) does, so
capture is inert on the delta recorder until the bundle is rebuilt.

## List filters

`GET /api/v1/replays` accepts `has_errors=true`, `min_duration` (ms),
`url_contains` (case-insensitive, wildcards literal, max 200 bytes) and
`distinct_id` (raw id; hashed with the site's ingest derivation before the
comparison), alongside `limit`/`offset`. Reads pass the query guard.
