# Umami v3.0.3 -- Comprehensive Research Document

**Date:** 2026-03-20
**Source:** https://github.com/umami-software/umami (commit analyzed from local clone)
**Purpose:** Competitor analysis for Teploy Observe

---

## 1. Tech Stack

### Languages and Runtime

- **Language:** TypeScript (frontend + backend), plain JavaScript (tracker script)
- **Runtime:** Node.js 22 (Alpine-based Docker image)
- **Module system:** ESM (`"type": "module"` in package.json)

### Frameworks

- **Full-stack:** Next.js 15.5.9 (App Router) with `output: 'standalone'`
- **React:** 19.2.3
- **Build tooling:** Turbo (Next.js dev + build), Rollup (tracker script), tsup (component library export)

### Database(s)

- **Primary (self-hosted):** PostgreSQL v12.14+ via Prisma ORM
- **High-scale option:** ClickHouse (optional, enabled via `CLICKHOUSE_URL` env var)
- **Message queue:** Kafka (optional, enabled via `KAFKA_URL` + `KAFKA_BROKER`), used to buffer writes into ClickHouse

Every query module exports a `runQuery()` dispatch that selects the database adapter at runtime:

```typescript
// src/lib/db.ts
export async function runQuery(queries: any) {
  if (process.env.CLICKHOUSE_URL) {
    if (queries[KAFKA]) return queries[KAFKA]();
    return queries[CLICKHOUSE]();
  }
  const db = getDatabaseType();
  if (db === POSTGRESQL) return queries[PRISMA]();
}
```

This means every query is written twice: once as raw PostgreSQL SQL executed through Prisma's `$queryRawUnsafe`, and once as ClickHouse SQL. There are no shared query abstractions -- each is hand-tuned for its engine.

### ORM / Query Layer

- **Prisma 6.18** with the `@prisma/adapter-pg` driver adapter (not the Prisma engine binary)
- Prisma is used for CRUD operations on metadata tables (User, Website, Team, Report, etc.)
- All analytics queries (events, pageviews, sessions, metrics) use **raw SQL** via `prisma.rawQuery()` -- a custom wrapper around `$queryRawUnsafe`
- Supports **read replicas** via `@prisma/extension-read-replicas` when `DATABASE_REPLICA_URL` is set
- Custom parameterized query syntax `{{paramName::type}}` that gets rewritten to `$1::type` positional params

### Frontend

- **UI framework:** React 19 + Next.js App Router (server components + client components)
- **Component library:** `@umami/react-zen` (proprietary Umami component library)
- **State management:** Zustand 5
- **Data fetching:** TanStack React Query 5
- **Charts:** Chart.js 4.5.1 with `chartjs-adapter-date-fns`
- **Maps:** react-simple-maps
- **i18n:** react-intl (FormatJS) -- 52 language files
- **Styling:** CSS Modules + PostCSS
- **Drag and drop:** @hello-pangea/dnd
- **Icons:** lucide-react + custom SVGs (generated via @svgr/cli)
- **Avatars:** DiceBear

### Caching

- **Redis:** Optional (`@umami/redis-client`) -- caches website lookups, session lookups, pixel/link lookups, and auth tokens
- **TTL:** 86400 seconds (24 hours) for website/session/pixel/link caches
- Without Redis, every request hits the database

### Build and Deployment

- **Package manager:** pnpm (with pnpm-workspace.yaml)
- **Build chain:** `build-db` -> `build-tracker` -> `build-geo` -> `build-app`
- **Docker:** Multi-stage build (deps -> builder -> runner), Node 22 Alpine
- **GeoIP:** MaxMind GeoLite2-City.mmdb, downloaded at build time
- **Standalone output:** Next.js `output: 'standalone'` for minimal Docker images
- **Health check:** `GET /api/heartbeat`

---

## 2. Core Features (Exhaustive)

### Analytics -- Dashboard (Mature)

- **Website stats overview:** Pageviews, unique visitors, visits, bounce rate, average visit time
- **Comparison period:** Automatic previous-period comparison on stats
- **Time-series charts:** Pageviews + sessions over time, configurable units (minute, hour, day, month, year)
- **Date range selector:** 24h default, custom ranges
- **Timezone support:** Per-user timezone preference

### Analytics -- Metrics (Mature)

- **Pages:** Top pages by unique visitors
- **Entry pages:** First page of each visit
- **Exit pages:** Last page of each visit
- **Referrers:** External referrer domains (excludes self-referral)
- **Browsers:** Browser breakdown
- **Operating systems:** OS breakdown
- **Devices:** Device type (desktop, laptop, tablet, mobile)
- **Screen sizes:** Resolution breakdown
- **Languages:** Language breakdown (truncated to 2-char code)
- **Countries/Regions/Cities:** Geo breakdown with map visualization
- **Channels:** Traffic channel classification (Direct, Organic Search, Paid Search, Social, Email, Shopping, Video, Referral, Affiliate, SMS, Paid Ads)
- **Hostnames:** Multi-domain tracking support

### Analytics -- Events (Mature)

- **Custom events:** Named events with arbitrary JSON data
- **Event data properties:** Key-value pairs with typed storage (string, number, boolean, date, array)
- **Event data stats:** Aggregated counts per event
- **Event time series:** Event counts over time

### Analytics -- Sessions (Mature)

- **Session list:** Paginated session browser with filters
- **Session detail:** Individual session activity timeline
- **Session properties:** Custom session data via `identify()` API
- **Session stats:** Weekly traffic patterns (day-of-week x hour-of-day)
- **Distinct ID support:** Link sessions to user identities

### Reports (Mature)

