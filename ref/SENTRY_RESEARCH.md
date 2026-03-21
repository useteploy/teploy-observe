# Sentry Competitor Research -- Complete Technical Analysis

**Date:** 2026-03-20
**Source:** getsentry/sentry main repo (cloned locally)

---

## 1. Tech Stack

### Languages and Frameworks

- **Backend:** Python 3.13+ on Django 5.2+
- **API Layer:** Django REST Framework with drf-spectacular for OpenAPI docs
- **Frontend:** React (TypeScript), Emotion for CSS-in-JS, pnpm as package manager, Node 24.14.0 LTS
- **Task Queue:** Celery 5.5+ with RabbitMQ as broker
- **Stream Processing:** Arroyo (their own Kafka consumer/producer framework)
- **Normalization:** sentry-relay (Rust library called via Python FFI -- `sentry_relay.processing.StoreNormalizer`)
- **Grouping Enhancer:** sentry-ophio (Rust library -- `sentry_ophio.enhancers.Enhancements`)
- **Build/Infra:** Docker, pnpm (frontend), pip (backend), devenv/devservices for local dev

### Databases

| Database | Purpose |
|---|---|
| **PostgreSQL** | Primary relational store: organizations, projects, groups (issues), users, alert rules, releases, grouphashes, all Django models |
| **ClickHouse** (via Snuba) | Event storage and analytics: raw error events, transactions, spans, profiles, replays, metrics, session data |
| **Redis** | Caching (grouphash lookups, config caching), rate limiting, real-time buffers, TSDB counters |
| **Kafka** | Message bus between all services: ingestion pipeline, event streaming to Snuba/ClickHouse, subscription results for alerts |
| **NodeStore** (Bigtable/PostgreSQL) | Blob storage for full event payloads (the actual JSON bodies) |

### What is Snuba?

Snuba is Sentry's **query abstraction layer over ClickHouse**. It is a separate service that:
1. Consumes events from Kafka and writes them into ClickHouse tables
2. Provides a query API that translates structured queries (using `snuba_sdk`) into ClickHouse SQL
3. Manages multiple **datasets** (Events, Transactions, Discover, Metrics, Profiles, Spans, etc.)
4. Handles **query subscriptions** for real-time alerting -- Snuba periodically runs queries and publishes results back to Kafka

Key datasets defined in `src/sentry/snuba/dataset.py`:
```python
class Dataset(Enum):
    Events = "events"           # All ingested errors
    Transactions = "transactions"  # All ingested transactions
    Discover = "discover"        # Combined events + transactions
    Outcomes = "outcomes"        # Usage tracking (materialized views)
    Metrics = "metrics"          # Release health metrics
    PerformanceMetrics = "generic_metrics"  # Generic metrics platform
    Replays = "replays"          # Session Replays
    Profiles = "profiles"        # Profiling data
    IssuePlatform = "search_issues"  # Non-error issues
    Functions = "functions"      # Profiling functions
    SpansIndexed = "spans"       # Searchable span data
    EventsAnalyticsPlatform = "events_analytics_platform"
```

### Deployment Model

Self-hosted uses Docker Compose. The `self-hosted/` directory contains a `Dockerfile`, `docker-entrypoint.sh`, `config.yml`, and `sentry.conf.py`. The config uses filesystem storage by default with paths like `/data/files`, `/data/dsym-cache`, etc.

---

## 2. Core Features (2026 Feature List)

Based on code analysis and 252+ feature flags in `src/sentry/features/temporary.py`:

### Error Tracking (Core -- Original Product)
- Error capture with full stack traces, breadcrumbs, contexts
- **Error grouping** (the crown jewel -- see Section 4)
- Issue management: status (unresolved/resolved/ignored), substatus (new/ongoing/escalating/regressed)
- Issue priority levels with AI-powered severity scoring (`seer_fixability_score`)
- Merge and unmerge issues
- Custom fingerprinting rules (server-side and client-side)
- Source map processing and symbolication
- Release tracking and regression detection

### Performance Monitoring
- Transaction/span-based tracing
- Web Vitals (LCP, CLS, FCP, FID, INP, TTFB)
- App Start metrics (cold/warm)
- Frame metrics (total/slow/frozen)
- Performance issue detection (N+1 queries, slow DB queries, etc.)
- Distributed tracing across services

### Session Replay
- Full session recording with DOM replay
- Dead click / rage click detection
- Linked to errors and transactions

### Profiling
- Continuous profiling
- Profile functions analysis
- Function-level performance metrics

### Crons / Uptime Monitoring
- Cron job monitoring (check-ins)
- Uptime monitoring with results tracking

### Dashboards and Discover
- Custom dashboards with multiple widget types (charts, tables, text)
- Saved queries
- Discover query interface across datasets
- AI-powered dashboard generation

### Alerts and Notifications
- Metric alerts (threshold-based on Snuba queries)
- Anomaly detection alerts (AI-powered via Seer)
- Issue alerts (rule-based on error events)
- Incident management with status tracking
- Integration with Slack, PagerDuty, email, webhooks, etc.

### Releases
- Release tracking with semver support
- Commit association and suspect commits
- Deploy tracking
- Release health (crash rates, adoption)
- Regression detection

