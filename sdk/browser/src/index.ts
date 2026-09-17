/**
 * @teploy/observe-browser — Observe's browser SDK.
 *
 * ```ts
 * import { init } from "@teploy/observe-browser";
 *
 * init({
 *   endpoint: "https://observe.example.com",
 *   siteId: "default",
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
  /** API key for server-side or authenticated ingest. Most browsers don't need one. */
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
}

export interface EventPayload {
  site_id: string;
  event_type: string;
  url?: string;
  referrer?: string;
  title?: string;
  /** Hashed user identifier set by identify(). Empty for anonymous events. */
  distinct_id?: string;
  /** Application release tag from init({ release }). Empty if not set. */
  release?: string;
  /** Custom event properties. The server only reads properties from here —
   * anything spread at the top level of the payload is not a field it stores. */
  properties?: Record<string, unknown>;
  [key: string]: unknown;
}

export interface ErrorPayload {
  site_id: string;
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
  opts: Required<Omit<InitOptions, "apiKey" | "release" | "environment" | "onError">> & {
    apiKey?: string;
    release?: string;
    environment?: string;
    onError?: (err: Error) => void;
  };
  buffer: EventPayload[];
  timer: number | null;
  userId: string | null;
  sessionId: string | null;
  /** Removes this client's interval and DOM listeners (audit F31). */
  dispose: () => void;
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

// Retained events on a failed send (audit F32): keep the batch for the next
// flush instead of silently erasing it, but bound the retention so a long
// outage cannot grow memory without limit. Beyond the cap, oldest events
// are dropped and reported.
const MAX_BUFFERED_ON_ERROR = 200;

const textEncoder = new TextEncoder();

/**
 * Send one JSON payload. Reports failures through the reject path (audit
 * F32): the old transport resolved on any fetch completion regardless of
 * status and swallowed network errors, so 4xx/5xx and offline transitions
 * silently erased telemetry. `keepalive` is only used for small final
 * sends while the page is unloading — the fetch standard fails any
 * keepalive request whose body exceeds the browser's 64 KiB in-flight
 * budget, which large snapshots used to hit.
 */
function sendJSON(target: Client, path: string, payload: unknown, unloading = false): Promise<void> {
  let raw: string;
  try {
    raw = JSON.stringify(payload);
  } catch (err) {
    reportError(target, err instanceof Error ? err : new Error("observe: payload not serializable"));
    return Promise.resolve();
  }
  const url = target.opts.endpoint.replace(/\/+$/, "") + path;
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (target.opts.apiKey) headers["X-API-Key"] = target.opts.apiKey;
  const bytes = textEncoder.encode(raw).byteLength;

  const viaFetch = (keepalive: boolean): Promise<void> =>
    fetch(url, { method: "POST", headers, body: raw, credentials: "omit", keepalive })
      .then((res) => {
        if (!res.ok) throw new Error(`observe: ingest returned ${res.status}`);
      })
      .catch((err) => {
        reportError(target, err instanceof Error ? err : new Error(String(err)));
        throw err;
      });

  if (unloading && bytes <= MAX_KEEPALIVE_BYTES) {
    // Beacons cannot carry headers, so they only apply to keyless installs;
    // a false return means the browser refused to queue it — fall through
    // to keepalive fetch instead of treating the batch as delivered.
    if (!target.opts.apiKey && typeof navigator !== "undefined" && "sendBeacon" in navigator) {
      try {
        if (navigator.sendBeacon(url, new Blob([raw], { type: "application/json" }))) {
          return Promise.resolve();
        }
      } catch {
        /* fall through to fetch */
      }
    }
    return viaFetch(true).catch(() => undefined); // best effort on unload
  }
  if (bytes > MAX_KEEPALIVE_BYTES) {
    // An oversized body can never go out keepalive; an explicit, reported
    // failure beats a silent network error the browser will log anyway.
    return viaFetch(false).catch(() => undefined);
  }
  return viaFetch(false).catch(() => undefined);
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

/** Initialize the SDK. Re-init disposes the previous client's timer and
 * listeners and flushes its buffer under the OLD configuration (audit F31);
 * previously each init() leaked one interval plus three listeners and
 * silently dropped everything the replaced client had buffered. */
export function init(options: InitOptions): void {
  if (!options.endpoint) throw new Error("@teploy/observe-browser: endpoint is required");

  const previous = client;
  if (previous) {
    previous.dispose();
    // Best-effort final flush of the old client's events to the old
    // endpoint under the old key — never redirected to the new config.
    if (previous.buffer.length > 0) {
      const batch = previous.buffer.splice(0);
      sendJSON(previous, "/api/v1/events/batch", { events: chunkEvents(batch) }).catch(() => undefined);
    }
  }

  const instance: Client = {
    opts: {
      endpoint: options.endpoint,
      siteId: options.siteId ?? "default",
      apiKey: options.apiKey,
      disableAutoPageview: options.disableAutoPageview ?? false,
      batchSize: options.batchSize ?? 50,
      flushIntervalMs: options.flushIntervalMs ?? 2000,
      release: options.release,
      environment: options.environment,
      onError: options.onError,
    },
    buffer: [],
    timer: null,
    userId: null,
    sessionId: makeId(),
    dispose: () => {},
  };
  client = instance;

  if (typeof window !== "undefined") {
    // Listeners are bound to THIS client instance and removed on dispose
    // via an AbortController (audit F31).
    const controller = new AbortController();
    instance.dispose = () => {
      if (instance.timer !== null) window.clearInterval(instance.timer);
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
 */
export function pageview(pathname?: string): void {
  if (!client) return;
  const props: Record<string, unknown> = {
    url: typeof location !== "undefined" ? location.href : "",
    referrer: typeof document !== "undefined" ? document.referrer : "",
    title: typeof document !== "undefined" ? document.title : "",
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
  "url",
  "referrer",
  "title",
  "language",
  "screen",
  "distinct_id",
  "release",
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
  if (!client) return;
  const payload: EventPayload = {
    site_id: client.opts.siteId,
    event_type: eventType,
  };
  const properties: Record<string, unknown> = {};
  for (const key of Object.keys(props)) {
    if (RESERVED_FIELDS.has(key)) {
      payload[key] = props[key];
    } else if (Object.keys(properties).length < MAX_PROPERTIES) {
      properties[key] = props[key];
    }
  }
  if (Object.keys(properties).length > 0) payload.properties = properties;
  if (client.userId) payload.distinct_id = client.userId;
  if (client.opts.release) payload.release = client.opts.release;
  client.buffer.push(payload);
  if (client.buffer.length >= client.opts.batchSize) flush();
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

/** Submit an error. Sends immediately — not buffered. */
export function captureException(err: Error, ctx?: CaptureContext): Promise<void> {
  if (!client) return Promise.resolve();
  return captureExceptionFor(client, err, ctx);
}

function captureExceptionFor(target: Client, err: Error, ctx?: CaptureContext): Promise<void> {
  const payload: ErrorPayload = {
    site_id: target.opts.siteId,
    error_type: err.name || "Error",
    error_value: err.message || String(err),
    release_tag: ctx?.release ?? target.opts.release,
    environment: ctx?.environment ?? target.opts.environment ?? "production",
    mechanism: ctx?.mechanism ?? "manual",
    url: typeof location !== "undefined" ? location.href : "",
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
  return sendJSON(target, "/api/v1/errors", payload);
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

/** Submit a log entry. */
export function log(entry: Omit<LogPayload, "site_id">): Promise<void> {
  if (!client) return Promise.resolve();
  return sendJSON(client, "/api/v1/logs", { site_id: client.opts.siteId, ...entry });
}

/** Split events into server-legal chunks (max 100 per request). */
function chunkEvents(events: EventPayload[]): EventPayload[][] {
  const chunks: EventPayload[][] = [];
  for (let i = 0; i < events.length; i += MAX_EVENTS_PER_REQUEST) {
    chunks.push(events.slice(i, i + MAX_EVENTS_PER_REQUEST));
  }
  return chunks;
}

/** Flush a specific client's buffer. Events are only removed from the
 * buffer once a chunk is accepted; a rejected chunk is retained (bounded)
 * for the next flush instead of being dropped (audit F32). */
async function flushClient(target: Client, unloading = false): Promise<void> {
  while (target.buffer.length > 0) {
    const batch = target.buffer.slice(0, MAX_EVENTS_PER_REQUEST);
    try {
      await sendJSON(target, "/api/v1/events/batch", { events: batch }, unloading);
    } catch {
      // Delivery failed: retain this chunk (up to the bounded cap) and
      // stop — the next interval/visibility flush retries it.
      target.buffer = target.buffer.slice(batch.length);
      target.buffer = [...batch.slice(0, MAX_BUFFERED_ON_ERROR), ...target.buffer].slice(0, MAX_BUFFERED_ON_ERROR);
      if (batch.length > MAX_BUFFERED_ON_ERROR) {
        reportError(target, new Error(`observe: dropped ${batch.length - MAX_BUFFERED_ON_ERROR} buffered events (send failing and retention cap reached)`));
      }
      return;
    }
    target.buffer = target.buffer.slice(batch.length);
    if (unloading) return; // one best-effort chunk per unload
  }
}

/** Force an immediate flush of buffered events. */
export function flush(): Promise<void> {
  if (!client || client.buffer.length === 0) return Promise.resolve();
  return flushClient(client);
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
export default { init, pageview, track, identify, reset, captureException, log, flush };