- **Funnels:** Multi-step conversion funnel with URL/event steps, wildcard matching, configurable time window
- **Retention:** Cohort retention analysis (daily cohorts, up to 31 days)
- **Journeys:** User flow visualization (up to 7 steps per visit), configurable start/end steps
- **Goals:** Conversion goal tracking against total traffic
- **Revenue:** Revenue tracking per event/currency, with time-series and country breakdown
- **Attribution:** First-click and last-click attribution models across referrers, UTM params, and paid ad click IDs
- **UTM analysis:** Breakdown by utm_source, utm_medium, utm_campaign, utm_content, utm_term
- **Breakdown:** Multi-dimensional breakdown with views, visitors, visits, bounces, total time

### Real-time (Mature)

- **Active visitors:** Count of distinct sessions in last 5 minutes
- **Real-time activity feed:** Live event stream with session/pageview/event classification
- **Real-time metrics:** Countries, URLs, referrers updating in real-time
- **30-second polling interval**

### Tracking -- Links (Mature)

- **Tracked links:** Create short links (`/q/[slug]`) that redirect and record analytics
- **Link metrics:** Same analytics pipeline as websites

### Tracking -- Pixels (Mature)

- **Tracking pixels:** Create 1x1 GIF pixel endpoints (`/p/[slug]`) for email/non-JS tracking
- **Pixel metrics:** Same analytics pipeline as websites

### Segments and Cohorts (Mature)

- **Segments:** Save filter combinations as reusable segments
- **Cohorts:** Define cohort criteria with date range + filters + action, apply to any query

### Sharing (Mature)

- **Share links:** Generate unique share IDs for public read-only dashboard access
- **Share token auth:** JWT-based anonymous access to shared dashboards

### Boards (Newer Feature)

- **Custom dashboards:** Create custom dashboard boards (limited UI found, appears relatively new)

### Admin (Mature)

- **User management:** CRUD users, assign roles
- **Team management:** Teams with role-based access (owner, manager, member, view-only)
- **Website management:** Admin view of all websites
- **Password change utility:** CLI script for password resets

### Settings (Mature)

- **Profile:** Username, display name, logo, password change
- **Preferences:** Language, timezone, date range default, theme (light/dark)
- **Website settings:** Edit name/domain, tracking code snippet, share link, data reset, website transfer, website deletion

### Data Management

- **Data export:** ZIP file containing CSVs (events, pages, referrers, browsers, OS, devices, countries)
- **Data reset:** Reset website data from a given date
- **Soft delete:** Users, teams, websites, links, pixels all use `deletedAt` for soft deletion

### API

- **REST API:** Full CRUD + analytics query API
- **Batch endpoint:** `POST /api/batch` for sending multiple events in one request
- **Config endpoint:** Public config for UI initialization
- **Telemetry:** Self-telemetry script at `/telemetry.js`

### Authentication

- **Username/password:** bcrypt-hashed passwords
- **JWT tokens:** Encrypted (AES-256-GCM) JWT tokens for auth
- **Redis session store:** Optional Redis-backed auth with expiring keys
- **SSO:** Token refresh endpoint for single sign-on integration
- **Roles:** admin, user, view-only

### Privacy

- **Do Not Track respect:** Configurable DNT header checking
- **Bot detection:** isbot library for filtering automated traffic
- **IP blocking:** IGNORE_IP env var with CIDR support
- **Domain locking:** data-domains attribute limits tracking to specific domains
- **Local storage opt-out:** `umami.disabled` localStorage key
- **Tracker `beforeSend` hook:** Custom callback for filtering/modifying events

---

## 3. Data Model / Database Schema

### PostgreSQL Schema (Prisma)

#### `user` table

| Column | Type | Notes |
|---|---|---|
| user_id | UUID (PK) | |
| username | VARCHAR(255) | Unique |
| password | VARCHAR(60) | bcrypt hash |
| role | VARCHAR(50) | 'admin', 'user', 'view-only' |
| logo_url | VARCHAR(2183) | Nullable |
| display_name | VARCHAR(255) | Nullable |
| created_at | TIMESTAMPTZ | Default now() |
| updated_at | TIMESTAMPTZ | Auto-updated |
| deleted_at | TIMESTAMPTZ | Soft delete |

#### `website` table

| Column | Type | Notes |
|---|---|---|
| website_id | UUID (PK) | |
| name | VARCHAR(100) | |
| domain | VARCHAR(500) | Nullable |
| share_id | VARCHAR(50) | Unique, nullable |
| reset_at | TIMESTAMPTZ | Data reset date |
| user_id | UUID (FK -> user) | Nullable |
| team_id | UUID (FK -> team) | Nullable |
| created_by | UUID (FK -> user) | Nullable |
| created_at | TIMESTAMPTZ | |
| updated_at | TIMESTAMPTZ | |
| deleted_at | TIMESTAMPTZ | |

**Indexes:** userId, teamId, createdAt, shareId, createdBy

#### `session` table

| Column | Type | Notes |
|---|---|---|
| session_id | UUID (PK) | Deterministic UUID v5 |
| website_id | UUID | |
| browser | VARCHAR(20) | |
| os | VARCHAR(20) | |
| device | VARCHAR(20) | |
| screen | VARCHAR(11) | e.g. "1920x1080" |
| language | VARCHAR(35) | |
| country | CHAR(2) | ISO 3166-1 alpha-2 |
| region | VARCHAR(20) | ISO 3166-2 subdivision |
| city | VARCHAR(50) | |
| distinct_id | VARCHAR(50) | User-provided identity |
| created_at | TIMESTAMPTZ | |

**Indexes:** createdAt, websiteId, (websiteId + createdAt), (websiteId + createdAt + browser/os/device/screen/language/country/region/city)

**Key insight:** Sessions use `ON CONFLICT (session_id) DO NOTHING` -- the session_id is deterministic (hash of websiteId + IP + UA + monthly salt), so duplicate inserts are idempotent.