### Integrations (Extensive)
- GitHub, GitLab, Bitbucket (source code, commits, PRs)
- Jira, Linear (issue tracking)
- Slack, MS Teams, Discord (notifications)
- PagerDuty, Opsgenie (incident management)
- Vercel, Netlify (deployment)
- And many more via the plugin/integration system

### Newer / Experimental (2025-2026 additions)
- **Seer** -- AI-powered features: severity scoring, similar issue detection, autofix, anomaly detection
- **Codecov integration** -- Code coverage linked to errors
- **Code review** features
- **Explore** interface (new query builder)
- **Logs** (ourlogs dataset)
- **Flags** -- Feature flag tracking linked to errors
- **Dynamic sampling** -- Intelligent sampling based on trace health
- **Insights** modules (DB, HTTP, Assets, App Starts, Screen Loads, etc.)
- **Workflow engine** -- Detector-based automated actions
- **Data secrecy** with "Break the Glass" feature
- **Tempest** (unclear purpose, has its own module)

---

## 3. Data Model / Database Schema

### PostgreSQL Models (Key Entities)

#### Organization
- Top-level tenant. All data is scoped to an organization.

#### Project
- Belongs to an Organization. Events are submitted per-project via DSN.

#### Group (Issue) -- `sentry_groupedmessage`
**File:** `src/sentry/models/group.py` (line 626)

```python
class Group(Model):
    project = FlexibleForeignKey("sentry.Project")
    logger = models.CharField(max_length=64)
    level = BoundedPositiveIntegerField()   # ERROR, WARNING, INFO, etc.
    message = models.TextField()            # Truncated first line of message
    culprit = models.CharField(max_length=MAX_CULPRIT_LENGTH)
    platform = models.CharField(max_length=64)
    status = BoundedPositiveIntegerField()  # UNRESOLVED=0, RESOLVED=1, IGNORED=2, etc.
    substatus = BoundedIntegerField()       # NEW, ONGOING, ESCALATING, REGRESSED, etc.
    times_seen = BoundedPositiveIntegerField(default=1)
    last_seen = models.DateTimeField()
    first_seen = models.DateTimeField()
    first_release = FlexibleForeignKey("sentry.Release")
    active_at = models.DateTimeField()
    short_id = BoundedBigIntegerField()     # Human-readable ID like "PROJECT-123"
    type = BoundedPositiveIntegerField()    # ErrorGroupType=1, performance issue types, etc.
    priority = models.PositiveIntegerField()
    seer_fixability_score = models.FloatField()
    data = LegacyTextJSONField()            # Metadata JSON blob
```

Key indexes on Group:
```
(project, first_release)
(project, id)
(project, status, last_seen, id)
(project, status, type, last_seen, id)
(project, status, substatus, last_seen, id)
(project, status, substatus, type, last_seen, id)
(project, status, priority, last_seen, id)
```

**Critical insight:** The Group model stores aggregate metadata only. The actual event data lives in ClickHouse (via Snuba) and NodeStore (full JSON blobs).

#### GroupHash -- `sentry_grouphash`
**File:** `src/sentry/models/grouphash.py`

The bridge between computed hashes and groups:

```python
class GroupHash(Model):
    project = FlexibleForeignKey("sentry.Project")
    hash = models.CharField(max_length=32)          # MD5 hex digest
    group = FlexibleForeignKey("sentry.Group", null=True)
    group_tombstone_id = BoundedPositiveIntegerField()  # For discarded events
    state = BoundedPositiveIntegerField()            # Locked during migration
    date_added = models.DateTimeField()

    # unique_together = (("project", "hash"),)
```

**This is the critical mapping table.** When an event arrives, its hash is computed, then looked up in GroupHash. If a match exists, the event joins that group. If not, a new group is created.

#### Release
**File:** `src/sentry/models/release.py` (line 196)

```python
class Release(Model):
    organization = FlexibleForeignKey("sentry.Organization")
    projects = models.ManyToManyField("sentry.Project", through=ReleaseProject)
    version = models.CharField(max_length=250)
    status = BoundedPositiveIntegerField()  # OPEN, ARCHIVED
    ref = models.CharField()                # Branch name
    date_added = models.DateTimeField()
    date_released = models.DateTimeField()
    commit_count = BoundedPositiveIntegerField()
    last_commit_id = BoundedBigIntegerField()
    authors = ArrayField(models.TextField())
    # Denormalized semver columns:
    package = models.TextField()
    major = models.BigIntegerField()
    minor = models.BigIntegerField()
    patch = models.BigIntegerField()
    revision = models.BigIntegerField()
```

### ClickHouse (via Snuba)

Events in ClickHouse are organized by dataset. The key columns visible from the Python code (`src/sentry/snuba/events.py`):

```
event_id, group_id, project_id, timestamp, time (bucketed)
culprit, title, location, message, platform
type, tags, contexts, user, ip_address
sdk.name, sdk.version, http.method, http.url
stack.* (abs_path, filename, function, module, lineno, colno, in_app, package)
```

Events are deduplicated on `(event_id, project_id, day)`. The latest event wins within a day.

ClickHouse table engines used: The codebase references MergeTree variants through Snuba (which manages the actual table DDL). Based on dataset patterns, errors likely use ReplacingMergeTree (for deduplication), and metrics use AggregatingMergeTree.

