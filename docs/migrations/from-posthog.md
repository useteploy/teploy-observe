# Migrating from PostHog

Observe covers the analytics + feature flags + session replay surface that
makes up PostHog's core. This guide walks through the concept mapping and SDK
swap.

> **TL;DR** — Observe's event model and flag evaluation are simpler than
> PostHog's (fewer concepts, no Kafka/Clickhouse) but cover the 80% path.
> Cohorts exist but are simpler than PostHog's (AND-only rules, see
> [Cohorts](#cohorts)); you lose retention-by-SQL and gain a single binary.

## Concept mapping

| PostHog                          | Observe                                          |
|----------------------------------|--------------------------------------------------|
| Project                          | Site                                             |
| Person                           | Session / identified user (via `identify`)       |
| Event (`$pageview`, `$autocapture`, custom) | Event (`pageview`, `click`, custom)    |
| Insight (trend)                  | Dashboard panel (`timeseries` or `metric`)       |
| Funnel                           | Funnel (`/insights` → Funnels)                   |
| Retention                        | Retention (`/insights` → Retention)              |
| Path analysis                    | Journeys (`/insights` → Journeys)                |
| Session recording                | Session replay (`/sessions`)                     |
| Feature flag                     | Feature flag (`/flags`)                          |
| Experiment                       | Experiment (`/experiments`)                      |
| Survey                           | Survey (`/surveys`)                              |
| Cohort                           | Cohort (`/api/v1/cohorts`; dynamic, AND-only, see [Cohorts](#cohorts)) |
| Plugin / app                     | Webhooks (alert delivery); `/integrations` are test/replay-only today |

## JavaScript / TypeScript

**PostHog:**
```ts
import posthog from "posthog-js";
posthog.init("phc_xxx", { api_host: "https://app.posthog.com" });
posthog.capture("signup", { plan: "pro" });
posthog.identify(user.id, { email: user.email });
if (posthog.isFeatureEnabled("new-checkout")) { /* ... */ }
```

**Observe:**
```ts
import { init, track, identify } from "@teploy/observe-browser";
init({ endpoint: "https://observe.example.com", siteId: "default" });
track("signup", { plan: "pro" });
identify(user.id, { email: user.email });

// Flag evaluation is a plain HTTP call. @teploy/observe-browser has no
// flag client, so you write this fetch yourself. The endpoint is public
// (no API key or JWT) and rate limited per IP and per site/IP:
const r = await fetch("https://observe.example.com/api/v1/flags/evaluate", {
  method: "POST",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify({ site_id: "default", flag_key: "new-checkout", user_id: user.id }),
});
const { enabled, variant } = await r.json();
```

The handler (`flagEvaluateHandler` in `cmd/observe/main.go`) reads `site_id`,
`flag_key`, `user_id` and an optional string map `context` used for targeting,
and returns `{enabled, variant?}`. There is no bootstrap/`/decide`-style call
that returns all flags at once, so the `isFeatureEnabled` shim below only works
for flags you have already evaluated by key.

### Drop-in shim

```ts
import { init, track, identify, reset } from "@teploy/observe-browser";

init({ endpoint: "https://observe.example.com" });

const flagCache: Record<string, boolean> = {};
async function evalFlag(key: string, userId: string) {
  const r = await fetch("/api/v1/flags/evaluate", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ site_id: "default", flag_key: key, user_id: userId }),
  });
  const j = await r.json();
  flagCache[key] = !!j.enabled;
  return j.enabled;
}

(globalThis as any).posthog = {
  init: () => {},
  capture: track,
  identify: (id: string, props?: any) => identify(id, props),
  reset,
  isFeatureEnabled: (key: string) => flagCache[key] ?? false,
  reloadFeatureFlags: (userId?: string) => {
    if (!userId) return Promise.resolve();
    // Refresh the cache for known flags
    return Promise.all(Object.keys(flagCache).map((k) => evalFlag(k, userId)));
  },
  getFeatureFlag: (key: string) => flagCache[key],
};
```

## Python

**PostHog:**
```python
import posthog
posthog.project_api_key = "phc_xxx"
posthog.capture(user_id, event="signup", properties={"plan": "pro"})
```

**Observe:**
```python
import observe_sdk as observe
observe.init(endpoint="https://observe.example.com", api_key=KEY)
# Use the event ingest endpoint directly (the Python SDK focuses on errors+logs):
import urllib.request, json
urllib.request.urlopen(urllib.request.Request(
    f"{ENDPOINT}/api/v1/events",
    data=json.dumps({
        "site_id": "default",
        "event_type": "signup",
        "properties": {"plan": "pro"},
    }).encode(),
    headers={"Content-Type": "application/json", "X-API-Key": KEY},
))
```

## Data import

PostHog can export events via its export API. The shape needs a small transform:

```bash
# PostHog event  → Observe event (per event)
# {
#   "event": "signup",              # → event_type
#   "distinct_id": "u_123",         # → session_id (or use identify)
#   "properties": {...},            # → properties (nested, NOT top-level)
#   "timestamp": "2026-04-01T..."   # → server-assigned on ingest
# }

# Custom properties must be nested under `properties` — the server reads no
# other top-level key, and everything else on the body is ignored.
jq -c '{
  site_id: "default",
  event_type: .event,
  distinct_id: .distinct_id,
  url: .properties["$current_url"],
  properties: .properties
}' posthog-export.jsonl | while read event; do
  curl -s -X POST "$OBSERVE/api/v1/events" \
    -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
    -d "$event"
done
```

Reserve `/api/v1/events/batch` (up to 100 events per call) for larger imports.

## Cohorts

Observe has cohorts (`internal/cohorts`, routes under `/api/v1/cohorts`:
list, create, preview, get, members). What they are, and are not:

- **Dynamic.** A cohort is a saved rule definition, re-evaluated on every
  request that uses it. There is no stored member table or refresh job
  (the package comment calls that a phase-2 item).
- **AND-only.** `Definition.Op` must be `"and"`; any other value, including
  `"or"`, is rejected, and rules cannot be nested.
- **Two rule types.** `event` (an event name seen at least `min_count` times
  within a window such as `7d`, `30d`, `24h`) and `property` (`=` or `!=`
  against an allow-listed event column (`isAllowedPropertyKey`), matching users who
  emitted *any* event with that value).
- **Identified users only.** Anonymous events (empty `distinct_id`) are never
  members.
- **Used as an analytics filter.** Pass `cohort_id` to the stats routes to
  filter a chart to the cohort. If the cohort cannot be resolved, the query
  logs a warning and falls back to **unfiltered** results
  (`internal/query/api.go`), so check a filtered chart really is filtered.
- **Not supported:** behavioral cohorts built from funnels or paths, static
  (uploaded) cohorts, cohort-to-cohort references, and cohorts as a flag
  targeting source.

## Flags, experiments, surveys: what is and is not client-side

- **Feature flags**: management UI and the public evaluate endpoint exist;
  there is no flag client in `sdk/browser` and no flag bootstrap payload.
- **Experiments**: assignment rides on a flag. Exposures and conversions are
  posted to `POST /api/v1/experiments/expose` (API key); analysis shows a
  frequentist p-value and a Bayesian probability-to-beat.
- **Surveys**: you can create, activate, close and read responses via the
  API/UI, and the public endpoints `GET /api/v1/surveys/active`,
  `POST /api/v1/surveys/expose` and `POST /api/v1/surveys/respond` exist. No
  on-page survey widget ships: none of the tracker scripts nor `sdk/browser`
  renders a survey. You must fetch active surveys and render and submit them
  yourself. (`observe-feedback.js` is a separate feedback widget that posts to
  `/api/v1/feedback`; it is not the survey runner.)

## What doesn't port cleanly

- **Behavioral / OR cohorts** — Observe cohorts are AND-only (see
  [Cohorts](#cohorts)); recreate OR logic as separate cohorts or use funnel
  breakdowns / journey filters.
- **PostHog Apps/Plugins** — no equivalent. Consider webhooks + integrations.
- **Data pipelines** — PostHog's destinations (BigQuery, S3) aren't built in.
  Use the `/api/v1/export` endpoint or database backups.
- **Formula insights** — derived metrics require custom dashboard queries
  (server-side aggregation, not formula-based composition).

## Checklist

- [ ] Swap init + capture / identify / flag calls.
- [ ] Re-create key dashboards on `/dashboards`.
- [ ] Recreate simple cohorts as AND-only cohort rules; use funnel breakdowns for anything else.
- [ ] If you used PostHog surveys, plan to render them yourself (no on-page widget ships).
- [ ] If you used session replays, add `observe-replay.js` to your tag setup.
- [ ] Delete the PostHog project and API key from env + secrets.