#### `website_event` table (the core events table)

| Column | Type | Notes |
|---|---|---|
| event_id | UUID (PK) | |
| website_id | UUID | |
| session_id | UUID (FK -> session) | |
| visit_id | UUID | Deterministic, rotates hourly + after 30min inactivity |
| created_at | TIMESTAMPTZ | |
| url_path | VARCHAR(500) | |
| url_query | VARCHAR(500) | |
| utm_source | VARCHAR(255) | |
| utm_medium | VARCHAR(255) | |
| utm_campaign | VARCHAR(255) | |
| utm_content | VARCHAR(255) | |
| utm_term | VARCHAR(255) | |
| referrer_path | VARCHAR(500) | |
| referrer_query | VARCHAR(500) | |
| referrer_domain | VARCHAR(500) | |
| page_title | VARCHAR(500) | |
| gclid | VARCHAR(255) | Google Click ID |
| fbclid | VARCHAR(255) | Facebook Click ID |
| msclkid | VARCHAR(255) | Microsoft Click ID |
| ttclid | VARCHAR(255) | TikTok Click ID |
| li_fat_id | VARCHAR(255) | LinkedIn Click ID |
| twclid | VARCHAR(255) | Twitter/X Click ID |
| event_type | INTEGER | 1=pageview, 2=custom event, 3=link event, 4=pixel event |
| event_name | VARCHAR(50) | Null for pageviews |
| tag | VARCHAR(50) | |
| hostname | VARCHAR(100) | |

**Indexes:** createdAt, sessionId, visitId, websiteId, (websiteId + createdAt), (websiteId + createdAt + urlPath/urlQuery/referrerDomain/pageTitle/eventName/tag/hostname), (websiteId + sessionId + createdAt), (websiteId + visitId + createdAt)

This is a **wide event table** -- session data is denormalized into events in ClickHouse but normalized in PostgreSQL. UTM parameters and click IDs are stored directly on each event row.

#### `event_data` table

| Column | Type | Notes |
|---|---|---|
| event_data_id | UUID (PK) | |
| website_id | UUID (FK -> website) | |
| website_event_id | UUID (FK -> website_event) | |
| data_key | VARCHAR(500) | Dot-notation for nested keys |
| string_value | VARCHAR(500) | |
| number_value | DECIMAL(19,4) | |
| date_value | TIMESTAMPTZ | |
| data_type | INTEGER | 1=string, 2=number, 3=boolean, 4=date, 5=array |
| created_at | TIMESTAMPTZ | |

**Design:** Custom event properties are stored as **EAV (Entity-Attribute-Value)** with typed columns. Nested JSON is flattened with dot-notation keys (e.g., `cart.items.count`). Each property becomes a separate row.

#### `session_data` table

Same structure as `event_data` but keyed on session_id instead of event_id. Supports the `identify()` API for attaching custom properties to sessions. Uses upsert logic (find existing by sessionId + dataKey, update or create).

#### `revenue` table

| Column | Type | Notes |
|---|---|---|
| revenue_id | UUID (PK) | |
| website_id | UUID | |
| session_id | UUID | |
| event_id | UUID | |
| event_name | VARCHAR(50) | |
| currency | VARCHAR(10) | |
| revenue | DECIMAL(19,4) | |
| created_at | TIMESTAMPTZ | |

Revenue is extracted from event_data when `data.revenue > 0 && data.currency` exist. In ClickHouse, revenue is auto-materialized from event_data via a materialized view.

#### `team` table

| Column | Type | Notes |
|---|---|---|
| team_id | UUID (PK) | |
| name | VARCHAR(50) | |
| access_code | VARCHAR(50) | Unique join code |
| logo_url | VARCHAR(2183) | |
| created_at, updated_at, deleted_at | TIMESTAMPTZ | |

#### `team_user` table

| Column | Type | Notes |
|---|---|---|
| team_user_id | UUID (PK) | |
| team_id | UUID (FK) | |
| user_id | UUID (FK) | |
| role | VARCHAR(50) | team-owner, team-manager, team-member, team-view-only |
| created_at, updated_at | TIMESTAMPTZ | |

#### `report` table

| Column | Type | Notes |
|---|---|---|
| report_id | UUID (PK) | |
| user_id | UUID (FK) | |
| website_id | UUID (FK) | |
| type | VARCHAR(50) | funnel, retention, journey, etc. |
| name | VARCHAR(200) | |
| description | VARCHAR(500) | |
| parameters | JSON | Report configuration |
| created_at, updated_at | TIMESTAMPTZ | |

#### `segment` table

| Column | Type | Notes |
|---|---|---|
| segment_id | UUID (PK) | |
| website_id | UUID (FK) | |
| type | VARCHAR(50) | 'segment' or 'cohort' |
| name | VARCHAR(200) | |
| parameters | JSON | Filter/cohort configuration |
| created_at, updated_at | TIMESTAMPTZ | |

#### `link` table

Tracked redirect links with name, url, slug, user/team ownership.

#### `pixel` table

Tracking pixels with name, slug, user/team ownership.

### ClickHouse Schema

The ClickHouse schema is fundamentally different from PostgreSQL. There is **no separate session table** -- session data is denormalized into every event row.

#### `website_event` (MergeTree)

```sql
ENGINE = MergeTree
    PARTITION BY toYYYYMM(created_at)
    ORDER BY (toStartOfHour(created_at), website_id, session_id, visit_id, created_at)
    PRIMARY KEY (toStartOfHour(created_at), website_id, session_id, visit_id)
    SETTINGS index_granularity = 8192;
```

