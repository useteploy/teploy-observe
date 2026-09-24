/**
 * @teploy/observe-browser — Observe's browser SDK.
 *
 * ```ts
 * import { init } from "@teploy/observe-browser";
 *
 * init({
 *   endpoint: "https://observe.example.com",
 *   siteId: "my-site",
 *   // Ingestion requires a site-scoped API key (create one under
 *   // Settings > API keys; browser keys are public by design and can only
 *   // append telemetry to their own site).
 *   apiKey: "pk_site_scoped_ingest_key",
 * });
 * ```
 *
 * For frameworks that can't use script tags (SPA routes, CSP, etc.) this
 * module takes the place of the `<script src="/observe.js">` snippet. It
 * sends the same payloads to the same ingestion endpoints.
 */

export interface InitOptions {
  /** Base URL of your Observe deployment, e.g. `https://observe.example.com`. */
  endpoint: string;
  /** The site identifier. Defaults to `"default"`. */
  siteId?: string;
  /** API key for authenticated ingest. Required: the server's ingest
   * routes reject keyless requests (a browser-visible key is public by
   * design — scope it to one site and append-only telemetry). */
  apiKey?: string;
  /** Disable automatic pageview on init (useful for SPAs doing manual routing). */
  disableAutoPageview?: boolean;
  /** Max events buffered before an automatic flush. Default: 50. */
  batchSize?: number;
  /** Flush cadence in ms when the buffer isn't full. Default: 2000. */
  flushIntervalMs?: number;
  /** Application release tag (git SHA, semver, etc.). Stamped on every
   * event and error so the /releases UI can surface crash-free %, error
   * rate, and adoption per release. */
  release?: string;
  /** Environment label stamped on captured errors ("production",
   * "staging", ...). Defaults to "production"; captureException's context
   * can override per-call. */
  environment?: string;
  /** Called when a batch or report cannot be delivered (non-2xx response,
   * network failure, payload rejected as oversized). The SDK never throws
   * into application code, so this is the only drop signal. */
  onError?: (err: Error) => void;
  /** Max automatic retry attempts per batch before it is dropped and
   * reported through onError (audit F32's retry budget). Default: 5. */
  maxRetryAttempts?: number;
  /** Base delay for the retry backoff after a failed send; doubles per
   * attempt up to 60 s (audit F32). Default: 1000 ms. */
  retryBackoffMs?: number;
  /** Visibility hook for the automatic retry policy (audit F32): fires
   * each time a failed batch is scheduled for a retry, before the backoff
   * sleep. Never throws into application code. */
  onRetry?: (info: { attempt: number; delayMs: number; batchSize: number; error: Error }) => void;
  /** Per-request deadline for flush sends (TO-034 made the deadline
   * exist; O11 makes it configurable so slow-endpoint behavior is
   * testable and tunable). Default: 10000 ms; clamped to [250, 600000]. */
  requestTimeoutMs?: number;
}

export interface EventPayload {
  site_id: string;
  event_type: string;
  /** Producer-assigned stable identity (F12): generated once when the event
   * is recorded, kept through requeue and retry so the server can dedupe a
   * redelivered batch at flush. */
  event_id?: string;
  /** Page URL as origin+path (F41): query, fragment, and credentials never
   * leave the browser from this SDK. */
  url?: string;
  referrer?: string;
  title?: string;
  /** Hashed user identifier set by identify(). Empty for anonymous events. */
  distinct_id?: string;
  /** Application release tag from init({ release }). Empty if not set. */
  release?: string;
  /** Allowlisted campaign params extracted from the current query string
   * (F41): attribution rides these explicit fields; the server's analytics
   * read them, not the raw query. */
  utm_source?: string;
  utm_medium?: string;
  utm_campaign?: string;
  utm_term?: string;
  utm_content?: string;
  /** Custom event properties. The server only reads properties from here —
   * anything spread at the top level of the payload is not a field it stores. */
  properties?: Record<string, unknown>;
  [key: string]: unknown;
}

/** Wire protocol version this SDK speaks (F12/F19 idempotent batches). */
export const PROTOCOL_VERSION = 2;

export interface ErrorPayload {
  site_id: string;
  /** Producer-stable error identity — the server's dedupe key (O01 §5.6).
   * Minted per capture so a retried submission dedupes server-side. */
  event_id?: string;
  error_type: string;
  error_value: string;
  stack_trace?: Array<{ filename: string; function: string; lineno: number; colno: number; in_app?: boolean }>;
  release_tag?: string;
  environment?: string;
  /** How the error was captured ("onerror", "onunhandledrejection", "manual"). */
  mechanism?: string;
  url?: string;
  breadcrumbs?: unknown[];
  level?: "error" | "warning" | "info";
  /** Active session-replay id, if observe-replay.js is loaded on the page. */
  replay_id?: string;
  /** User identifier set by identify(). Hashed server-side with site salt. */
  distinct_id?: string;
  /** Active trace context, when the error was captured inside a traced
   * operation. Enables exact trace<->error correlation server-side. */
  trace_id?: string;
  span_id?: string;
  /** Extra structured context. captureException's tags ride here under
   * `tags` — the server has no top-level tags field. */
  extra?: Record<string, unknown>;
}

