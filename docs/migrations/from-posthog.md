# Migrating from PostHog

Observe covers the analytics + feature flags + session replay surface that
makes up PostHog's core. This guide walks through the concept mapping and SDK
swap.

> **TL;DR** — Observe's event model and flag evaluation are simpler than
> PostHog's (fewer concepts, no Kafka/Clickhouse) but cover the 80% path.
> You lose cohort-as-query and retention-by-SQL; you gain a single binary.

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
| Cohort                           | (no direct equivalent — use funnel breakdown)    |
| Plugin / app                     | Integration (`/integrations`)                    |

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

// Flag evaluation is a server call:
const r = await fetch("/api/v1/flags/evaluate", {
  method: "POST",
  headers: { "Content-Type": "application/json", "X-API-Key": OBSERVE_API_KEY },
  body: JSON.stringify({ site_id: "default", flag_key: "new-checkout", user_id: user.id }),
});
const { enabled, variant } = await r.json();
```

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

## What doesn't port cleanly

- **Cohort-as-query** — Observe doesn't have a separate cohort concept;
  use funnel breakdowns or journey filters instead.
- **PostHog Apps/Plugins** — no equivalent. Consider webhooks + integrations.
- **Data pipelines** — PostHog's destinations (BigQuery, S3) aren't built in.
  Use the `/api/v1/export` endpoint or database backups.
- **Formula insights** — derived metrics require custom dashboard queries
  (server-side aggregation, not formula-based composition).

## Checklist

- [ ] Swap init + capture / identify / flag calls.
- [ ] Re-create key dashboards on `/dashboards`.
- [ ] Migrate cohorts to equivalent funnels with breakdowns.
- [ ] If you used session replays, add `observe-replay.js` to your tag setup.
- [ ] Delete the PostHog project and API key from env + secrets.