Contains all columns from the PostgreSQL website_event table PLUS session columns (browser, os, device, screen, language, country, region, city, hostname, distinct_id). Uses `LowCardinality(String)` for categorical columns.

**Projections:**
- `website_event_url_path_projection`: ORDER BY (toStartOfDay, website_id, url_path, created_at)
- `website_event_referrer_domain_projection`: ORDER BY (toStartOfDay, website_id, referrer_domain, created_at)

#### `website_event_stats_hourly` (AggregatingMergeTree)

This is the **pre-aggregation table** -- a materialized view that rolls up events to hourly granularity per (website, visit, session, event_type):

```sql
ENGINE = AggregatingMergeTree
    PARTITION BY toYYYYMM(created_at)
    ORDER BY (website_id, event_type, toStartOfHour(created_at), cityHash64(visit_id), visit_id)
    SAMPLE BY cityHash64(visit_id);
```

Stores:
- `views` (count of pageviews)
- `min_time` / `max_time` (for duration calculation)
- `entry_url` / `exit_url` (argMin/argMax aggregates)
- Array columns for url_path, referrer_domain, utm_*, etc. (groupArrayArray)

Many dashboard queries read from this materialized view instead of the raw `website_event` table, which is the primary performance optimization for ClickHouse at scale.

#### `website_revenue` (MergeTree + Materialized View)

Auto-populated from `event_data` via a materialized view that joins revenue + currency event data rows.

#### `event_data` and `session_data`

Same structure as PostgreSQL. `session_data` uses `ReplacingMergeTree` for upsert semantics.

---

## 4. Ingestion Pipeline

### Full Path: HTTP Request to Database Write

#### 1. Tracker Script Load

The tracking script is built by Rollup from `src/tracker/index.js` into `public/script.js`. It is served with CORS `Access-Control-Allow-Origin: *` and cached for 24 hours.

**Script tag:**
```html
<script defer src="https://analytics.example.com/script.js" data-website-id="uuid"></script>
```

**Configurable attributes:**
- `data-website-id` (required)
- `data-host-url` -- override API host
- `data-auto-track` -- disable automatic pageview tracking
- `data-do-not-track` -- respect DNT headers
- `data-exclude-search` -- strip query strings
- `data-exclude-hash` -- strip hash fragments
- `data-domains` -- restrict tracking to specific domains
- `data-tag` -- attach a tag to all events
- `data-before-send` -- name of a global callback function for event modification/filtering
- `data-fetch-credentials` -- fetch credentials mode (default: 'omit')

#### 2. Client-Side Event Collection

The tracker:
1. Captures screen size, language, page title, hostname, URL, referrer
2. Hooks `history.pushState` and `history.replaceState` for SPA navigation
3. Listens for click events on elements with `data-umami-event` attributes
4. Sends events via `fetch()` POST to `/api/send` (configurable endpoint)
5. Passes a cache token via `x-umami-cache` header for subsequent requests
6. Returns `umami.track(name, data)` and `umami.identify(id, data)` APIs to the window

**Payload structure:**
```json
{
  "type": "event",
  "payload": {
    "website": "uuid",
    "hostname": "example.com",
    "screen": "1920x1080",
    "language": "en-US",
    "title": "Page Title",
    "url": "https://example.com/page",
    "referrer": "https://google.com",
    "name": "button_click",
    "data": { "key": "value" },
    "tag": "campaign-a"
  }
}
```

#### 3. Server-Side Processing (`POST /api/send`)

File: `src/app/api/send/route.ts`

1. **Parse and validate** request body with Zod schema
2. **Cache check:** Parse `x-umami-cache` JWT header to get existing sessionId/visitId
3. **Website lookup:** Fetch website from Redis cache or database (reject if not found)
4. **Client info extraction:** IP address (from headers), user agent, browser/OS/device detection (ua-parser-js, detect-browser), geo lookup (MaxMind or CDN headers from Cloudflare/Vercel/CloudFront)
5. **Bot check:** Reject if isbot detects automated traffic
6. **IP block check:** Check against IGNORE_IP env var
7. **Session ID generation:**
   ```typescript
   const sessionSalt = hash(startOfMonth(createdAt).toUTCString());
   const sessionId = id ? uuid(sourceId, id) : uuid(sourceId, ip, userAgent, sessionSalt);
   ```
   The session ID is a deterministic UUID v5 derived from website ID + IP + user agent + monthly salt. This means the same visitor gets the same session ID within a calendar month. If a `distinctId` is provided, it overrides IP/UA-based identification.

8. **Visit ID generation:**
   ```typescript
   const visitSalt = hash(startOfHour(createdAt).toUTCString());
   let visitId = cache?.visitId || uuid(sessionId, visitSalt);
   // Expire visit after 30 minutes of inactivity
   if (now - iat > 1800) {
     visitId = uuid(sessionId, visitSalt);
   }
   ```

9. **Session creation** (PostgreSQL only): INSERT with ON CONFLICT DO NOTHING
10. **Event save:** Write to database (see below)
11. **Return cache token:** JWT containing websiteId, sessionId, visitId, timestamp

#### 4. Database Write

**PostgreSQL path:**
- `prisma.client.websiteEvent.create()` -- single Prisma insert
- If eventData exists: `prisma.client.eventData.createMany()` -- bulk insert of flattened key-value pairs
- If revenue data exists: `prisma.client.revenue.create()`

**ClickHouse path:**
- **Without Kafka:** Direct `clickhouse.insert('website_event', [message])`
- **With Kafka:** `kafka.sendMessage('event', message)` -- Kafka consumer (external) loads into ClickHouse
- Event data similarly goes through Kafka or direct insert

#### 5. Batch Endpoint

`POST /api/batch` accepts an array of event payloads and processes each sequentially by calling the `send.POST()` handler in a loop. No true batching/buffering -- it is a convenience wrapper.