### Event Data Structure

An error event contains these interfaces (from `src/sentry/interfaces/`):

| Interface | Description |
|---|---|
| `exception` | Exception type, value, stacktrace, mechanism, chained exceptions |
| `stacktrace` | Frames with filename, function, module, lineno, context_line, in_app flag |
| `message` / `logentry` | Log message with formatted and raw versions |
| `breadcrumbs` | Chronological trail of events leading to the error |
| `contexts` | Device, OS, browser, runtime, app, GPU info |
| `user` | User ID, email, username, IP |
| `http` | Request method, URL, headers, query string |
| `sdk` | SDK name and version |
| `threads` | Thread information with stacktraces |
| `debug_meta` | Debug file references for native symbolication |
| `spans` | Performance spans within a transaction |
| `template` | Template rendering info |
| `security` | CSP, HPKP, Expect-CT/Staple reports |

---

## 4. Error Grouping Algorithm -- CRITICAL SECTION

### Overview

Error grouping is Sentry's most complex and valuable algorithm. It determines which errors are "the same issue." The system has evolved through multiple versioned configurations and involves several layers.

**Key files:**
- `src/sentry/grouping/api.py` -- Main entry point
- `src/sentry/grouping/strategies/newstyle.py` -- Frame, stacktrace, exception, chained exception strategies
- `src/sentry/grouping/strategies/message.py` -- Message-based grouping
- `src/sentry/grouping/strategies/configurations.py` -- Config versions
- `src/sentry/grouping/strategies/base.py` -- Strategy framework
- `src/sentry/grouping/component.py` -- Component tree nodes
- `src/sentry/grouping/variants.py` -- Variant types
- `src/sentry/grouping/fingerprinting/` -- Custom fingerprinting rules
- `src/sentry/grouping/enhancer/` -- Stack trace enhancement rules
- `src/sentry/grouping/parameterization.py` -- Message parameterization (replacing dynamic values)
- `src/sentry/grouping/ingest/hashing.py` -- Ingest-time hash calculation
- `src/sentry/grouping/utils.py` -- Hash computation, message normalization

### The Grouping Pipeline

```
Event arrives
    |
    v
1. Apply server-side fingerprinting rules
    |
    v
2. Normalize stacktraces (via enhancer rules)
    |
    v
3. Run grouping strategies (in priority order):
   - chained-exception:v1 (score=2000) -- handles single + chained exceptions
   - threads:v1
   - stacktrace:v1 (score=1800) -- bare stacktraces without exceptions
   - template:v1
   - csp:v1, hpkp:v1, expect-staple:v1, expect-ct:v1
   - message:v1 (score=0) -- lowest priority, only used if nothing else works
    |
    v
4. Build component tree -> variants -> hashes
    |
    v
5. Apply fingerprint type (default / custom / hybrid)
    |
    v
6. Look up hashes in GroupHash table
    |
    v
7. If no match, optionally consult Seer AI for similar issues
    |
    v
8. Assign to existing group or create new one
```

### Grouping Configs (Versions)

```python
# From src/sentry/grouping/strategies/configurations.py

# Current default (Winter 2023)
WINTER_2023_GROUPING_CONFIG = "newstyle:2023-01-11"

# Next generation (Fall 2025)
FALL_2025_GROUPING_CONFIG = "newstyle:2025-09-01"
```

The Fall 2025 config fixes several legacy bugs:
- `use_legacy_exception_subcomponent_order: False` -- fixes component ordering
- `handle_js_single_frame_url_origin_backwards: False` -- fixes inverted JS URL check
- `prevent_python_multiprocessing_context_line_parameterization: False`
- `use_legacy_unknown_variable_handling: False` -- flags unknown fingerprint variables

Projects can transition between configs using primary/secondary grouping. During transition, BOTH configs are run and secondary hashes are used to link new primary hashes to existing groups.

### Strategies in Detail

#### Strategy Priority (highest to lowest)
1. **chained-exception:v1** (score=2000) -- Handles both single and chained exceptions
2. **stacktrace:v1** (score=1800) -- Bare stacktraces (no exception wrapper)
3. **template:v1** -- Template rendering errors
4. **csp:v1, hpkp:v1, expect-staple:v1, expect-ct:v1** -- Security reports
5. **message:v1** (score=0) -- Fallback to log message

The **first strategy to produce a contributing result wins**. Lower-priority strategies are marked as non-contributing.

#### Variants: app vs. system

Most strategies produce **two variants**:
- **app variant** (`!app`): Only considers in-app frames (user code)
- **system variant** (`system`): Considers all frames including third-party/system

The `!` prefix on `app` marks it as **priority** -- if both produce the same hash, the system variant is deduplicated against the app variant.

The app variant is preferred. If both contribute, `app` wins.

#### Frame Grouping (`frame:v1`)

Each stack frame produces a component from:

1. **Module** (highest priority for non-JS)
2. **Filename** (ignored if module contributes)
3. **Function name** (with extensive normalization)
4. **Context line** (only for JS, Python, PHP, Ruby, Node)