export interface LogPayload {
  site_id: string;
  level: "debug" | "info" | "warn" | "error" | "fatal";
  message: string;
  service_name?: string;
  trace_id?: string;
  span_id?: string;
  attributes?: Record<string, unknown>;
}

interface Client {
  opts: Required<Pick<InitOptions, "endpoint" | "siteId" | "disableAutoPageview" | "batchSize" | "flushIntervalMs" | "maxRetryAttempts" | "retryBackoffMs" | "requestTimeoutMs">> & {
    apiKey?: string;
    release?: string;
    environment?: string;
    onError?: (err: Error) => void;
    onRetry?: (info: { attempt: number; delayMs: number; batchSize: number; error: Error }) => void;
  };
  /** Newly recorded events not yet packed into a request (TO-032). Each
   * entry is an owned JSON snapshot taken at admission (TO-033). */
  buffer: string[];
  /** Serialized bytes reserved by buffer + pending (TO-034). */
  queuedBytes: number;
  /** Frozen, identity-stamped request envelopes awaiting first send or
   * retry (TO-032). A request retries its exact bytes; its events are
   * never merged back into the live buffer. */
  pending: PendingEventRequest[];
  timer: number | null;
  userId: string | null;
  sessionId: string | null;
  /** F12: stable producer identity for this SDK instance; part of every
   * batch envelope so the server can dedupe per-producer batch ids. */
  producerId: string;
  /** Single-flight flush owner (AUD-022): overlapping flushes used to send
   * duplicate prefixes and then slice differing lengths off a mutated
   * queue, deleting events neither request had sent. */
  flushing: Promise<void> | null;
  /** F32: retry attempts per pending request, keyed by batch id. */
  retryCounts: Map<string, number>;
  /** F32: earliest wall-clock time (ms) at which a flush may send again —
   * the retry backoff gate. Set after each failed attempt. */
  retryAfter: number;
  /** O11: visible loss/delivery counters for THIS client, surfaced via
   * getStats(). Drops are never silent: every counter increment also
   * reports through onError. */
  stats: Stats;
  /** Removes this client's interval and DOM listeners (audit F31). */
  dispose: () => void;
}

/**
 * O11 diagnostics snapshot. `dropped` counts LOST events by reason; the
 * sum of its values plus `deliveredEvents` plus whatever is still queued
 * (see `queued`) accounts for every event this SDK admitted. `undeliveredAtUnload`
 * is a point-in-time snapshot taken when the page started unloading.
 */
export interface Stats {
  /** Batches the server acknowledged (accepted >= 0 events). */
  deliveredBatches: number;
  /** Events the server accepted. */
  deliveredEvents: number;
  /** Retry attempts made (a batch may be retried several times). */
  retries: number;
  /** Lost events by reason: admission_unserializable, admission_oversize,
   * queue_full (drop-newest), pack_oversize, retry_exhausted,
   * server_rejected, retention_cap, reinit_drain_failed. */
  dropped: Record<string, number>;
  /** Events reported undelivered at the moment the page began unloading
   * (pagehide/hidden). The keepalive flush may still deliver some; this
  * snapshot is the honest upper bound visible before the page died. */
  undeliveredAtUnload: number;
  /** Events currently waiting in the live buffer + frozen pending requests. */
  queued: number;
  /** This client instance's producer id (correlates with server-side logs). */
  producerId: string;
}

function newStats(producerId: string): Stats {
  return {
    deliveredBatches: 0,
    deliveredEvents: 0,
    retries: 0,
    dropped: {},
    undeliveredAtUnload: 0,
    queued: 0,
    producerId,
  };
}

/** O11: count a loss (never silent) — increments the visible counter and
 * reports through onError so an unset hook cannot hide the drop either. */
function countDrop(target: Client, reason: string, events: number, detail?: string): void {
  target.stats.dropped[reason] = (target.stats.dropped[reason] ?? 0) + events;
  reportError(target, new Error(
    `observe: lost ${events} event(s) (${reason})${detail ? ` — ${detail}` : ""}`));
}

/**
 * TO-032: one immutable request envelope. Identity (producer, batch) is
 * allocated once, the body serialized once, and a retry resends the exact
 * same bytes — a failed batch can never be repacked with newly recorded
 * events under its already-acknowledged batch id (the old repack lost the
 * new tail whenever the server's admission cache recognized the id).
 */
interface PendingEventRequest {
  readonly producerId: string;
  readonly batchId: string;
  readonly body: string;
  readonly bytes: number;
  readonly eventCount: number;
}

let client: Client | null = null;

function makeId(): string {
  const arr = new Uint8Array(16);
  (globalThis.crypto ?? (globalThis as any).msCrypto).getRandomValues(arr);
  return Array.from(arr, (b) => b.toString(16).padStart(2, "0")).join("");
}

// Server-side caps the SDK must respect (audit F32): the events batch
// endpoint rejects >100 events per request, and a keepalive body plus any
// other in-flight keepalive traffic must stay under the browser's 64 KiB
// budget — 48 KiB is a conservative per-request allowance.
const MAX_EVENTS_PER_REQUEST = 100;
const MAX_KEEPALIVE_BYTES = 48 * 1024;