#### 6. Pixel Tracking (`GET /p/[slug]`)

Looks up pixel by slug (Redis-cached), constructs an event payload internally, calls `POST /api/send` programmatically, returns a 1x1 transparent GIF.

#### 7. Link Tracking (`GET /q/[slug]`)

Looks up link by slug (Redis-cached), records an event, returns a 302 redirect to the target URL.

### Key Observation: No Write Buffering

In the PostgreSQL path, there is **no write buffering, batching, or queueing**. Every single event results in an immediate INSERT. The only buffering mechanism is the optional Kafka integration for ClickHouse deployments.

---

## 5. Query Patterns

### Dashboard Queries

All dashboard queries use **raw SQL** (not Prisma ORM queries). The query builder dynamically constructs SQL with:

- Filter injection via parameterized queries
- Date range filtering
- Session join (only when session columns are needed)
- Cohort subqueries (CTE-based)
- Segment filter application

### Time-Series Charts

**Pageview stats** (`getPageviewStats`):
```sql
SELECT to_char(date_trunc('day', created_at, 'timezone'), 'format') x, count(*) y
FROM website_event
WHERE website_id = $1 AND created_at BETWEEN $2 AND $3 AND event_type != 2
GROUP BY 1 ORDER BY 1
```

Sessions are counted similarly with `count(distinct session_id)`.

### Unique Visitor Counting

- **PostgreSQL:** `count(distinct session_id)` -- exact count
- **ClickHouse:** `uniq(session_id)` -- HyperLogLog approximate count (ClickHouse default)
- **ClickHouse (some queries):** `uniqExact(session_id)` -- exact count for attribution/metrics

### Pre-Aggregation (ClickHouse Only)

The `website_event_stats_hourly` materialized view is the main performance optimization. When queries don't require specific event-column filters, they read from this hourly rollup table instead of scanning raw events:

```sql
-- Fast path (no event column filters):
SELECT sum(views) FROM website_event_stats_hourly WHERE ...

-- Slow path (event column filters present):
SELECT count(*) FROM website_event WHERE ...
```

The code checks at query time:
```typescript
if (EVENT_COLUMNS.some(item => Object.keys(filters).includes(item))) {
  // query raw website_event table
} else {
  // query website_event_stats_hourly materialized view
}
```

### Bounce Rate

Bounces are visits with exactly 1 pageview:
```sql
SELECT sum(case when t.c = 1 then 1 else 0 end) as "bounces"
FROM (
  SELECT visit_id, count(*) as c
  FROM website_event
  WHERE event_type != 2
  GROUP BY visit_id
) t
```

### Visit Duration

Calculated as the difference between the first and last event timestamps within a visit:
```sql
sum(extract(epoch from (t.max_time - t.min_time))) as "totaltime"
```

### Entry/Exit Pages

- **PostgreSQL:** Uses `DISTINCT ON (visit_id)` ordered by created_at ASC (entry) or DESC (exit)
- **ClickHouse:** Uses `argMin(url_path, created_at)` (entry) or `argMax(url_path, created_at)` (exit)

### Active Visitors

Simple count of distinct sessions in the last 5 minutes:
```sql
SELECT count(distinct session_id) FROM website_event
WHERE website_id = $1 AND created_at >= now() - interval '5 minutes'
```

### Channel Classification

Traffic channels are classified in SQL using domain matching against hardcoded lists (SOCIAL_DOMAINS, SEARCH_DOMAINS, SHOPPING_DOMAINS, etc.) and UTM parameter inspection. The classification happens at query time, not at ingestion. In ClickHouse, uses `multiSearchAny()` for efficient multi-pattern matching.

---

## 6. Architecture

### Monolith

Umami is a **pure monolith**. The entire application -- API, dashboard UI, tracker script, ingestion endpoint -- is a single Next.js application. There are no microservices, no separate worker processes, no background jobs.

### Concurrency

Next.js handles concurrent requests through Node.js's event loop. There is no explicit concurrency management. The ClickHouse + Kafka path is the only architecture that offloads write processing.

### Deployment Model

1. **Docker** (primary): Single container running Next.js standalone server
2. **Source build:** `pnpm install && pnpm build && pnpm start`
3. **Docker Compose:** Umami + PostgreSQL
4. **Netlify:** netlify.toml present for edge deployment
5. **Heroku/Railway:** app.json present

### Caching Layer

1. **Redis** (optional): Website, session, pixel, link lookups; auth token storage
2. **Client-side JWT cache:** The `x-umami-cache` header avoids repeated website lookups and session creation
3. **Browser caching:** Tracker script cached 24 hours via Cache-Control headers
4. **No application-level query cache** -- every dashboard request queries the database

### High Traffic Handling

The architecture has clear scaling tiers:

1. **Small scale:** PostgreSQL only, no Redis. Every event is a synchronous INSERT.
2. **Medium scale:** PostgreSQL + Redis. Website/session lookups cached.
3. **Large scale:** ClickHouse + Kafka + Redis. Events buffered through Kafka, queries hit pre-aggregated materialized views.

**Weakness:** The PostgreSQL path has no write optimization. At high traffic, the session table gets hammered with `INSERT ... ON CONFLICT DO NOTHING` on every single pageview. The wide indexing strategy on website_event (12+ indexes) also creates write amplification.

---

## 7. Privacy / Compliance

### Cookie-Free Tracking

Umami uses **zero cookies**. Session identity is maintained through:

1. **Deterministic session ID:** `uuid_v5(hash(websiteId + IP + userAgent + monthlySalt))` -- the same inputs always produce the same session ID
2. **Client-side cache token:** JWT passed via `x-umami-cache` header for visit continuity
3. **Monthly rotation:** The session salt changes monthly, so the same visitor gets a new session ID each month