Frame normalization rules:
- **Filename:** Takes basename only, lowercased. Ignores `<anonymous>`, `[native code]`, URLs
- **Module:** Strips Java/Clojure codegen markers (CGLIB, Javassist, Lambda, Hibernate proxy)
- **Function:**
  - Ruby: strips erb template suffixes, normalizes block functions
  - PHP: ignores anonymous functions/classes
  - Java: ignores lambda functions
  - JavaScript: strips object path (`Object.foo` -> `foo`), ignores function if sourcemap used and context line available
  - Native: ignores `<redacted>`, `<unknown>`
- **JavaScript-specific:** Ignores `?`, `<anonymous>`, `eval`, `[native code]` frames entirely
- **Recursive frames:** Detected and ignored (compared by abs_path, package, module, filename, function, lineno, colno)

#### Stacktrace Grouping (`stacktrace:v1`)

1. Processes each frame through `frame:v1`
2. Removes recursive frames
3. For app variant: marks non-in-app frames as non-contributing
4. Applies enhancer rules (mark frames as +group/-group, +app/-app)
5. Counts frame types (in-app vs system, contributing vs non-contributing)
6. Single JS frame with no function and no URL origin is ignored

#### Exception Grouping (`single-exception:v1`)

For each exception, creates components from:
1. **Error type** (e.g., "KeyError", "NullPointerException")
2. **Error value/message** (normalized -- see message parameterization)
3. **NSError** info (domain + code, for Apple platforms)
4. **Stacktrace** (via stacktrace strategy)

**Critical precedence rules:**
- If stacktrace contributes -> error value is marked non-contributing
- If ns_error contributes -> error value is marked non-contributing
- Error type always contributes (unless synthetic exception)
- For synthetic exceptions: type is ignored (synthetic = SDK-generated dummy exceptions for harvesting stacktraces)

#### Chained Exception Grouping (`chained-exception:v1`)

1. Gets all exceptions in the chain
2. For exception groups: filters to "top-level exceptions" (non-group descendants of exception groups)
3. Deduplicates sibling exceptions with identical hashes
4. If only one exception remains, returns its component directly
5. Otherwise wraps all exception components in a `ChainedExceptionGroupingComponent`

#### Message Grouping (`message:v1`)

Used only when no other strategy contributes. Normalizes the message by:
1. **Parameterization:** Replaces dynamic values with placeholders using regex patterns for:
   - Email addresses
   - URLs (http/https/ftp/wss)
   - Hostnames (with TLD matching)
   - IP addresses (IPv4 and IPv6)
   - Trace parent IDs
   - UUIDs
   - Hex values
   - Integers/floats
   - File paths
   - And more (see `src/sentry/grouping/parameterization.py`)
2. **Trimming:** Removes blank lines, limits to 2 lines, appends "..."

### Fingerprinting

Three fingerprint types:

1. **Default** (`{{ default }}`): Uses strategy-computed hash
2. **Custom**: User-defined fingerprint completely replaces strategy hash. Can be:
   - Client-side: Set in SDK via `event.fingerprint = ["my-group"]`
   - Server-side: Matching rules in project settings
   - Built-in: Sentry's default fingerprinting rules (e.g., `javascript@2024-02-02`)
3. **Hybrid**: Combines custom values with default hash, e.g., `["my-prefix", "{{ default }}"]`

Fingerprinting rules are defined in a DSL:
```
# Match on exception type and set fingerprint
error.type:DatabaseError -> database-error

# Match on message pattern
message:"connection refused*" -> connection-refused
```

### Enhancer Rules

Stack trace enhancers modify frame behavior before grouping:
- `+app` / `-app`: Mark frames as in-app or not
- `+group` / `-group`: Include/exclude frames from grouping
- Match on: `stack.abs_path`, `stack.module`, `stack.function`, `stack.package`

The enhancer is implemented in Rust (`sentry_ophio`) for performance. It processes ~300+ built-in rules plus custom project rules.

### The Hash Computation

```python
def hash_from_values(values: Iterable[str | int]) -> str:
    result = md5()
    for value in values:
        result.update(force_bytes(value, errors="replace"))
    return result.hexdigest()
```

The hash is an **MD5 hex digest** of the concatenated contributing values from the component tree. Values are collected by recursively walking contributing branches.

### GroupHash Lookup and Assignment

**File:** `src/sentry/grouping/ingest/hashing.py`

1. Calculate hashes using primary grouping config
2. For each hash, `get_or_create` a `GroupHash` record (with 60s caching)
3. Search grouphashes for one with an assigned group
4. If found -> event joins that group
5. If not found and project is in transition -> calculate secondary hashes, check for match
6. If still not found -> ask Seer AI for similar issue match
7. If still nothing -> create new Group and assign to all grouphashes

### Known Edge Cases and Failures

1. **Config transitions:** When switching grouping configs, secondary hashing prevents creating duplicate groups, but there's a transition period where both configs must run
2. **Hash collisions across issue types:** Extremely unlikely but theoretically possible for error hashes to collide with performance issue hashes
3. **Chained exception groups:** Python SDK v2->v3 changed exception group handling, requiring a temporary hack to preserve grouping behavior
4. **JavaScript source maps:** Function names from sourcemaps are "unreliable by nature" -- when a sourcemap is used and context line is available, function is ignored
5. **Message-only grouping:** Low quality; parameterization regex can over-match or under-match
6. **Single frame JS stacks:** Special-cased to avoid low-quality groups
7. **Legacy checksum support:** Old events may use raw checksums instead of computed hashes