// AUD-026 (round 2): batches are packed by ENCODED BYTES as well as event
// count — a count-only cap let a batch of large events exceed the server's
// 2 MiB body limit (or the keepalive budget) and be rejected whole. 1 MiB
// leaves room for envelope overhead under the route cap.
const MAX_REQUEST_BYTES = 1024 * 1024;

// TO-033/TO-034: admission-time budgets. Every queued record carries its
// serialized bytes, and the queued+pending total is capped in BOTH count
// and bytes — during retry backoff or a hanging request the buffer can no
// longer grow without limit (the old MAX_BUFFERED_ON_ERROR only ran after
// a send had already failed).
const MAX_QUEUED_EVENTS = 200;
const MAX_QUEUED_BYTES = 8 * 1024 * 1024;

// TO-035: a conservative per-event admission cap under the server's 64 KiB
// stored-event limit, so a record that can never be accepted is reported
// at admission instead of poisoning every request that packs it.
const MAX_EVENT_BYTES = 60 * 1024;

// Retained requests on a failed send (audit F32): keep the batch for the next
// flush instead of silently erasing it, but bound the retention so a long
// outage cannot grow memory without limit. Beyond the cap, the OLDEST
// pending requests are dropped and reported (they carry the oldest data).
const MAX_PENDING_REQUESTS = 200;

// F32 retry policy: the backoff doubles per attempt up to this ceiling.
const MAX_RETRY_DELAY_MS = 60_000;

// TO-034: every flush-network request carries a deadline; a hanging fetch
// must not pin the single flush owner forever while the buffer grows. The
// default is 10 s; init({ requestTimeoutMs }) overrides (O11, clamped).
const FETCH_DEADLINE_MS = 10_000;

const textEncoder = new TextEncoder();

// F41 URL contract: what leaves the browser is origin+path only. Campaign
// attribution rides the explicit utm_* fields extracted from the
// allowlisted query params — the raw query string never leaves the page.
const UTM_KEYS = ["utm_source", "utm_medium", "utm_campaign", "utm_term", "utm_content"] as const;

function pageUrl(): string {
  if (typeof location === "undefined") return "";
  return location.origin + location.pathname;
}

function campaignFields(): Partial<Record<(typeof UTM_KEYS)[number], string>> {
  const out: Partial<Record<(typeof UTM_KEYS)[number], string>> = {};
  if (typeof location === "undefined" || !location.search) return out;
  try {
    const params = new URLSearchParams(location.search);
    for (const key of UTM_KEYS) {
      const v = params.get(key);
      if (v) out[key] = v.slice(0, 256);
    }
  } catch {
    /* a malformed search string carries no attribution */
  }
  return out;
}

/** Serialize the v2 batch envelope around already-serialized events.
 * The producer id is ALWAYS the owning client's (TO-036): the old global
 * fallback let an old client's in-flight flush adopt a replacement
 * client's producer identity after re-init. */
function batchEnvelopeJSON(producerId: string, batchId: string, events: string[]): string {
  return `{"v":${PROTOCOL_VERSION},"producer_id":${JSON.stringify(producerId)},"batch_id":${JSON.stringify(batchId)},"events":[${events.join(",")}]}`;
}

/** Freeze buffer entries into request-legal envelopes by count and encoded
 * bytes (AUD-026/TO-032). Every emitted request is complete and immutable;
 * entries that cannot fit any envelope are reported and dropped (they were
 * already rejected at admission, so this is a belt-and-braces re-check). */
function packEventRequests(
  producerId: string,
  entries: string[],
  onDrop: (why: string) => void,
): PendingEventRequest[] {
  const out: PendingEventRequest[] = [];
  let batch: string[] = [];
  let bytes = 0;
  const flushBatch = () => {
    if (!batch.length) return;
    const batchId = makeId();
    const body = batchEnvelopeJSON(producerId, batchId, batch);
    out.push({
      producerId,
      batchId,
      body,
      bytes: textEncoder.encode(body).byteLength,
      eventCount: batch.length,
    });
    batch = [];
    bytes = 0;
  };
  for (const raw of entries) {
    const rawBytes = textEncoder.encode(raw).byteLength;
    const solo = textEncoder.encode(batchEnvelopeJSON(producerId, "probe", [raw])).byteLength;
    if (solo > MAX_REQUEST_BYTES) {
      onDrop(`dropped an event exceeding the request byte budget (${solo} > ${MAX_REQUEST_BYTES})`);
      continue;
    }
    const envelopeOverhead = solo - rawBytes;
    if (
      batch.length >= MAX_EVENTS_PER_REQUEST ||
      (batch.length > 0 && bytes + rawBytes + envelopeOverhead > MAX_REQUEST_BYTES)
    ) {
      flushBatch();
    }
    bytes += rawBytes;
    batch.push(raw);
  }
  flushBatch();
  return out;
}

/** fetch with a hard deadline (TO-034) and redirect refusal (TO-037: a
 * redirect would forward the X-API-Key credential to another origin). */
async function fetchWithDeadline(url: string, init: RequestInit, deadlineMs: number = FETCH_DEADLINE_MS): Promise<Response> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), deadlineMs);
  try {
    return await fetch(url, { ...init, redirect: "error", signal: controller.signal });
  } finally {
    clearTimeout(timer);
  }
}