### IP Handling

- **IP is never stored in the database.** It is used only at ingestion time for:
  1. Session ID generation (hashed, not stored)
  2. GeoIP lookup (country/region/city stored, IP discarded)
- IP source priority: Custom header (`CLIENT_IP_HEADER`), then CDN headers (Cloudflare, Vercel, CloudFront, etc.), then `x-forwarded-for`
- IP blocking via `IGNORE_IP` env var supports individual IPs and CIDR ranges

### GeoIP

- Uses MaxMind GeoLite2-City database
- Checks CDN-provided geo headers first (Cloudflare, Vercel, CloudFront) before falling back to MaxMind
- Can be skipped with `SKIP_LOCATION_HEADERS`
- Localhost IPs return null location

### Bot Filtering

- Uses the `isbot` library to detect bots from user agent strings
- Can be disabled with `DISABLE_BOT_CHECK` env var

### Do Not Track

- Tracker respects DNT when `data-do-not-track="true"` is set on the script tag
- Checks `navigator.doNotTrack`, `navigator.msDoNotTrack`, and `window.doNotTrack`

### User Opt-Out

- Setting `umami.disabled` in localStorage disables tracking
- Domain restriction via `data-domains` attribute

### GDPR Compliance Claim

By not using cookies and not storing IPs, Umami claims to be GDPR-compliant without requiring cookie consent banners. The session hashing with monthly rotation means individual users cannot be re-identified across months.

**Weakness:** The `distinct_id` feature (from `identify()`) breaks this privacy model by allowing explicit user identification. If a site uses `umami.identify('user@email.com')`, that email is stored in plain text in the session_data table.

---

## 8. API Design

### Authentication

- **Bearer token:** `Authorization: Bearer <encrypted-jwt>`
- Tokens are AES-256-GCM encrypted JWTs containing userId and role
- With Redis: tokens contain an auth key that maps to user data in Redis
- Share tokens: Separate JWT in `x-umami-share-token` header for public dashboards

### Endpoints

#### Auth
| Method | Path | Description |
|---|---|---|
| POST | `/api/auth/login` | Login with username/password |
| POST | `/api/auth/logout` | Logout |
| POST | `/api/auth/sso` | SSO token refresh |
| POST | `/api/auth/verify` | Verify token validity |

#### Event Collection
| Method | Path | Description |
|---|---|---|
| POST | `/api/send` | Primary event ingestion endpoint |
| POST | `/api/batch` | Batch event ingestion |
| GET | `/p/[slug]` | Pixel tracking (returns 1x1 GIF) |
| GET | `/q/[slug]` | Link tracking (302 redirect) |

#### Current User
| Method | Path | Description |
|---|---|---|
| GET | `/api/me` | Get current user |
| POST | `/api/me/password` | Change password |
| GET | `/api/me/teams` | List user's teams |
| GET | `/api/me/websites` | List user's websites |

#### Websites
| Method | Path | Description |
|---|---|---|
| GET/POST | `/api/websites` | List/create websites |
| GET/PUT/DELETE | `/api/websites/[id]` | CRUD single website |
| GET | `/api/websites/[id]/active` | Active visitor count |
| GET | `/api/websites/[id]/daterange` | Available date range |
| GET | `/api/websites/[id]/stats` | Aggregate stats with comparison |
| GET | `/api/websites/[id]/pageviews` | Pageview time series |
| GET | `/api/websites/[id]/metrics` | Dimension metrics (type param) |
| GET | `/api/websites/[id]/metrics/expanded` | Expanded metrics view |
| GET | `/api/websites/[id]/events` | Event list |
| GET | `/api/websites/[id]/events/series` | Event time series |
| GET | `/api/websites/[id]/sessions` | Session list |
| GET | `/api/websites/[id]/sessions/stats` | Session statistics |
| GET | `/api/websites/[id]/sessions/weekly` | Weekly traffic pattern |
| GET | `/api/websites/[id]/sessions/[sid]` | Session detail |
| GET | `/api/websites/[id]/sessions/[sid]/activity` | Session activity log |
| GET | `/api/websites/[id]/sessions/[sid]/properties` | Session custom properties |
| GET | `/api/websites/[id]/values` | Distinct values for filters |
| GET | `/api/websites/[id]/export` | Export data as ZIP/CSV |
| POST | `/api/websites/[id]/reset` | Reset website data |
| POST | `/api/websites/[id]/transfer` | Transfer website ownership |

#### Event Data
| Method | Path | Description |
|---|---|---|
| GET | `/api/websites/[id]/event-data/events` | Event names list |
| GET | `/api/websites/[id]/event-data/fields` | Event data fields |
| GET | `/api/websites/[id]/event-data/properties` | Event data properties |
| GET | `/api/websites/[id]/event-data/stats` | Event data stats |
| GET | `/api/websites/[id]/event-data/values` | Event data values |
| GET | `/api/websites/[id]/event-data/[eventId]` | Single event data |

#### Session Data
| Method | Path | Description |
|---|---|---|
| GET | `/api/websites/[id]/session-data/properties` | Session data properties |
| GET | `/api/websites/[id]/session-data/values` | Session data values |

#### Segments
| Method | Path | Description |
|---|---|---|
| GET/POST | `/api/websites/[id]/segments` | List/create segments |
| GET/PUT/DELETE | `/api/websites/[id]/segments/[segId]` | CRUD single segment |