### Minimum Viable Grouping Algorithm

For a competing product, the minimum viable version would be:

1. **Exception + stacktrace grouping:** Hash the error type + in-app frame filenames/functions. This covers 80%+ of cases.
2. **Message parameterization:** Replace UUIDs, IPs, numbers, URLs with placeholders, then hash the message. Covers the remaining log-style errors.
3. **Custom fingerprinting:** Allow users to set `fingerprint` on events to override grouping.
4. **Fallback hash:** When nothing else works, use a static fallback so events still get grouped.

Skip: app/system variants (just use in-app), enhancer rules (can add later), secondary/transition configs (not needed at launch), Seer AI matching.

---

## 5. Ingestion Pipeline

### Full Path: SDK -> Storage

```
SDK (client)
    |
    | HTTP POST (envelope format)
    v
Relay (edge proxy, Rust)
    |
    | Rate limiting, PII scrubbing, filtering
    | Produces to Kafka
    v
Kafka: ingest-events / ingest-transactions / ingest-attachments
    |
    | Arroyo consumers (multi-process)
    v
Ingest Consumer (Python)
    |
    | Deserialization, routing by event type
    v
Celery Tasks (store.py):
    |
    +-- preprocess_event: check if processing needed
    |       |
    |       +-- (if stacktraces need symbolication)
    |       |       |
    |       |       v
    |       |   Symbolicator (Rust service)
    |       |       |
    |       |       v
    |       +-- process_event: symbolicate, run plugins
    |               |
    |               v
    +-- save_event:
            |
            v
        EventManager.save()
            |
            +-- Normalize event (Rust: StoreNormalizer)
            +-- Route by type:
            |   - error -> save_error_events()
            |   - transaction -> save_transaction_events()
            |   - generic -> save_generic_events()
            |
            +-- For errors:
            |   1. Get/create Release, Environment
            |   2. Calculate grouping hashes
            |   3. Assign event to group (create group if new)
            |   4. Save to NodeStore (event payload)
            |   5. Publish to Kafka eventstream
            |       |
            |       v
            |   Kafka: events / transactions / generic-events
            |       |
            |       v
            |   Snuba consumers -> ClickHouse
            |       |
            |       v
            +-- Post-process (async):
                - Trigger alert rules
                - Update activity feed
                - Process integrations
                - Update search index
```

### Kafka Topics

From `src/sentry/conf/types/kafka_definition.py`:

```python
# Ingestion (Relay -> Sentry)
INGEST_EVENTS = "ingest-events"
INGEST_TRANSACTIONS = "ingest-transactions"
INGEST_FEEDBACK_EVENTS = "ingest-feedback-events"
INGEST_REPLAY_EVENTS = "ingest-replay-events"

# Event stream (Sentry -> Snuba)
EVENTS = "events"
TRANSACTIONS = "transactions"
EVENTSTREAM_GENERIC = "generic-events"
SNUBA_ITEMS = "snuba-items"

# Commit logs (for Snuba consumers)
EVENTS_COMMIT_LOG = "snuba-commit-log"
TRANSACTIONS_COMMIT_LOG = "snuba-transactions-commit-log"

# Alert subscription results (Snuba -> Sentry)
EVENTS_SUBSCRIPTIONS_RESULTS = "events-subscription-results"
TRANSACTIONS_SUBSCRIPTIONS_RESULTS = "transactions-subscription-results"

# Metrics
SNUBA_METRICS = "snuba-metrics"
SNUBA_GENERIC_METRICS = "snuba-generic-metrics"
```

### Relay (Edge Proxy)

