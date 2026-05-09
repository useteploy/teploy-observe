# API Response Shape Convention

Every `/api/v1/*` endpoint follows one of two response shapes. There is no
third option.

## 1. List endpoints — bare JSON arrays, never `null`

A handler whose Go return type is `[]T` MUST always serialize as a JSON array.
On empty results the body is `[]`, never `null`.

```http
GET /api/v1/issues?site_id=demo
200 OK
[]
```

In Go this means coercing nil slices before returning. Use the helper:

```go
return emptyOnNil(svc.List(ctx, siteID))
```

`emptyOnNil` lives in `cmd/observe/main.go`. It is a thin wrapper that turns
`(nil, nil)` into `([]T{}, nil)` so the JSON encoder never emits `null`.

The root cause this guards against: `nucleus.Query[T]` declares
`var results []T` and returns it unchanged when zero rows match, so the
result is a typed nil slice. `encoding/json` encodes a nil slice as `null`.

## 2. Detail endpoints — single objects

A handler whose return type is a struct (`T`) MUST serialize as a JSON object.
404 is returned when the entity does not exist; never an empty object with
zero values.

```http
GET /api/v1/issues/abc123?site_id=demo
404 Not Found
{ "type": "...", "title": "issue not found", "status": 404 }
```

When a detail response needs to bundle related collections (e.g. dashboard
+ panels), define an explicit response struct with named fields. **Do not**
use Go struct embedding for this — the JSON output flattens the embedded
fields, which silently breaks any TS client that expects a nested shape.

```go
type dashboardDetail struct {
    Dashboard dashboards.Dashboard `json:"dashboard"`
    Panels    []dashboards.Panel   `json:"panels"`
}
```

## What is forbidden

- Wrapping list responses as `{"items": [...]}` or `{"data": [...]}`.
  Lists are bare arrays. Period.
- Returning `null` for an absent list. Always `[]`.
- Mixing shapes per call (e.g. object on success, array on error). Errors
  always use RFC 7807 `application/problem+json`.