/**
 * Send one pre-serialized JSON body. STRICT (AUD-021/TO-037): rejects on
 * any transport failure (non-2xx, network error, redirect) and carries a
 * request deadline so a hanging fetch cannot pin the flush owner (TO-034).
 * The flush owner consumes rejections to drive retention; the
 * fire-and-forget public boundaries (captureException, log, unload sends)
 * report them instead. `keepalive` is only used for small final sends
 * while the page is unloading.
 */
function sendRawJSON(target: Client, path: string, raw: string, unloading = false): Promise<void> {
  const url = target.opts.endpoint.replace(/\/+$/, "") + path;
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (target.opts.apiKey) headers["X-API-Key"] = target.opts.apiKey;
  const bytes = textEncoder.encode(raw).byteLength;
  return fetchWithDeadline(url, {
    method: "POST",
    headers,
    body: raw,
    credentials: "omit",
    keepalive: unloading && bytes <= MAX_KEEPALIVE_BYTES,
  }, target.opts.requestTimeoutMs).then((res) => {
    if (!res.ok) throw new Error(`observe: ingest returned ${res.status}`);
  });
}

function sendJSON(target: Client, path: string, payload: unknown, unloading = false): Promise<void> {
  let raw: string;
  try {
    raw = JSON.stringify(payload);
  } catch (err) {
    return Promise.reject(err instanceof Error ? err : new Error("observe: payload not serializable"));
  }
  return sendRawJSON(target, path, raw, unloading).catch((err) => {
    reportError(target, err instanceof Error ? err : new Error(String(err)));
  });
}

/**
 * TO-035: read and validate the events-batch acknowledgment. HTTP success
 * alone is not acceptance — the server reports per-event rejections in
 * `rejected`; those surface through onError (the rejected events are gone
 * server-side; retrying would only duplicate the accepted neighbors).
 */
async function readEventBatchAck(res: Response): Promise<{ ok: boolean; accepted?: number; rejected?: number; deduped?: boolean }> {
  if (!res.ok) throw new Error(`observe: ingest returned ${res.status}`);
  const value: unknown = await res.json().catch(() => null);
  if (!value || typeof value !== "object" || (value as { ok?: unknown }).ok !== true) {
    throw new Error("observe: invalid batch acknowledgment");
  }
  return value as { ok: boolean; accepted?: number; rejected?: number; deduped?: boolean };
}

/** Route a transport failure to the configured hook without throwing into
 * application code (audit F32's drop signal). */
function reportError(target: Client, err: Error): void {
  try {
    target.opts.onError?.(err);
  } catch {
    /* an onError hook that throws must not break the SDK */
  }
}

/** Route a scheduled retry to the visibility hook (audit F32). */
function reportRetry(target: Client, info: { attempt: number; delayMs: number; batchSize: number; error: Error }): void {
  try {
    target.opts.onRetry?.(info);
  } catch {
    /* an onRetry hook that throws must not break the SDK */
  }
}

/** O11: the honest unload accounting. At pagehide the still-queued events
 * are the worst-case loss; record the snapshot visibly (onError + stats)
 * before the best-effort keepalive flush, because the page may die before
 * any delivery confirmation arrives. */
function recordUnloadSnapshot(target: Client): void {
  const queuedEvents = target.buffer.length + target.pending.reduce((n, r) => n + r.eventCount, 0);
  if (queuedEvents > 0) {
    target.stats.undeliveredAtUnload = queuedEvents;
    reportError(target, new Error(
      `observe: page unloading with ${queuedEvents} undelivered event(s) — attempting keepalive flush; this count is the loss upper bound`));
  }
}

/** Initialize the SDK. Re-init disposes the previous client's timer and
 * listeners and drains its queued work under the OLD configuration (audit
 * F31); previously each init() leaked one interval plus three listeners
 * and silently dropped everything the replaced client had buffered.
 * Numeric options are clamped to finite sane ranges (TO-034). */
