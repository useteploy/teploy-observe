import { useState, useEffect } from "preact/hooks";
import "../styles/docs.css";

export const config = { mode: "app" };

interface DocPage {
  slug: string;
  title: string;
  content: string;
}

interface DocSection {
  title: string;
  pages: DocPage[];
}

const DOCS: DocSection[] = [
  {
    title: "Getting Started",
    pages: [
      {
        slug: "intro",
        title: "Introduction",
        content: `# What is Observe?

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

Solo developers and small teams who want one platform instead of paying for Sentry + Umami + LaunchDarkly + Statuspage + Grafana. Self-hosted, free forever, no event caps.`,
      },
      {
        slug: "quickstart",
        title: "Quick Start",
        content: `# Quick Start

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
  data-site-id="YOUR_SITE_ID"></script>
\`\`\`

That's it. Open your site, visit a few pages, check the **Dashboard**. Data appears within seconds.`,
      },
      {
        slug: "self-host",
        title: "Self-Hosting",
        content: `# Self-Hosting

## Environment variables

| Variable | Default | Description |
|---|---|---|
| \`OBSERVE_ADDR\` | \`:3000\` | HTTP listen address |
| \`OBSERVE_NUCLEUS_URL\` | (required) | Nucleus pgwire URL |
| \`OBSERVE_JWT_SECRET\` | (random) | JWT signing key |
| \`OBSERVE_ADMIN_USER\` | \`admin\` | Initial admin username |
| \`OBSERVE_ADMIN_PASSWORD\` | (required) | Initial admin password |
| \`OBSERVE_SESSION_SALT\` | (required) | Salt for cookie-free session hashing |
| \`OBSERVE_BUFFER_SIZE\` | \`100000\` | Event buffer capacity |
| \`OBSERVE_FLUSH_INTERVAL_MS\` | \`2000\` | Buffer flush interval |
| \`OBSERVE_FLUSH_SIZE\` | \`500\` | Batch size per flush |
| \`OBSERVE_RAW_RETENTION_DAYS\` | \`30\` | Raw events retention |
| \`OBSERVE_HOURLY_RETENTION_DAYS\` | \`365\` | Hourly rollup retention |
| \`OBSERVE_RATE_LIMIT\` | \`100\` | Ingest rate limit (events/sec per IP) |

## Performance

On commodity hardware (8GB VPS):
- **Ingestion**: 5,000 events/sec sustained, 10k burst
- **Query latency**: <200ms p99 for last-24h queries
- **Memory**: <500MB under load
- **Binary size**: <30MB
`,
      },
    ],
  },
  {
    title: "Tracker SDK",
    pages: [
      {
        slug: "tracker",
        title: "Browser Tracker",
        content: `# Browser Tracker (observe.js)

## Install

\`\`\`html
<script defer src="/t/observe.js" data-site-id="SITE_ID"></script>
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
`,
      },
      {
        slug: "errors-sdk",
        title: "Error Tracking SDK",
        content: `# Error Tracking SDK (observe-errors.js)

## Install

\`\`\`html
<script defer src="/t/observe-errors.js" data-site-id="SITE_ID"></script>
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
  data-release="v1.2.3"></script>
\`\`\`
`,
      },
      {
        slug: "replay-sdk",
        title: "Session Replay SDK",
        content: `# Session Replay (observe-replay.js)

## Install

\`\`\`html
<script defer src="/t/observe-replay.js" data-site-id="SITE_ID"></script>
\`\`\`

## What it captures

- DOM snapshots on page load
- Mouse movements, clicks, scroll position
- DOM mutations (input changes, new elements)
- JS errors linked to the session

Privacy-first: passwords and \`data-private\` elements are masked.

## View replays

Go to **Sessions** in the dashboard. Click any session to see the event timeline and page journey.
`,
      },
    ],
  },
  {
    title: "Features",
    pages: [
      {
        slug: "analytics",
        title: "Analytics",
        content: `# Analytics

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
`,
      },
      {
        slug: "errors",
        title: "Error Tracking",
        content: `# Error Tracking

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
`,
      },
      {
        slug: "traces",
        title: "APM / Tracing",
        content: `# APM / Distributed Tracing

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
`,
      },
      {
        slug: "flags",
        title: "Feature Flags",
        content: `# Feature Flags

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
`,
      },
      {
        slug: "experiments",
        title: "Experiments",
        content: `# Experiments

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
`,
      },
      {
        slug: "alerts",
        title: "Alerts",
        content: `# Alerts

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
`,
      },
    ],
  },
  {
    title: "Reference",
    pages: [
      {
        slug: "api",
        title: "API Reference",
        content: `# API Reference

All API endpoints are at \`/api/v1/\`. Auth via JWT (dashboard) or API key (ingest).

## Ingestion

- \`POST /api/v1/events\` — single event
- \`POST /api/v1/events/batch\` — batch events
- \`POST /api/v1/errors\` — error event
- \`POST /api/v1/logs\` — log entry
- \`POST /api/v1/replays\` — replay session
- \`POST /v1/traces\` — OTLP traces

## Query

- \`GET /api/v1/stats/overview\` — pageviews, visitors, sessions
- \`GET /api/v1/stats/timeseries\` — time-series data
- \`GET /api/v1/stats/pages\` — top pages
- \`GET /api/v1/stats/funnel\` — funnel analysis
- \`GET /api/v1/stats/retention\` — cohort retention
- \`GET /api/v1/stats/journeys\` — user journeys
- \`GET /api/v1/stats/correlations\` — property correlations

## Platform

- \`GET/POST /api/v1/sites\` — site management
- \`GET/POST/DELETE /api/v1/platform/alerts/rules\` — alert rules
- \`GET/POST/DELETE /api/v1/platform/webhooks\` — webhooks
- \`GET/POST /api/v1/platform/users\` — user management
- \`POST /api/v1/auth/login\` — login
- \`POST /api/v1/auth/password\` — password change

## Feature flags

- \`GET/POST /api/v1/flags\` — CRUD
- \`POST /api/v1/flags/evaluate\` — evaluate for a user

## OpenAPI

Full spec at \`/api/v1/openapi.json\`.
`,
      },
      {
        slug: "healthcheck",
        title: "Health Check",
        content: `# Health Check

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
`,
      },
    ],
  },
];