#### Reports
| Method | Path | Description |
|---|---|---|
| GET/POST | `/api/reports` | List/create reports |
| GET/PUT/DELETE | `/api/reports/[id]` | CRUD single report |
| POST | `/api/reports/funnel` | Run funnel analysis |
| POST | `/api/reports/retention` | Run retention analysis |
| POST | `/api/reports/journey` | Run journey analysis |
| POST | `/api/reports/goal` | Run goal analysis |
| POST | `/api/reports/revenue` | Run revenue analysis |
| POST | `/api/reports/attribution` | Run attribution analysis |
| POST | `/api/reports/breakdown` | Run breakdown analysis |
| POST | `/api/reports/utm` | Run UTM analysis |

#### Teams
| Method | Path | Description |
|---|---|---|
| GET/POST | `/api/teams` | List/create teams |
| GET/PUT/DELETE | `/api/teams/[id]` | CRUD single team |
| POST | `/api/teams/join` | Join team via access code |
| GET/POST | `/api/teams/[id]/users` | List/add team members |
| PUT/DELETE | `/api/teams/[id]/users/[uid]` | Update/remove team member |
| GET | `/api/teams/[id]/websites` | List team websites |
| GET | `/api/teams/[id]/links` | List team links |
| GET | `/api/teams/[id]/pixels` | List team pixels |

#### Users (Admin)
| Method | Path | Description |
|---|---|---|
| GET/POST | `/api/users` | List/create users |
| GET/PUT/DELETE | `/api/users/[id]` | CRUD single user |
| GET | `/api/users/[id]/teams` | List user's teams |
| GET | `/api/users/[id]/websites` | List user's websites |

#### Links
| Method | Path | Description |
|---|---|---|
| GET/POST | `/api/links` | List/create links |
| GET/PUT/DELETE | `/api/links/[id]` | CRUD single link |

#### Pixels
| Method | Path | Description |
|---|---|---|
| GET/POST | `/api/pixels` | List/create pixels |
| GET/PUT/DELETE | `/api/pixels/[id]` | CRUD single pixel |

#### Admin
| Method | Path | Description |
|---|---|---|
| GET | `/api/admin/users` | Admin: list all users |
| GET | `/api/admin/teams` | Admin: list all teams |
| GET | `/api/admin/websites` | Admin: list all websites |

#### Other
| Method | Path | Description |
|---|---|---|
| GET | `/api/config` | Public app configuration |
| GET | `/api/heartbeat` | Health check |
| GET | `/api/share/[shareId]` | Get shared website token |
| GET | `/api/realtime/[websiteId]` | Real-time data |
| GET | `/api/scripts/telemetry` | Self-telemetry script |

### Rate Limiting

**There is no rate limiting in the source code.** No middleware, no IP-based throttling, no request counting. This is a significant gap for self-hosted deployments facing abuse.

### Input Validation

All endpoints use **Zod schemas** for request validation. The `parseRequest()` utility handles:
1. JSON body parsing (POST/PUT)
2. Query parameter parsing (GET)
3. Zod schema validation
4. Auth token verification

---

## 9. Strengths and Weaknesses

### Strengths

1. **Privacy-first session tracking is genuinely clever.** The deterministic UUID v5 approach (hash of websiteId + IP + UA + monthly salt) gives session continuity without cookies and without storing PII. The monthly rotation balances analytics accuracy with privacy. This is the core differentiator and it works well.

2. **Dual database architecture is well-executed.** The `runQuery()` dispatch pattern cleanly separates PostgreSQL and ClickHouse implementations. Every query has two implementations tuned for its engine. This allows gradual scaling from PostgreSQL to ClickHouse without changing the API layer.

3. **ClickHouse materialized views for pre-aggregation.** The `website_event_stats_hourly` materialized view with `AggregatingMergeTree` is a textbook optimization. Most dashboard queries hit the rollup table, dramatically reducing scan volume for high-traffic sites.

4. **UTM + Click ID tracking is comprehensive.** First-party capture of gclid, fbclid, msclkid, ttclid, li_fat_id, twclid covers all major ad platforms. The attribution report with first-click/last-click models is genuinely useful.

5. **Channel classification logic.** The hardcoded domain lists (social, search, shopping, email, video) combined with UTM parameter analysis for paid/organic classification gives good out-of-the-box channel insights.

6. **Tracker script is tiny and well-designed.** Single IIFE, no dependencies, hooks into History API for SPA support, `beforeSend` callback for filtering, `data-*` attributes for configuration, `keepalive: true` for reliable delivery on page unload.

7. **Share links for public dashboards.** Simple JWT-based anonymous access -- no account needed to view shared stats.

8. **52 languages.** Extensive i18n coverage.

### Weaknesses

1. **No write buffering in PostgreSQL mode.** Every single pageview triggers a synchronous database INSERT. There is no in-memory buffer, no write batching, no async queue. At moderate traffic (>100 req/s), this will saturate a PostgreSQL connection pool. The 12+ indexes on `website_event` compound this problem.

2. **No query caching.** Every dashboard page load fires multiple raw SQL queries. No in-memory cache, no Redis query cache, no stale-while-revalidate. The Redis integration only caches entity lookups (website, session), not analytics results.

3. **No rate limiting.** The ingestion endpoint (`/api/send`) has zero protection against abuse. Anyone can send unlimited events to any website ID. The only guard is the bot check.

4. **EAV pattern for custom event data is a scalability trap.** Each property of each event becomes a separate row in `event_data`. An event with 10 properties generates 10 rows. At scale, this table grows 5-10x faster than the events table, and querying across properties requires self-joins.

5. **No data sampling or approximation in PostgreSQL.** All PostgreSQL queries use exact counts (`count(distinct)`). For large datasets, this is orders of magnitude slower than ClickHouse's `uniq()` (HyperLogLog). There is no option for approximate queries.

6. **Monolithic architecture limits scaling.** The ingestion endpoint and dashboard API share the same Node.js process. A traffic spike on the ingestion path directly impacts dashboard query latency.