export function init(options: InitOptions): void {
  if (!options.endpoint) throw new Error("@teploy/observe-browser: endpoint is required");

  const clamp = (v: number | undefined, dflt: number, min: number, max: number): number => {
    const n = typeof v === "number" && Number.isFinite(v) ? v : dflt;
    return Math.min(max, Math.max(min, n));
  };

  const previous = client;
  if (previous) {
    previous.dispose();
    // Best-effort final drain of the old client's work to the old endpoint
    // under the old key — never redirected to the new config (TO-036: the
    // old client's requests keep ITS producer identity). Frozen pending
    // requests resend their exact bytes (TO-032).
    drainClient(previous);
  }

  const instance: Client = {
    opts: {
      endpoint: options.endpoint,
      siteId: options.siteId ?? "default",
      apiKey: options.apiKey,
      disableAutoPageview: options.disableAutoPageview ?? false,
      batchSize: clamp(options.batchSize, 50, 1, MAX_EVENTS_PER_REQUEST),
      flushIntervalMs: clamp(options.flushIntervalMs, 2000, 250, 3_600_000),
      release: options.release,
      environment: options.environment,
      onError: options.onError,
      onRetry: options.onRetry,
      maxRetryAttempts: clamp(options.maxRetryAttempts, 5, 0, 50),
      retryBackoffMs: clamp(options.retryBackoffMs, 1000, 1, 60_000),
      requestTimeoutMs: clamp(options.requestTimeoutMs, 10_000, 250, 600_000),
    },
    buffer: [],
    queuedBytes: 0,
    pending: [],
    timer: null,
    userId: null,
    sessionId: makeId(),
    producerId: makeId(),
    flushing: null,
    retryCounts: new Map(),
    retryAfter: 0,
    stats: null as unknown as Stats,
    dispose: () => {},
  };
  instance.stats = newStats(instance.producerId);
  client = instance;

  if (typeof window !== "undefined") {
    // Listeners are bound to THIS client instance and removed on dispose
    // via an AbortController (audit F31).
    const controller = new AbortController();
    instance.dispose = () => {
      // The window global can vanish between init and dispose in tests and
      // embedded runtimes — teardown must not throw there.
      if (typeof window !== "undefined" && instance.timer !== null) window.clearInterval(instance.timer);
      instance.timer = null;
      controller.abort();
    };

    // Auto-flush on buffer age.
    instance.timer = window.setInterval(() => {
      void flushClient(instance);
    }, instance.opts.flushIntervalMs);

    // Flush when the tab is hidden (mobile app-switch, close).
    document.addEventListener(
      "visibilitychange",
      () => {
        if (document.visibilityState === "hidden") void flushClient(instance, true);
      },
      { signal: controller.signal },
    );

    // O11: pagehide is the last moment sync work is guaranteed to run —
    // snapshot the undelivered count (visible through onError + getStats)
    // and fire the keepalive flush. Anything still queued after this is
    // honestly reported as the unload loss, never swallowed.
    window.addEventListener(
      "pagehide",
      () => {
        recordUnloadSnapshot(instance);
        void flushClient(instance, true);
      },
      { signal: controller.signal },
    );

    // Autocapture: uncaught errors and promise rejections.
    window.addEventListener(
      "error",
      (e) => {
        void captureExceptionFor(instance, e.error ?? new Error(e.message), { mechanism: "onerror" });
      },
      { signal: controller.signal },
    );
    window.addEventListener(
      "unhandledrejection",
      (e: PromiseRejectionEvent) => {
        const reason: any = e.reason;
        void captureExceptionFor(instance, reason instanceof Error ? reason : new Error(String(reason)), {
          mechanism: "onunhandledrejection",
        });
      },
      { signal: controller.signal },
    );

    if (!instance.opts.disableAutoPageview) {
      pageview();
    }
  }
}

/**
 * Record a pageview for the current URL.
 *
 * `pathname` is only carried as a property: the server derives the stored
 * pathname from `url`, so an explicit one is an annotation, not a field.
 *
 * F41: url is origin+path; the allowlisted utm_* campaign params ride as
 * explicit fields. The full query string and fragment never leave the
 * browser.
 */
export function pageview(pathname?: string): void {
  if (!client) return;
  const props: Record<string, unknown> = {
    url: pageUrl(),
    referrer: typeof document !== "undefined" ? document.referrer : "",
    title: typeof document !== "undefined" ? document.title : "",
    ...campaignFields(),
  };
  if (pathname) props.pathname = pathname;
  track("pageview", props);
}

/**
 * Top-level payload keys the ingest endpoint actually reads. Anything else a
 * caller passes to track() is a custom property and must be nested under
 * `properties` — the server stores no other top-level field.
 */
const RESERVED_FIELDS = new Set([
  "site_id",
  "event_type",
  "event_id",
  "url",
  "referrer",
  "title",
  "language",
  "screen",
  "distinct_id",
  "release",
  // F41 explicit campaign fields.
  "utm_source",
  "utm_medium",
  "utm_campaign",
  "utm_term",
  "utm_content",
]);

/** Server-side cap on custom properties per event. Exceeding it is a 400. */
const MAX_PROPERTIES = 50;

/**
 * Record a custom event. Includes distinct_id when identify() has been called.
 *
 * Custom props are nested under `properties`; the handful of keys the server
 * reads as real fields (url, referrer, title, language, screen, distinct_id,
 * release) stay at the top level. Props beyond the server's 50-property cap
 * are dropped rather than letting the whole event be rejected.
 */