// Minimal markdown renderer — headers, bold, code blocks, lists, tables
function renderMarkdown(md: string): string {
  let html = md;

  // Escape HTML
  html = html.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");

  // Code blocks ```lang\n...\n```
  html = html.replace(/```(\w*)\n([\s\S]*?)```/g, (_m, lang, code) => {
    return `<pre class="docs-code"><code>${code.replace(/\n$/, "")}</code></pre>`;
  });

  // Inline code
  html = html.replace(/`([^`]+)`/g, '<code class="docs-inline-code">$1</code>');

  // Headers
  html = html.replace(/^###### (.*$)/gm, "<h6>$1</h6>");
  html = html.replace(/^##### (.*$)/gm, "<h5>$1</h5>");
  html = html.replace(/^#### (.*$)/gm, "<h4>$1</h4>");
  html = html.replace(/^### (.*$)/gm, "<h3>$1</h3>");
  html = html.replace(/^## (.*$)/gm, "<h2>$1</h2>");
  html = html.replace(/^# (.*$)/gm, "<h1>$1</h1>");

  // Bold
  html = html.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");

  // Tables (simple pipe-separated)
  html = html.replace(/(\|[^\n]+\|\n\|[-:\s|]+\|\n(?:\|[^\n]+\|\n?)+)/g, (match) => {
    const rows = match.trim().split("\n");
    const headers = rows[0].split("|").slice(1, -1).map((h) => h.trim());
    const body = rows.slice(2).map((r) => r.split("|").slice(1, -1).map((c) => c.trim()));
    let t = `<table class="docs-table"><thead><tr>${headers.map((h) => `<th>${h}</th>`).join("")}</tr></thead><tbody>`;
    for (const row of body) {
      t += `<tr>${row.map((c) => `<td>${c}</td>`).join("")}</tr>`;
    }
    t += "</tbody></table>";
    return t;
  });

  // Unordered lists
  html = html.replace(/((?:^- .+\n?)+)/gm, (match) => {
    const items = match.trim().split("\n").map((line) => line.replace(/^- /, "").trim());
    return "<ul>" + items.map((i) => `<li>${i}</li>`).join("") + "</ul>";
  });

  // Ordered lists
  html = html.replace(/((?:^\d+\. .+\n?)+)/gm, (match) => {
    const items = match.trim().split("\n").map((line) => line.replace(/^\d+\. /, "").trim());
    return "<ol>" + items.map((i) => `<li>${i}</li>`).join("") + "</ol>";
  });

  // Paragraphs (double newline)
  const parts = html.split(/\n\n+/);
  html = parts.map((p) => {
    const t = p.trim();
    if (!t) return "";
    if (t.startsWith("<h") || t.startsWith("<pre") || t.startsWith("<table") || t.startsWith("<ul") || t.startsWith("<ol")) {
      return t;
    }
    return `<p>${t.replace(/\n/g, "<br>")}</p>`;
  }).join("\n");

  return html;
}

export default function DocsPage() {
  const [activeSlug, setActiveSlug] = useState<string>("intro");

  useEffect(() => {
    const hash = typeof window !== "undefined" ? window.location.hash.slice(1) : "";
    if (hash) setActiveSlug(hash);
  }, []);

  const allPages = DOCS.flatMap(s => s.pages);
  const current = allPages.find((p) => p.slug === activeSlug) || allPages[0];

  const handleNav = (slug: string) => {
    setActiveSlug(slug);
    if (typeof window !== "undefined") {
      window.location.hash = slug;
      window.scrollTo(0, 0);
    }
  };

  return (
    <div class="docs-layout">
      <aside class="docs-sidebar">
        {DOCS.map((section) => (
          <div key={section.title} class="docs-section">
            <div class="docs-section-title">{section.title}</div>
            {section.pages.map((page) => (
              <button key={page.slug}
                class={`docs-nav-item ${activeSlug === page.slug ? "docs-nav-item--active" : ""}`}
                onClick={() => handleNav(page.slug)}>
                {page.title}
              </button>
            ))}
          </div>
        ))}
      </aside>

      <main class="docs-content">
        <article dangerouslySetInnerHTML={{ __html: renderMarkdown(current.content) }} />
      </main>
    </div>
  );
}