Relay is a **Rust-based proxy** that sits in front of Sentry:
- Receives events from SDKs via HTTP
- Validates DSN authentication
- Applies rate limiting and quotas
- Performs PII scrubbing / data scrubbing (using relay's PII processor)
- Filters events (inbound filters)
- Normalizes events (same Rust normalizer used by EventManager)
- Produces events to Kafka for async processing

### Rate Limiting and Quotas

- Quotas are checked at multiple layers: Relay (fast path), ingest consumers, save_event
- Kill switches can disable processing for specific projects/organizations
- Circuit breakers protect against cascade failures (e.g., Seer service)
- Dynamic sampling controls what percentage of transactions are kept
- Backpressure management via health checks in Arroyo consumers

### Source Map Processing

- Source maps are uploaded as **artifact bundles** (or legacy release files)
- The **Symbolicator** service (Rust) handles source map lookup and application
- During event processing, if stacktraces need symbolication, the event is routed through `process_event` which calls Symbolicator
- Symbolicator resolves minified frames to original source locations using uploaded source maps
- Results are cached in Redis

---

## 6. Query Patterns / Snuba

### How Snuba Works

1. **Ingestion:** Snuba consumers read from Kafka topics and write to ClickHouse tables
2. **Query API:** Sentry constructs queries using `snuba_sdk` (Python SDK for Snuba's query language)
3. **Translation:** Snuba translates structured queries into ClickHouse SQL
4. **Subscriptions:** For alerting, Snuba runs periodic queries and publishes results

### Issue List Queries

When querying the issue list, Sentry:
1. Queries PostgreSQL for Group metadata (status, substatus, priority, etc.)
2. Uses ClickHouse (via Snuba) for event-level aggregations (event count over time, latest event data)
3. Joins are NOT done across databases -- instead, Group IDs from PostgreSQL are used to filter ClickHouse queries

```python
# Example: Get events for a group
events = eventstore.backend.get_events_snql(
    organization_id=org_id,
    group_id=group.id,
    conditions=[
        Condition(Column("project_id"), Op.IN, [project.id]),
        Condition(Column("group_id"), Op.IN, [group.id]),
    ],
    limit=1,
    orderby=["-timestamp", "-id"],
    dataset=Dataset.Events,
)
```

### SnubaQuery Model (for Alerts)

```python
class SnubaQuery(Model):
    environment = FlexibleForeignKey("sentry.Environment")
    type = models.SmallIntegerField()     # ERROR, PERFORMANCE, CRASH_RATE
    dataset = models.TextField()           # Which Snuba dataset to query
    query = models.TextField()             # The filter expression
    aggregate = models.TextField()         # e.g., "count()", "p95(transaction.duration)"
    time_window = models.IntegerField()    # Window in seconds
    resolution = models.IntegerField()     # How often to check
```

### Pre-aggregations and Materialized Views

- **Outcomes** dataset uses materialized views of raw outcomes data
- **Metrics** use AggregatingMergeTree for pre-aggregated metric data
- **Release health** metrics are separate from general metrics
- Time-bucketed columns (`time`, `bucketed_end`) enable efficient time-series queries

---

## 7. Architecture

### Services

| Service | Language | Role |
|---|---|---|
| **Web** | Python/Django | HTTP API, UI serving |
| **Worker** | Python/Celery | Async task processing (save events, send notifications, etc.) |
| **Cron** | Python/Celery Beat | Scheduled tasks |
| **Relay** | Rust | Edge proxy, event ingestion, PII scrubbing |
| **Snuba** | Python | ClickHouse query layer, event consumer |
| **Symbolicator** | Rust | Source map and native symbol processing |
| **Ingest Consumer** | Python/Arroyo | Kafka consumers for event ingestion |
| **Post-process Consumer** | Python | Post-processing (alerts, integrations) |
| **TaskBroker** | -- | Async task processing (newer alternative to Celery) |
| **ClickHouse** | C++ | Columnar analytics database |
| **PostgreSQL** | -- | Primary relational database |
| **Redis** | -- | Cache, rate limiting, buffers |
| **Kafka** | -- | Message queue |
| **Bigtable** (optional) | -- | NodeStore for event payloads |

### Communication Patterns

```
SDK -> Relay (HTTP)
Relay -> Kafka (produce events)
Kafka -> Ingest Consumer (consume)
Ingest Consumer -> Celery (task dispatch)
Celery -> PostgreSQL (group/release/etc.)
Celery -> NodeStore (event payload storage)
Celery -> Kafka (eventstream publish)
Kafka -> Snuba (consume events)
Snuba -> ClickHouse (write events)
Web -> PostgreSQL (read groups, users, etc.)
Web -> Snuba -> ClickHouse (read event data)
Snuba -> Kafka (subscription results)
Kafka -> Alert Worker (process subscription results)
```

### Self-Hosted Deployment

Self-hosted runs everything via Docker Compose:
- All services containerized
- PostgreSQL, Redis, Kafka, ClickHouse, Snuba all run as containers
- Relay runs as the entry point for SDK traffic
- File storage defaults to filesystem (`/data/files`)
- Configuration via `config.yml` and `sentry.conf.py`

### Silo Architecture

Sentry uses a "silo" model for its SaaS deployment:
- **Control Silo:** User authentication, billing, organization management
- **Region/Cell Silo:** Project data, events, issues (region-specific)
- Cross-silo communication via outbox pattern
- Models are decorated with `@cell_silo_model` or `@control_silo_model`

---

## 8. SDK / Client Libraries

### Event Submission

SDKs submit events using the **envelope format** -- a binary protocol that can bundle multiple items (events, attachments, sessions, etc.) in a single HTTP request.

```
POST /api/{project_id}/envelope/
Content-Type: application/x-sentry-envelope
X-Sentry-Auth: Sentry sentry_key=<DSN_PUBLIC_KEY>

{event_id, ...}          <- envelope header (JSON line)
{type: event, ...}       <- item header (JSON line)
{...event payload...}    <- item body (JSON)
```

### What the SDK Captures

- **Stack traces:** Full call stack with filename, function, module, line/column numbers, context lines, in-app flag
- **Breadcrumbs:** Chronological trail of events (console logs, HTTP requests, UI clicks, navigation)
- **Contexts:** Device info, OS, browser, runtime, app version, GPU
- **Tags:** Key-value pairs for filtering (custom + auto-collected)
- **User:** ID, email, username, IP address
- **Request data:** HTTP method, URL, headers, query string, body
- **SDK metadata:** SDK name, version

### Transport Layer

- **Batching:** Envelopes can contain multiple items
- **Retry:** SDKs implement retry with backoff
- **Rate limiting:** SDKs respect `429 Too Many Requests` with `Retry-After` headers
- **Offline caching:** Some SDKs cache events locally when offline

### Performance Monitoring in SDKs

- **Transactions:** Represent a unit of work (e.g., HTTP request, page load)
- **Spans:** Children of transactions representing sub-operations (DB queries, HTTP calls, rendering)
- **Trace context:** Distributed tracing headers (`sentry-trace`, `baggage`) propagated across services
- **Measurements:** Web Vitals and custom measurements attached to transactions

---

## 9. Release Tracking

### How Releases Work

1. **Creation:** Release created via API or automatically when SDK reports a new version
2. **Commit association:** Releases linked to commits via `ReleaseCommit` model
3. **Deploy tracking:** Deploys recorded with environment and timestamp
4. **First seen in release:** Each Group tracks `first_release`

### Regression Detection

```python
# From event_manager.py
class GroupInfo:
    group: Group
    is_new: bool         # First time this group has been seen
    is_regression: bool  # Was resolved, now re-appeared
```

Regression detection flow:
1. Event assigned to a group
2. If group status is `RESOLVED` and event's release is newer than the resolving release -> **REGRESSION**
3. If group was resolved via "resolve in next release" and the current release is that next release -> check if actually fixed
4. Commit-based resolution: checks if the resolving commit has been deployed

### Commit Linking

- Releases track `last_commit_id` and `commit_count`
- `ReleaseCommit` links individual commits to releases
- `CommitAuthor` tracks who made each commit
- Integration with GitHub/GitLab/Bitbucket for automatic commit association

---

## 10. Alerts and Notifications

### Alert System Architecture

**File:** `src/sentry/incidents/`

Two alert types:

#### 1. Metric Alerts (Incidents System)

```python
class AlertRule(Model):
    organization = FlexibleForeignKey("sentry.Organization")
    snuba_query = FlexibleForeignKey("sentry.SnubaQuery")  # The query to run
    name = models.TextField()
    status = models.SmallIntegerField()      # PENDING, TRIGGERED, etc.
    threshold_type = models.SmallIntegerField()  # ABOVE, BELOW
    resolve_threshold = models.FloatField()
    threshold_period = models.IntegerField()  # How many times must exceed
    comparison_delta = models.IntegerField()  # Time window for comparison
    detection_type = models.CharField()       # STATIC, DYNAMIC (anomaly)
    sensitivity = models.CharField()          # For anomaly detection
    seasonality = models.CharField()          # For anomaly detection

class AlertRuleTrigger(Model):
    alert_rule = FlexibleForeignKey("sentry.AlertRule")
    label = models.TextField()               # "critical", "warning"
    threshold_type = models.SmallIntegerField()
    alert_threshold = models.FloatField()
    resolve_threshold = models.FloatField()
```

Flow:
1. AlertRule defines a SnubaQuery (what to measure)
2. Snuba runs the query periodically based on `time_window` and `resolution`
3. Results published to Kafka subscription results topic
4. `subscription_processor.py` evaluates results against AlertRuleTrigger thresholds
5. If threshold exceeded -> create/update Incident
6. Trigger AlertRuleTriggerActions (send Slack, PagerDuty, email, etc.)

#### 2. Issue Alerts (Rules System)

**File:** `src/sentry/rules/`

Rule-based system with conditions and actions:
- Conditions: "An event is seen", "An event's tags match", "Event frequency exceeds N in M minutes"
- Actions: "Send notification to Slack", "Create Jira ticket", "Send email"
- Evaluated in the post-process pipeline when events are saved

### Incident Model

```python
class Incident(Model):
    organization = FlexibleForeignKey("sentry.Organization")
    alert_rule = FlexibleForeignKey("sentry.AlertRule")
    status = models.PositiveSmallIntegerField()  # OPEN, CLOSED, WARNING, CRITICAL
    type = models.PositiveSmallIntegerField()
    title = models.TextField()
    date_started = models.DateTimeField()
    date_closed = models.DateTimeField()
    date_detected = models.DateTimeField()
```

---

## 11. Privacy / Compliance

### Data Scrubbing (PII Stripping)

**File:** `src/sentry/relay/datascrubbing.py`

Data scrubbing happens at **two layers**:

1. **Relay (edge):** Before event enters the pipeline, Relay applies PII scrubbing using Rust-based `pii_strip_event`
2. **Store task (Python):** As a fallback, `scrub_data()` applies PII rules again

Configuration is hierarchical:
- Organization-level PII config (takes precedence)
- Project-level PII config
- Organization/project options for scrub toggles

```python
# Settings per org/project
scrubData: bool          # Enable scrubbing
scrubIpAddresses: bool   # Strip IP addresses
sensitiveFields: list    # Additional fields to scrub
excludeFields: list      # Fields to never scrub ("safe fields")
scrubDefaults: bool      # Use default scrubbing rules
```

### Data Retention

- Events have configurable retention periods (90 days default for self-hosted, varies by plan for SaaS)
- `Group.data` tracks retention-related metadata
- NodeStore entries (event payloads) are TTL'd
- ClickHouse tables use TTL-based expiration

---

## 12. API Design

### Event Submission

**DSN Format:** `https://<public_key>@<host>/<project_id>`

**Endpoints:**
```
POST /api/<project_id>/store/          -- Legacy store endpoint
POST /api/<project_id>/envelope/       -- Modern envelope endpoint
POST /api/<project_id>/minidump/       -- Native crash reports
POST /api/<project_id>/security/       -- CSP/security reports
POST /api/<project_id>/unreal/<hash>/  -- Unreal Engine crashes
```

**Authentication:** DSN public key in `X-Sentry-Auth` header or `sentry_key` query parameter.

### Query APIs

RESTful APIs under `/api/0/`:
```
GET  /api/0/organizations/{org}/issues/          -- List issues
GET  /api/0/issues/{issue_id}/events/            -- Issue events
GET  /api/0/organizations/{org}/events/          -- Discover query
POST /api/0/organizations/{org}/releases/        -- Create release
GET  /api/0/projects/{org}/{project}/            -- Project details
```

**Pagination:** Cursor-based using `cursor` parameter.

**Auth tokens:** Bearer tokens (`Authorization: Bearer <token>`) for API access. Scoped with permissions like `org:read`, `project:write`, `event:read`.

### API Design Conventions

- URL params: `snake_case`
- Request/response bodies: `camelCase`
- Numeric IDs returned as strings
- Error responses use `{"detail": "message"}` format
- Pagination via cursor

---

## 13. Strengths and Weaknesses

### Strengths

1. **Error grouping is genuinely sophisticated.** The multi-layered approach (strategies -> variants -> components -> hashes) with platform-specific normalization handles a huge range of real-world scenarios. The component tree architecture provides excellent explainability (users can see *why* events grouped together).

2. **The enhancer system is powerful.** Stack trace enhancement rules let users customize grouping without custom code. This solves the "framework noise" problem elegantly.

3. **Dual ClickHouse/PostgreSQL architecture is well-suited.** PostgreSQL for relational data + ClickHouse for high-volume analytics is the right split. Groups live in PG (low volume, relational), events live in ClickHouse (high volume, analytical).

4. **The ingestion pipeline is battle-tested.** Kafka-based with Relay as an edge proxy provides high throughput, backpressure management, and fault isolation.

5. **Fingerprinting is a great escape hatch.** When automatic grouping fails, users have a clean path to override it.

6. **The Seer AI integration for grouping is forward-thinking.** Using ML similarity for cases where deterministic grouping fails.

### Weaknesses / Over-Complexity

1. **The grouping system is over-engineered for 90% of use cases.** The app/system variant distinction, the legacy config transition system, the secondary hashing -- most of this complexity exists to handle migration between config versions. A new product doesn't need any of it.

2. **Too many databases.** PostgreSQL + ClickHouse + Redis + Kafka + NodeStore (Bigtable) is a LOT of infrastructure. This makes self-hosting painful and is the primary reason alternatives like Sentry's own self-hosted is complex to run.

3. **Django is showing its age.** The codebase has massive model files (group.py is 700+ lines of model definition alone), circular import workarounds everywhere, and the ORM's N+1 problems require constant attention.

4. **The silo architecture adds immense complexity.** Cross-silo communication via outboxes, hybrid cloud foreign keys, cell vs control endpoints -- this is SaaS-scale complexity that poisons the entire codebase.

5. **Snuba as a separate service is overhead.** For most deployments, querying ClickHouse directly would be simpler. Snuba adds another service to deploy, monitor, and maintain.

6. **252+ feature flags** in the temporary features file alone indicates massive configuration debt and ongoing feature experimentation that complicates the codebase.

### Design Decisions Worth Learning From

1. **Hash = MD5 of contributing values.** Simple, deterministic, fast. No need for anything fancier.
2. **GroupHash as the mapping table.** Clean separation of "what hash an event produces" from "which group that hash belongs to." Enables merge/unmerge operations.
3. **Component tree for explainability.** The tree structure lets you show users exactly which parts of their event contributed to grouping and why.
4. **Server-side fingerprinting rules as a DSL.** Gives power users control without code changes.
5. **Relay as a separate Rust service.** Offloads CPU-intensive normalization and PII scrubbing from Python.
6. **Envelope format.** Efficient wire format for batching multiple types of data in one request.

### What to Simplify for Teploy Observe

1. **Single database.** Start with PostgreSQL for everything. Add ClickHouse only when you need time-series analytics at scale. For small-to-medium deployments, PG with proper indexing and partitioning handles event queries fine.

2. **Skip variants.** Don't implement app/system variants. Just use in-app frames. If all frames are system frames, use all frames.

3. **Skip config versioning/transitions.** Ship one grouping algorithm. When you change it, let users manually re-process or accept the split.

4. **Skip Snuba.** Query ClickHouse directly. Or query PostgreSQL directly if you're not at ClickHouse scale.

5. **Skip Relay initially.** Accept events directly into the API. Add an edge proxy later for scale.

6. **Simple fingerprinting.** Support `fingerprint` on events and basic server-side rules. Skip the DSL initially.

7. **Minimum viable grouping:**
   - Hash = MD5 of (error_type + in_app_frame_filenames + in_app_frame_functions)
   - Fallback: MD5 of parameterized message
   - Override: user-set fingerprint
   - That's it. Add sophistication based on real user feedback.