export function track(eventType: string, props: Record<string, unknown> = {}): void {
  const target = client;
  if (!target) return;
  const payload: Record<string, unknown> = {};
  const properties: Record<string, unknown> = {};
  for (const key of Object.keys(props)) {
    // SDK-owned identity fields are assigned LAST (below) so a caller
    // cannot override site/event/id routing through props (TO-033).
    if (key === "site_id" || key === "event_type" || key === "event_id") continue;
    if (RESERVED_FIELDS.has(key)) {
      payload[key] = props[key];
    } else if (Object.keys(properties).length < MAX_PROPERTIES) {
      properties[key] = props[key];
    }
  }
  if (Object.keys(properties).length > 0) payload.properties = properties;
  if (target.userId) payload.distinct_id = target.userId;
  if (target.opts.release) payload.release = target.opts.release;
  payload.site_id = target.opts.siteId;
  payload.event_type = eventType;
  payload.event_id = makeId();

  // TO-033: snapshot the record at admission. The queue owns immutable
  // bytes — caller mutations after track() (nested objects, arrays) can no
  // longer change the eventual body, and an unserializable record (BigInt,
  // circular) is isolated and reported here instead of poisoning the flush.
  let raw: string;
  try {
    raw = JSON.stringify(payload);
  } catch (err) {
    countDrop(target, "admission_unserializable", 1, err instanceof Error ? err.message : String(err));
    return;
  }
  const bytes = textEncoder.encode(raw).byteLength;
  if (bytes > MAX_EVENT_BYTES) {
    countDrop(target, "admission_oversize", 1, `${bytes} > ${MAX_EVENT_BYTES}`);
    return;
  }
  // TO-034: admission-time count+byte budget across queued and pending —
  // during retry backoff or a hanging request the buffer can no longer
  // grow without limit. Overflow drops the NEWEST record (the oldest data
  // is closest to delivery) and says so.
  if (target.buffer.length >= MAX_QUEUED_EVENTS || target.queuedBytes + bytes > MAX_QUEUED_BYTES) {
    countDrop(target, "queue_full", 1, "drop-newest overflow policy");
    return;
  }
  target.buffer.push(raw);
  target.queuedBytes += bytes;
  if (target.buffer.length >= target.opts.batchSize) flush();
}

/** Trait keys that duplicate the raw identity (audit F33). The ID travels
 * only in the top-level distinct_id field the server hashes; these keys
 * used to copy it verbatim into stored event properties. */
const FORBIDDEN_ID_TRAITS = new Set(["user_id", "distinct_id", "email"]);

/**
 * Associate subsequent events with a user id (and optional traits).
 *
 * Stores the userId locally and emits a one-shot `$identify` event so the
 * server can record the identification in the events stream. Traits ride
 * as event properties minus any identity-shaped key. Every subsequent
 * track()/captureException() call includes `distinct_id` in the payload —
 * the server hashes it with the per-site `session_salt` before storage,
 * matching the existing session-id privacy pattern.
 */
export function identify(userId: string, traits?: Record<string, unknown>): void {
  if (!client || !userId) return;
  client.userId = userId;
  const safe: Record<string, unknown> = {};
  if (traits) {
    for (const [key, value] of Object.entries(traits)) {
      if (FORBIDDEN_ID_TRAITS.has(key) || RESERVED_FIELDS.has(key)) continue;
      if (Object.keys(safe).length < MAX_PROPERTIES) safe[key] = value;
    }
  }
  track("$identify", safe);
}

/** Clear the current user — call on logout. */
export function reset(): void {
  if (!client) return;
  client.userId = null;
  client.sessionId = makeId();
}

/**
 * The current client-generated session id, or null before init(). Lets a
 * separately-loaded script (e.g. observe-replay.js, which tracks its own
 * session id via setSessionId()) correlate its events with this session
 * rather than defaulting to an empty session_id.
 */
export function getSessionId(): string | null {
  return client ? client.sessionId : null;
}

/** Capture context for error submissions. Every declared option affects
 * the outgoing payload (audit F51: mechanism and tags used to be accepted
 * and silently dropped, and the environment was hard-coded). */
export interface CaptureContext {
  mechanism?: string;
  release?: string;
  environment?: string;
  tags?: Record<string, string>;
  traceId?: string;
  spanId?: string;
}

/** Submit an error. Sends immediately — not buffered. Never rejects into
 * application code (AUD-021: the strict transport's rejection is reported
 * through onError instead). */
export function captureException(err: Error, ctx?: CaptureContext): Promise<void> {
  if (!client) return Promise.resolve();
  return captureExceptionFor(client, err, ctx);
}

function captureExceptionFor(target: Client, err: Error, ctx?: CaptureContext): Promise<void> {
  const payload: ErrorPayload = {
    site_id: target.opts.siteId,
    event_id: makeId(),
    error_type: err.name || "Error",
    error_value: err.message || String(err),
    release_tag: ctx?.release ?? target.opts.release,
    environment: ctx?.environment ?? target.opts.environment ?? "production",
    mechanism: ctx?.mechanism ?? "manual",
    url: pageUrl(),
    level: "error",
    stack_trace: parseStack(err.stack),
  };
  // Tags ride in the server-supported `extra` envelope (it has no
  // top-level tags field).
  if (ctx?.tags && Object.keys(ctx.tags).length > 0) {
    payload.extra = { tags: { ...ctx.tags } };
  }
  // Callers tracing their own operations can pass the active trace context
  // for exact trace<->error correlation in the trace detail view.
  if (ctx?.traceId) payload.trace_id = ctx.traceId;
  if (ctx?.spanId) payload.span_id = ctx.spanId;
  // If observe-replay.js is on the page, attach the active replay id so the
  // Errors UI can cross-jump to the session replay.
  const replayId = activeReplayId();
  if (replayId) payload.replay_id = replayId;
  if (target.userId) payload.distinct_id = target.userId;
  // Fire-and-forget public boundary (AUD-021): report, don't reject.
  return sendJSON(target, "/api/v1/errors", payload).catch((sendErr) => {
    reportError(target, sendErr instanceof Error ? sendErr : new Error(String(sendErr)));
  });
}

