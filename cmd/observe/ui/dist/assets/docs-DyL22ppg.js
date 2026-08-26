import{u as i}from"./jsxRuntime-CTOXgF2p.js";import{h as d,y as h}from"./hooks-CuL87h67.js";import"./index-D5p0bVPt.js";const S={mode:"app"},u=[{title:"Getting Started",pages:[{slug:"intro",title:"Introduction",content:`# What is Observe?

Observe is a single-binary, all-in-one observability platform that replaces 5-10 tools with one:

- **Web analytics** — pageviews, sessions, visitors, bounce rate, funnels, retention
- **Error tracking** — stack traces, grouping, release health, source maps
- **APM / Distributed tracing** — OTLP ingestion, service maps, RED metrics
- **Logs** — search, filter, correlate with traces
- **Feature flags** — targeting, multivariate variants, evaluation
- **Experiments** — A/B testing with statistical significance
- **Session replay** — pageview journeys, click/scroll events
- **Uptime monitoring** — HTTP probes, cron heartbeats
- **LLM observability** — token usage, cost tracking, latency
- **Alerts + integrations** — Slack, email, PagerDuty, webhooks

## Architecture

Two processes, one database. Observe runs as a Go binary talking to a Nucleus database. That's it. No ClickHouse, no Redis, no Kafka, no ZooKeeper.

## Who it's for

Solo developers and small teams who want one platform instead of paying for Sentry + Umami + LaunchDarkly + Statuspage + Grafana. Self-hosted, free forever, no event caps.`},{slug:"quickstart",title:"Quick Start",content:`# Quick Start

## 1. Run with Docker Compose

\`\`\`yaml
services:
  nucleus:
    image: ghcr.io/neutron-build/nucleus:latest
    ports: ["5432:5432"]
    volumes: ["nucleus_data:/data"]

  observe:
    image: ghcr.io/teploy/observe:latest
    ports: ["3000:3000"]
    environment:
      OBSERVE_NUCLEUS_URL: "postgres://nucleus:5432/observe"
      OBSERVE_JWT_SECRET: "change-me-in-production"
      OBSERVE_ADMIN_USER: "admin"
      OBSERVE_ADMIN_PASSWORD: "observe"
    depends_on: [nucleus]

volumes:
  nucleus_data:
\`\`\`

Run:

\`\`\`bash
docker-compose up
\`\`\`

Open **http://localhost:3000**, log in with admin / observe.

## 2. Create a site and API key

Go to **Settings > Sites**, click "Add Site", then "API Key" to generate a key.

## 3. Install the tracker

Add this one line to your HTML:

\`\`\`html
<script defer src="https://observe.example.com/t/observe.js"
  data-site-id="YOUR_SITE_ID"><\/script>
\`\`\`

That's it. Open your site, visit a few pages, check the **Dashboard**. Data appears within seconds.`},{slug:"self-host",title:"Self-Hosting",content:"# Self-Hosting\n\n## Environment variables\n\n| Variable | Default | Description |\n|---|---|---|\n| `OBSERVE_ADDR` | `:3000` | HTTP listen address |\n| `OBSERVE_NUCLEUS_URL` | (required) | Nucleus pgwire URL |\n| `OBSERVE_JWT_SECRET` | (random) | JWT signing key |\n| `OBSERVE_ADMIN_USER` | `admin` | Initial admin username |\n| `OBSERVE_ADMIN_PASSWORD` | (required) | Initial admin password |\n| `OBSERVE_SESSION_SALT` | (required) | Salt for cookie-free session hashing |\n| `OBSERVE_BUFFER_SIZE` | `100000` | Event buffer capacity |\n| `OBSERVE_FLUSH_INTERVAL_MS` | `2000` | Buffer flush interval |\n| `OBSERVE_FLUSH_SIZE` | `500` | Batch size per flush |\n| `OBSERVE_RAW_RETENTION_DAYS` | `30` | Raw events retention |\n| `OBSERVE_HOURLY_RETENTION_DAYS` | `365` | Hourly rollup retention |\n| `OBSERVE_RATE_LIMIT` | `100` | Ingest rate limit (events/sec per IP) |\n\n## Performance\n\nOn commodity hardware (8GB VPS):\n- **Ingestion**: 5,000 events/sec sustained, 10k burst\n- **Query latency**: <200ms p99 for last-24h queries\n- **Memory**: <500MB under load\n- **Binary size**: <30MB\n"}]},{title:"Tracker SDK",pages:[{slug:"tracker",title:"Browser Tracker",content:`# Browser Tracker (observe.js)

## Install

\`\`\`html
<script defer src="/t/observe.js" data-site-id="SITE_ID"><\/script>
\`\`\`

## Auto-tracked events

By default the tracker captures:

- **Pageviews** — initial load + SPA navigation (history.pushState)
- **Clicks** — on links, buttons, inputs, role=button
- **Form submissions** — with form id + action
- **Rage clicks** — 3+ clicks on same element within 1 second
- **Outbound clicks** — links to external domains
- **Web vitals** — LCP, FID, CLS, TTFB (when trackVitals is called)

## Manual events

\`\`\`js
window.observe.track('signup_completed', {
  plan: 'pro',
  source: 'landing'
});
\`\`\`

## Revenue tracking

\`\`\`js
window.observe.revenue(99, 'USD', { plan: 'pro' });
\`\`\`

## Web vitals

\`\`\`js
window.observe.trackVitals();
\`\`\`

## Configuration

| Attribute | Description |
|---|---|
| \`data-site-id\` | Required site ID |
| \`data-auto-track="false"\` | Disable automatic pageview tracking |
| \`data-autocapture="false"\` | Disable click/form autocapture |
| \`data-respect-dnt="false"\` | Ignore Do Not Track header |
| \`data-endpoint\` | Override ingest endpoint |

## Privacy

- No cookies, no localStorage for tracking
- Session IDs are derived server-side from IP + UA hash with monthly salt rotation
- Raw IPs never stored
- DNT respected by default
`},{slug:"errors-sdk",title:"Error Tracking SDK",content:`# Error Tracking SDK (observe-errors.js)

## Install

\`\`\`html
<script defer src="/t/observe-errors.js" data-site-id="SITE_ID"><\/script>
\`\`\`

## What it captures

- \`window.onerror\` — synchronous errors
- \`unhandledrejection\` — promise rejections
- Breadcrumbs — recent console, fetch, click, navigation events

## Manual capture

\`\`\`js
window.observeErrors.capture(new Error('payment failed'), {
  tags: { release: 'v1.2.3' },
  extra: { orderId: 'abc123' }
});
\`\`\`

## Release tracking

\`\`\`html
<script src="/t/observe-errors.js"
  data-site-id="SITE_ID"
  data-release="v1.2.3"><\/script>
\`\`\`
`},{slug:"replay-sdk",title:"Session Replay SDK",content:`# Session Replay (observe-replay.js)

## Install

\`\`\`html
<script defer src="/t/observe-replay.js" data-site-id="SITE_ID"><\/script>
\`\`\`

## What it captures

- DOM snapshots on page load
- Mouse movements, clicks, scroll position
- DOM mutations (input changes, new elements)
- JS errors linked to the session

Privacy-first: passwords and \`data-private\` elements are masked.

## View replays

Go to **Sessions** in the dashboard. Click any session to see the event timeline and page journey.
`}]},{title:"Features",pages:[{slug:"analytics",title:"Analytics",content:`# Analytics

## What's tracked

Cookie-free, GDPR-compliant analytics:
- Pageviews, visitors, sessions
- Bounce rate, average visit duration
- Top pages, referrers, entry/exit pages
- Browser, OS, device, country, language
- UTM source/medium/campaign/term/content
- Custom events with properties

## Dashboard

The main dashboard shows:
- **Stat cards** — active now, pageviews, visitors, sessions, bounce rate, duration
- **Time-series chart** — pageviews + visitors over time with hover tooltips
- **Breakdowns** — pages, sources, tech, location
- **World map** — countries by visitor count
- **Custom events panel** — event type volume with property drill-down

## Filters

Click any row in a breakdown panel to filter. Filters compose — click a country, then a browser, then a referrer.

## Date ranges

Use the date picker top-right. Supports:
- Today, Yesterday, Last 7/30/90 days
- Custom range
- Compare mode — shows % change vs previous period
`},{slug:"errors",title:"Error Tracking",content:`# Error Tracking

## Grouping

Errors are grouped by a hash of:
1. Error type (TypeError, ReferenceError)
2. In-app stack frames (excluding vendor code)
3. Parameterized message (UUIDs, numbers, URLs replaced with placeholders)

This means "Cannot read property 'id' of undefined" and "Cannot read property 'name' of undefined" group together if they occur in the same code path.

## Issue list

Each issue shows:
- Status (open/resolved/ignored)
- Title + culprit file
- 14-day activity indicator
- Event count
- Last seen timestamp

## Issue detail

- Stack trace viewer with in-app frame highlighting
- Breadcrumb timeline (console, fetch, clicks, navigation)
- Multiple error events (click to inspect each)
- Browser/OS/device/URL/environment context
- Link to correlated session

## Full-text search

Uses BM25 ranking over error messages. Search for "timeout" and you get relevance-ranked results, not just substring matches.
`},{slug:"traces",title:"APM / Tracing",content:`# APM / Distributed Tracing

## Ingestion

Observe accepts OTLP HTTP/JSON at \`/v1/traces\`. Configure your OpenTelemetry SDK:

\`\`\`
OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://observe.example.com/v1/traces
OTEL_EXPORTER_OTLP_PROTOCOL=http/json
\`\`\`

## Service list

Shows all services emitting traces with RED metrics:
- **R**ate — requests per second
- **E**rrors — error rate %
- **D**uration — p50/p95/p99 latency

## Trace waterfall

Click a trace to see:
- Hierarchical span tree with depth indentation
- Bars colored by duration percentile (green < p50, yellow p50-p95, orange > p95, red on error)
- Attributes, resources, events per span (JSONB)
- Correlated errors — errors that occurred during the trace

## Dependency graph

SVG visualization of service-to-service calls. Edge width shows call volume. Red edges indicate errors.
`},{slug:"flags",title:"Feature Flags",content:`# Feature Flags

## Create a flag

**Flags > Create Flag**:
- Flag key (unique, used in code)
- Flag type: boolean or multivariate
- Rollout percentage (0-100)
- Targeting rules (attribute-based)
- Variants (for multivariate)

## Evaluation

\`\`\`
POST /api/v1/flags/evaluate
{
  "site_id": "SITE",
  "flag_key": "new_checkout",
  "user_id": "user123",
  "context": { "country": "US", "plan": "pro" }
}
\`\`\`

Response:
\`\`\`json
{ "enabled": true, "variant": "treatment_b" }
\`\`\`

## Targeting

Rules support operators: \`eq\`, \`neq\`, \`in\`, \`not_in\`, \`contains\`.

Example: Enable for pro users in US/CA:
- \`country in [US, CA]\`
- \`plan eq pro\`

## Deterministic rollout

Hashing is based on flag_key + user_id (SHA-256). Same user always gets the same variant across evaluations.
`},{slug:"experiments",title:"Experiments",content:`# Experiments

## A/B test setup

Create an experiment linked to a feature flag. Observe handles:
- Exposure tracking (who saw which variant)
- Conversion tracking (who completed the goal)
- Statistical significance (chi-squared test)

## Variant assignment

Variants are assigned deterministically via the linked feature flag. Call:

\`\`\`js
const result = await observe.flags.evaluate('checkout_test', userId);
if (result.variant === 'treatment') { ... }

// Record exposure (automatic on evaluate)
// Record conversion when goal is hit:
observe.experiments.convert('checkout_test', userId);
\`\`\`

## Results

The experiment detail page shows:
- Exposures and conversions per variant
- Conversion rate with comparison to control
- Statistical significance (chi-squared, p<0.05)
- Winner declaration when significant
`},{slug:"alerts",title:"Alerts",content:`# Alerts

## Rules

Create rules in **Alerts > Create Rule**:
- **Metric**: error_count, error_rate, pageviews, visitors
- **Operator**: >, >=, <, <=, =
- **Threshold**: numeric value
- **Window**: minutes over which to evaluate (default 5)
- **Cooldown**: minutes before re-triggering (default 5)

## Integrations

Route alerts to:
- **Slack** — webhook URL
- **Email** — SMTP config
- **Jira** — create issue in project
- **GitHub** — create issue in repo
- **PagerDuty** — routing key
- **Webhook** — any HTTP endpoint

## Example rule

"Error count > 10 in 5 minutes, cooldown 30 minutes"

When triggered:
1. Row added to alert history
2. Payload sent to all configured integrations
3. Cooldown prevents re-firing for 30 minutes
`}]},{title:"Reference",pages:[{slug:"api",title:"API Reference",content:"# API Reference\n\nAll API endpoints are at `/api/v1/`. Auth via JWT (dashboard) or API key (ingest).\n\n## Ingestion\n\n- `POST /api/v1/events` — single event\n- `POST /api/v1/events/batch` — batch events\n- `POST /api/v1/errors` — error event\n- `POST /api/v1/logs` — log entry\n- `POST /api/v1/replays` — replay session\n- `POST /v1/traces` — OTLP traces\n\n## Query\n\n- `GET /api/v1/stats/overview` — pageviews, visitors, sessions\n- `GET /api/v1/stats/timeseries` — time-series data\n- `GET /api/v1/stats/pages` — top pages\n- `GET /api/v1/stats/funnel` — funnel analysis\n- `GET /api/v1/stats/retention` — cohort retention\n- `GET /api/v1/stats/journeys` — user journeys\n- `GET /api/v1/stats/correlations` — property correlations\n\n## Platform\n\n- `GET/POST /api/v1/sites` — site management\n- `GET/POST/DELETE /api/v1/platform/alerts/rules` — alert rules\n- `GET/POST/DELETE /api/v1/platform/webhooks` — webhooks\n- `GET/POST /api/v1/platform/users` — user management\n- `POST /api/v1/auth/login` — login\n- `POST /api/v1/auth/password` — password change\n\n## Feature flags\n\n- `GET/POST /api/v1/flags` — CRUD\n- `POST /api/v1/flags/evaluate` — evaluate for a user\n\n## OpenAPI\n\nFull spec at `/api/v1/openapi.json`.\n"},{slug:"healthcheck",title:"Health Check",content:`# Health Check

\`GET /healthz\` returns:

\`\`\`json
{ "status": "ok" }
\`\`\`

or on failure:

\`\`\`json
{ "status": "error", "error": "..." }
\`\`\`

Returns HTTP 200 when Nucleus connectivity is healthy, 503 otherwise.

Used by Docker healthcheck in the default docker-compose.yml.
`}]}];function g(o){let e=o;return e=e.replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;"),e=e.replace(/```(\w*)\n([\s\S]*?)```/g,(s,r,t)=>`<pre class="docs-code"><code>${t.replace(/\n$/,"")}</code></pre>`),e=e.replace(/`([^`]+)`/g,'<code class="docs-inline-code">$1</code>'),e=e.replace(/^###### (.*$)/gm,"<h6>$1</h6>"),e=e.replace(/^##### (.*$)/gm,"<h5>$1</h5>"),e=e.replace(/^#### (.*$)/gm,"<h4>$1</h4>"),e=e.replace(/^### (.*$)/gm,"<h3>$1</h3>"),e=e.replace(/^## (.*$)/gm,"<h2>$1</h2>"),e=e.replace(/^# (.*$)/gm,"<h1>$1</h1>"),e=e.replace(/\*\*([^*]+)\*\*/g,"<strong>$1</strong>"),e=e.replace(/(\|[^\n]+\|\n\|[-:\s|]+\|\n(?:\|[^\n]+\|\n?)+)/g,s=>{const r=s.trim().split(`
`),t=r[0].split("|").slice(1,-1).map(a=>a.trim()),n=r.slice(2).map(a=>a.split("|").slice(1,-1).map(p=>p.trim()));let c=`<table class="docs-table"><thead><tr>${t.map(a=>`<th>${a}</th>`).join("")}</tr></thead><tbody>`;for(const a of n)c+=`<tr>${a.map(p=>`<td>${p}</td>`).join("")}</tr>`;return c+="</tbody></table>",c}),e=e.replace(/((?:^- .+\n?)+)/gm,s=>"<ul>"+s.trim().split(`
`).map(t=>t.replace(/^- /,"").trim()).map(t=>`<li>${t}</li>`).join("")+"</ul>"),e=e.replace(/((?:^\d+\. .+\n?)+)/gm,s=>"<ol>"+s.trim().split(`
`).map(t=>t.replace(/^\d+\. /,"").trim()).map(t=>`<li>${t}</li>`).join("")+"</ol>"),e=e.split(/\n\n+/).map(s=>{const r=s.trim();return r?r.startsWith("<h")||r.startsWith("<pre")||r.startsWith("<table")||r.startsWith("<ul")||r.startsWith("<ol")?r:`<p>${r.replace(/\n/g,"<br>")}</p>`:""}).join(`
`),e}function E(){const[o,e]=d("intro");h(()=>{const t=typeof window<"u"?window.location.hash.slice(1):"";t&&e(t)},[]);const l=u.flatMap(t=>t.pages),s=l.find(t=>t.slug===o)||l[0],r=t=>{e(t),typeof window<"u"&&(window.location.hash=t,window.scrollTo(0,0))};return i("div",{class:"docs-layout",children:[i("aside",{class:"docs-sidebar",children:u.map(t=>i("div",{class:"docs-section",children:[i("div",{class:"docs-section-title",children:t.title}),t.pages.map(n=>i("button",{class:`docs-nav-item ${o===n.slug?"docs-nav-item--active":""}`,onClick:()=>r(n.slug),children:n.title},n.slug))]},t.title))}),i("main",{class:"docs-content",children:i("article",{dangerouslySetInnerHTML:{__html:g(s.content)}})})]})}export{S as config,E as default};
