# Breadcrumbs and feature flags in the SDKs

Applies to the browser, Python and Go SDKs. The Sentry shim keeps its own
scope-based breadcrumbs and is unchanged.

## Breadcrumb wire shape

Sent in the `breadcrumbs` array of `POST /api/v1/errors`; matches
`internal/errors.Breadcrumb`.

| Field | Type | Notes |
|---|---|---|
| `type` | string | `default`, `console`, `user`, `navigation`, `http`, `log` |
| `category` | string | `console`, `click`, `navigation`, `fetch`, `xhr`, logger name, or yours |
| `message` | string | at most 256 chars |
| `data` | object | optional; JSON-serializable, over 1 KiB replaced by `{_truncated: true}` |
| `timestamp` | integer | Unix epoch milliseconds |
| `level` | string | `debug`, `info`, `warning`, `error` |

All SDKs keep a bounded ring (default 100, max 1000; oldest dropped), attach at
most 32 KiB of the newest breadcrumbs per payload, omit the key when empty,
and never let recording raise into application code. A scrub hook
(`beforeBreadcrumb` / `before_breadcrumb` / `BeforeBreadcrumb`) can edit or
drop; a hook that fails drops the breadcrumb.

| | Browser | Python | Go |
|---|---|---|---|
| Manual | `addBreadcrumb` | `add_breadcrumb` | `AddBreadcrumb` |
| Automatic | `init({ breadcrumbs })`: console, clicks, navigation, fetch, XHR | `logging_breadcrumbs=True`: `logging` >= WARNING | `NewSlogBreadcrumbHandler` / `SlogHandler.WithBreadcrumbs` |
| Message capture | `captureMessage` | `capture_message` | `CaptureMessage` |

Privacy defaults: browser clicks record `tag#id.class` only (no text unless
`clickText: true`); URLs are origin+path with query, fragment and credentials
removed; console arguments are recorded and should be scrubbed if they can
hold personal data. Buffers are process-wide (Python, Go) or page-wide
(browser), not request-scoped.

## Flag evaluation contract

`POST {endpoint}/api/v1/flags/evaluate` (public, rate limited per IP and site)

```
request:  {"site_id", "flag_key", "user_id", "context": {string: string}}
response: {"enabled": bool, "variant"?: string,
           "reason": "evaluated" | "unavailable" | "invalid", "detail"?: string}
```

Only POST exists. The server logs every enabled evaluation, so avoid calling
it per render.

| | Browser | Python | Go |
|---|---|---|---|
| Call | `evaluateFlag`, `evaluateFlags` | `evaluate_flag` | `EvaluateFlag` |
| Default timeout | 3 s | min(client timeout, 3 s) | 3 s |
| Cache | in-memory TTL 30 s, bounded | none | none |
| Coalescing | identical in-flight calls | no | no |

Failure policy (all SDKs): never throw. Network error, timeout, non-2xx,
malformed body, or a server answer with `reason` other than `evaluated`
returns the caller-supplied default (disabled when none) with
`source: "default"` and the cause in `error`. Defaulted results are not cached.

Exposure safety: evaluating a flag is not exposure. No SDK records an exposure
automatically. The browser can record one `flag_exposure` event per
flag/user/variant with `exposure: true`; otherwise call `track()` where the
variant is actually shown.