function activeReplayId(): string | null {
  if (typeof window === "undefined") return null;
  const r: any = (window as any).observeReplay;
  if (r && typeof r.getReplayId === "function") {
    try {
      const id = r.getReplayId();
      return typeof id === "string" && id ? id : null;
    } catch {
      return null;
    }
  }
  return null;
}

/** Submit a log entry. Fire-and-forget: failures are reported through
 * onError, never rejected into application code (AUD-021). */
export function log(entry: Omit<LogPayload, "site_id">): Promise<void> {
  const target = client;
  if (!target) return Promise.resolve();
  return sendJSON(target, "/api/v1/logs", { site_id: target.opts.siteId, ...entry })
    .catch((err) => {
      reportError(target, err instanceof Error ? err : new Error(String(err)));
    });
}

/** Split events into server-legal chunks by count and encoded bytes
 * (AUD-026). */
/** Release the queue reservation of a delivered/abandoned request (TO-034). */
function releaseRequest(target: Client, req: PendingEventRequest): void {
  target.queuedBytes -= req.bytes;
  if (target.queuedBytes < 0) target.queuedBytes = 0;
  target.retryCounts.delete(req.batchId);
}

/** Drop the OLDEST pending requests over the retention bounds, reporting
 * each. Called under the flush owner only. */
function trimPending(target: Client): void {
  let droppedRequests = 0;
  let droppedEvents = 0;
  while (target.pending.length > MAX_PENDING_REQUESTS || target.queuedBytes > MAX_QUEUED_BYTES) {
    const oldest = target.pending.shift();
    if (!oldest) break;
    releaseRequest(target, oldest);
    droppedRequests++;
    droppedEvents += oldest.eventCount;
  }
  if (droppedRequests > 0) {
    countDrop(target, "retention_cap", droppedEvents, `${droppedRequests} oldest pending request(s), drop-oldest policy`);
  }
}

/** Send one frozen request and consume its acknowledgment. Returns the
 * ack on acceptance (including partial rejections, which are counted and
 * reported but NOT retried — the rejected events are gone server-side and
 * the accepted neighbors must not be resent). */
async function deliverRequest(target: Client, req: PendingEventRequest, unloading: boolean): Promise<{ ok: boolean; accepted?: number; rejected?: number; deduped?: boolean }> {
  const res = await fetchWithDeadline(
    target.opts.endpoint.replace(/\/+$/, "") + "/api/v1/events/batch",
    requestInitFor(target, req.body, unloading),
    target.opts.requestTimeoutMs,
  );
  const ack = await readEventBatchAck(res);
  target.stats.deliveredBatches++;
  const rejected = ack.rejected ?? 0;
  target.stats.deliveredEvents += Math.max(req.eventCount - rejected, 0);
  if (rejected > 0) {
    countDrop(target, "server_rejected", rejected,
      `server accepted ${ack.accepted ?? req.eventCount - rejected} of ${req.eventCount}`);
  }
  return ack;
}

/** Build the fetch init for a frozen body. */
function requestInitFor(target: Client, body: string, unloading: boolean): RequestInit {
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (target.opts.apiKey) headers["X-API-Key"] = target.opts.apiKey;
  return {
    method: "POST",
    headers,
    body,
    credentials: "omit",
    keepalive: unloading && textEncoder.encode(body).byteLength <= MAX_KEEPALIVE_BYTES,
  };
}

/** Best-effort final drain of a replaced client's work (TO-036): pending
 * requests resend their exact bytes under their own producer identity;
 * buffered records are frozen into fresh requests. Failures are counted
 * as visible losses and reported, never retried (the replacement owns the
 * timer from here on). */
function drainClient(target: Client): void {
  const requests = [...target.pending];
  target.pending = [];
  if (target.buffer.length > 0) {
    const entries = target.buffer.splice(0);
    requests.push(...packEventRequests(target.producerId, entries, (why) => {
      countDrop(target, "pack_oversize", 1, `${why} on re-init`);
    }));
  }
  for (const req of requests) {
    deliverRequest(target, req, true)
      .then(() => releaseRequest(target, req))
      .catch((err) => {
        releaseRequest(target, req);
        countDrop(target, "reinit_drain_failed", req.eventCount,
          err instanceof Error ? err.message : String(err));
      });
  }
}

/** Flush a specific client's queue.
 *
 * AUD-022 (round 2): ONE flush owner per client.
 *
 * TO-032: delivery units are FROZEN request envelopes. A request keeps its
 * identity and exact bytes across every retry — a failed batch is never
 * merged back into the live buffer, so a retry can never reuse an
 * already-acknowledged batch id for different content (the old repack
 * lost the newly queued tail whenever the server recognized the id).
 *
 * F32 retry policy: a failed request is re-sent automatically with a
 * doubling backoff (retryAfter gate below) until maxRetryAttempts, then
 * dropped with an onError report. Retries keep their producer/batch ids
 * (F12), so the server dedupes any redelivery. */