7. **No retention/data lifecycle management.** No automatic data pruning, no configurable retention periods, no data downsampling for old data. The `reset_at` feature is manual and deletes everything before a date.

8. **No alerting or anomaly detection.** No way to set up alerts for traffic drops, spikes, or goal completion.

9. **No A/B testing or feature flags.** PostHog's core differentiator that Umami completely lacks.

10. **No session replay, heatmaps, or error tracking.** Pure analytics only -- no qualitative user behavior tools.

11. **Limited real-time capabilities.** Real-time is polling-based (client polls every 10 seconds), not WebSocket-based. The data is just a re-query of the last N minutes.

12. **Batch endpoint is fake batching.** The `/api/batch` endpoint processes events sequentially in a for-loop, calling `POST /api/send` for each one. There is no actual batch INSERT optimization.

### Design Decisions Worth Copying

1. **Deterministic session IDs** via salted hash. Eliminates cookies entirely while maintaining session accuracy within a month. The monthly rotation is a good privacy/accuracy tradeoff.

2. **Visit ID as a separate concept from session ID.** Visits expire after 30 minutes of inactivity OR hourly boundary, while sessions persist for a month. This gives proper bounce rate and visit duration metrics.

3. **Tracker script `data-*` attribute configuration.** Clean DX -- no JavaScript config needed, everything declarative on the script tag.

4. **`x-umami-cache` JWT header pattern.** Eliminates redundant website lookups and session creation for repeat requests from the same page session.

5. **Channel classification at query time.** Keeping the logic in SQL rather than at ingestion means channels can be reclassified retroactively if domain lists change.

6. **ClickHouse MergeTree partitioning by month** with hourly primary key granularity. Good balance of partition size and query selectivity.

### What to Avoid

1. **Writing every query twice** (PostgreSQL + ClickHouse). This is a maintenance burden. Consider a query abstraction layer or pick one database engine.

2. **No write buffering.** At minimum, buffer events in memory and batch-insert every N seconds or N events.

3. **EAV for custom properties.** Consider JSONB (PostgreSQL) or JSON columns (ClickHouse) instead of row-per-property expansion.

4. **No rate limiting on ingestion.** Must have from day one.

5. **Monolith for both reads and writes.** Separate ingestion from the dashboard API early, even if they share the same codebase initially. At least use separate process pools.

6. **`$queryRawUnsafe` for everything.** The custom `{{param}}` template syntax works but loses Prisma's type safety. If doing raw SQL, consider a proper query builder (like Kysely or Drizzle).

---

## Appendix: Environment Variables

| Variable | Purpose |
|---|---|
| `DATABASE_URL` | PostgreSQL connection string |
| `DATABASE_REPLICA_URL` | Read replica connection string |
| `CLICKHOUSE_URL` | ClickHouse connection URL (enables ClickHouse mode) |
| `KAFKA_URL` | Kafka connection URL |
| `KAFKA_BROKER` | Kafka broker addresses (comma-separated) |
| `REDIS_URL` | Redis connection URL (enables caching) |
| `APP_SECRET` | Secret for JWT signing and encryption |
| `BASE_PATH` | URL base path for reverse proxy setups |
| `CLOUD_MODE` | Enable cloud mode (Umami SaaS) |
| `COLLECT_API_ENDPOINT` | Custom collection endpoint path |
| `TRACKER_SCRIPT_NAME` | Custom tracker script filename(s) |
| `TRACKER_SCRIPT_URL` | External tracker script URL |
| `CLIENT_IP_HEADER` | Custom header for client IP |
| `IGNORE_IP` | Comma-separated IPs/CIDRs to block |
| `DISABLE_BOT_CHECK` | Disable bot filtering |
| `DISABLE_TELEMETRY` | Disable self-telemetry |
| `DISABLE_UPDATES` | Disable update check |
| `FORCE_SSL` | Enable HSTS headers |
| `REMOVE_TRAILING_SLASH` | Normalize URL paths |
| `PRIVATE_MODE` | Enable private mode |
| `LOG_QUERY` | Enable query logging |
| `DEFAULT_LOCALE` | Default language |
| `GEOLITE_DB_PATH` | Custom GeoLite2 database path |
| `SKIP_LOCATION_HEADERS` | Skip CDN geo headers |
| `ALLOWED_FRAME_URLS` | CSP frame-ancestors |
| `USE_UUIDV7` | Use UUID v7 instead of v4 |

---

## Appendix: File Paths Reference

| Area | Path |
|---|---|
| Prisma schema | `prisma/schema.prisma` |
| ClickHouse schema | `db/clickhouse/schema.sql` |
| Tracker source | `src/tracker/index.js` |
| Tracker build config | `rollup.tracker.config.js` |
| Send endpoint | `src/app/api/send/route.ts` |
| Batch endpoint | `src/app/api/batch/route.ts` |
| Database dispatch | `src/lib/db.ts` |
| Prisma wrapper | `src/lib/prisma.ts` |
| ClickHouse wrapper | `src/lib/clickhouse.ts` |
| Kafka wrapper | `src/lib/kafka.ts` |
| Redis wrapper | `src/lib/redis.ts` |
| Session/visit ID generation | `src/lib/crypto.ts` |
| IP detection | `src/lib/ip.ts` |
| Geo/client detection | `src/lib/detect.ts` |
| Auth system | `src/lib/auth.ts` |
| JWT handling | `src/lib/jwt.ts` |
| Constants | `src/lib/constants.ts` |
| Event data flattening | `src/lib/data.ts` |
| All SQL queries | `src/queries/sql/` |
| All Prisma CRUD queries | `src/queries/prisma/` |
| Permissions | `src/permissions/` |
| API routes | `src/app/api/` |
| Dashboard pages | `src/app/(main)/` |