function flushClient(target: Client, unloading = false): Promise<void> {
  if (target.flushing) return target.flushing;
  // Backoff gate: during the retry sleep, wakeups (timer, visibility,
  // explicit flush()) do not send. The unload path is exempt — it is a
  // last-gasp best effort with nothing left to wait for.
  if (!unloading && Date.now() < target.retryAfter) return Promise.resolve();
  const work = Promise.resolve().then(async () => {
    for (let units = 0; units < 8; units++) {
      // Freeze newly recorded records into requests first: retries of
      // older requests keep FIFO order ahead of them.
      if (target.buffer.length > 0) {
        const entries = target.buffer.splice(0);
        target.queuedBytes -= entries.reduce((n, raw) => n + textEncoder.encode(raw).byteLength, 0);
        if (target.queuedBytes < 0) target.queuedBytes = 0;
        const fresh = packEventRequests(target.producerId, entries, (why) => {
          countDrop(target, "pack_oversize", 1, why);
        });
        target.pending.push(...fresh);
        trimPending(target);
      }
      const req = target.pending.shift();
      if (!req) return;
      try {
        await deliverRequest(target, req, unloading);
        releaseRequest(target, req);
      } catch (err) {
        const sendErr = err instanceof Error ? err : new Error(String(err));
        const attempts = (target.retryCounts.get(req.batchId) ?? 0) + 1;
        if (attempts > target.opts.maxRetryAttempts) {
          // Retry budget exhausted: give up on THIS request only. Its
          // reservation is released; later requests keep their turn.
          releaseRequest(target, req);
          countDrop(target, "retry_exhausted", req.eventCount,
            `${target.opts.maxRetryAttempts} attempts, last error: ${sendErr.message}`);
        } else {
          target.retryCounts.set(req.batchId, attempts);
          target.stats.retries++;
          const delayMs = Math.min(target.opts.retryBackoffMs * 2 ** (attempts - 1), MAX_RETRY_DELAY_MS);
          target.retryAfter = Date.now() + delayMs;
          // Delivery failed: the frozen request keeps its bytes and its
          // place at the head of the queue; the backoff gate above holds
          // the next attempt until the retry moment.
          target.pending.unshift(req);
          trimPending(target);
          reportRetry(target, { attempt: attempts, delayMs, batchSize: req.eventCount, error: sendErr });
        }
        reportError(target, sendErr);
        return;
      }
      if (unloading) return; // one best-effort request per unload
    }
  });
  const owned = work.finally(() => {
    if (target.flushing === owned) target.flushing = null;
  });
  target.flushing = owned;
  return owned;
}

/** Force an immediate flush of buffered events. */
export function flush(): Promise<void> {
  if (!client || (client.buffer.length === 0 && client.pending.length === 0)) return Promise.resolve();
  return flushClient(client);
}

/**
 * O11 diagnostics: delivery and loss counters for the active client
 * (null before init()). Every dropped event is counted here by reason AND
 * reported through onError — the SDK never loses events silently.
 * `queued` is live; the rest are monotone counters.
 */
export function getStats(): Stats | null {
  if (!client) return null;
  const snapshot: Stats = {
    deliveredBatches: client.stats.deliveredBatches,
    deliveredEvents: client.stats.deliveredEvents,
    retries: client.stats.retries,
    dropped: { ...client.stats.dropped },
    undeliveredAtUnload: client.stats.undeliveredAtUnload,
    queued: client.buffer.length + client.pending.reduce((n, r) => n + r.eventCount, 0),
    producerId: client.producerId,
  };
  return snapshot;
}


/** Parse one stack frame line in V8 or SpiderMonkey syntax.
 *
 * Audit F30: the old regex's filename group excluded ':' entirely, so any
 * http(s) URL, port number, or Windows drive path failed to match and the
 * frame was dropped — normal web errors lost their stacks. The location is
 * now matched as "everything up to the final :line:col suffix", which
 * accepts URLs, ports, and file:/// and drive-letter paths.
 */
function parseFrame(line: string): { function: string; filename: string; lineno: number; colno: number; in_app: boolean } | null {
  line = line.trim();
  let fn = "<anonymous>";
  let location = line;
  if (line.startsWith("at ")) {
    location = line.slice(3);
    const paren = location.lastIndexOf(" (");
    if (paren >= 0 && location.endsWith(")")) {
      fn = location.slice(0, paren);
      location = location.slice(paren + 2, -1);
    }
  } else {
    const at = line.indexOf("@");
    if (at < 0) return null;
    fn = location.slice(0, at) || fn;
    location = location.slice(at + 1);
  }
  const m = /^(.*):(\d+):(\d+)$/.exec(location);
  if (!m || !m[1]) return null;
  const lineno = Number(m[2]);
  const colno = Number(m[3]);
  if (!Number.isSafeInteger(lineno) || !Number.isSafeInteger(colno)) return null;
  return { function: fn, filename: m[1], lineno, colno, in_app: !/node_modules|chrome-extension:/.test(m[1]) };
}

function parseStack(stack?: string): ErrorPayload["stack_trace"] {
  if (!stack) return undefined;
  const frames: NonNullable<ErrorPayload["stack_trace"]> = [];
  for (const line of stack.split("\n").slice(0, 50)) {
    const frame = parseFrame(line);
    if (frame) frames.push(frame);
  }
  return frames.length ? frames : undefined;
}

// Default export for convenience with older bundlers.
export default { init, pageview, track, identify, reset, captureException, log, flush, getStats };
