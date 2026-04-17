/**
 * @observe/browser — Observe's browser SDK.
 *
 * ```ts
 * import { init } from "@observe/browser";
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
}

export interface EventPayload {
  site_id: string;
  event_type: string;
  pathname?: string;
  url?: string;
  referrer?: string;
  title?: string;
  [key: string]: unknown;
}

export interface ErrorPayload {
  site_id: string;
  error_type: string;
  error_value: string;
  stack_trace?: Array<{ filename: string; function: string; lineno: number; colno: number; in_app?: boolean }>;
  release_tag?: string;
  environment?: string;
  url?: string;
  breadcrumbs?: unknown[];
  level?: "error" | "warning" | "info";
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
  opts: Required<Omit<InitOptions, "apiKey">> & { apiKey?: string };
  buffer: EventPayload[];
  timer: number | null;
  userId: string | null;
  sessionId: string | null;
}

let client: Client | null = null;

function makeId(): string {
  const arr = new Uint8Array(16);
  (globalThis.crypto ?? (globalThis as any).msCrypto).getRandomValues(arr);
  return Array.from(arr, (b) => b.toString(16).padStart(2, "0")).join("");
}

function post<T>(path: string, body: T, apiKey?: string): Promise<void> {
  if (!client) return Promise.resolve();
  const url = client.opts.endpoint.replace(/\/$/, "") + path;
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (apiKey) headers["X-API-Key"] = apiKey;

  // Prefer sendBeacon for fire-and-forget paths when available.
  const raw = JSON.stringify(body);
  if (typeof navigator !== "undefined" && "sendBeacon" in navigator && !apiKey) {
    const ok = navigator.sendBeacon(url, new Blob([raw], { type: "application/json" }));
    if (ok) return Promise.resolve();
  }
  return fetch(url, { method: "POST", headers, body: raw, keepalive: true })
    .then(() => void 0)
    .catch(() => void 0);
}

/** Initialize the SDK. Idempotent — calling again replaces the active client. */
export function init(options: InitOptions): void {
  if (!options.endpoint) throw new Error("@observe/browser: endpoint is required");

  client = {
    opts: {
      endpoint: options.endpoint,
      siteId: options.siteId ?? "default",
      apiKey: options.apiKey,
      disableAutoPageview: options.disableAutoPageview ?? false,
      batchSize: options.batchSize ?? 50,
      flushIntervalMs: options.flushIntervalMs ?? 2000,
    },
    buffer: [],
    timer: null,
    userId: null,
    sessionId: makeId(),
  };

  if (typeof window !== "undefined") {
    // Auto-flush on buffer age.
    client.timer = window.setInterval(flush, client.opts.flushIntervalMs);

    // Flush when the tab is hidden (mobile app-switch, close).
    document.addEventListener("visibilitychange", () => {
      if (document.visibilityState === "hidden") flush();
    });

    // Autocapture: uncaught errors and promise rejections.
    window.addEventListener("error", (e) => {
      captureException(e.error ?? new Error(e.message), { mechanism: "onerror" });
    });
    window.addEventListener("unhandledrejection", (e: PromiseRejectionEvent) => {
      const reason: any = e.reason;
      captureException(reason instanceof Error ? reason : new Error(String(reason)), {
        mechanism: "onunhandledrejection",
      });
    });

    if (!client.opts.disableAutoPageview) {
      pageview();
    }
  }
}

/** Record a pageview for the current URL. */
export function pageview(pathname?: string): void {
  if (!client) return;
  track("pageview", {
    pathname: pathname ?? (typeof location !== "undefined" ? location.pathname + location.search : ""),
    url: typeof location !== "undefined" ? location.href : "",
    referrer: typeof document !== "undefined" ? document.referrer : "",
    title: typeof document !== "undefined" ? document.title : "",
  });
}

/** Record a custom event. */
export function track(eventType: string, props: Record<string, unknown> = {}): void {
  if (!client) return;
  const payload: EventPayload = {
    site_id: client.opts.siteId,
    event_type: eventType,
    ...props,
  };
  client.buffer.push(payload);
  if (client.buffer.length >= client.opts.batchSize) flush();
}

/** Associate subsequent events with a user id (and optional traits). */
export function identify(userId: string, traits?: Record<string, unknown>): void {
  if (!client) return;
  client.userId = userId;
  if (traits) track("identify", { user_id: userId, ...traits });
}

/** Clear the current user — call on logout. */
export function reset(): void {
  if (!client) return;
  client.userId = null;
  client.sessionId = makeId();
}

/** Submit an error. Sends immediately — not buffered. */
export function captureException(err: Error, ctx?: { mechanism?: string; release?: string; tags?: Record<string, string> }): Promise<void> {
  if (!client) return Promise.resolve();
  const payload: ErrorPayload = {
    site_id: client.opts.siteId,
    error_type: err.name || "Error",
    error_value: err.message || String(err),
    release_tag: ctx?.release,
    environment: "production",
    url: typeof location !== "undefined" ? location.href : "",
    level: "error",
    stack_trace: parseStack(err.stack),
  };
  return post("/api/v1/errors", payload, client.opts.apiKey);
}

/** Submit a log entry. */
export function log(entry: Omit<LogPayload, "site_id">): Promise<void> {
  if (!client) return Promise.resolve();
  return post("/api/v1/logs", { site_id: client.opts.siteId, ...entry }, client.opts.apiKey);
}

/** Force an immediate flush of buffered events. */
export function flush(): Promise<void> {
  if (!client || client.buffer.length === 0) return Promise.resolve();
  const batch = client.buffer.splice(0);
  return post("/api/v1/events/batch", { events: batch });
}

function parseStack(stack?: string): ErrorPayload["stack_trace"] {
  if (!stack) return undefined;
  // Chrome / Node format: "    at fn (file:line:col)"
  const frames: NonNullable<ErrorPayload["stack_trace"]> = [];
  for (const line of stack.split("\n").slice(0, 50)) {
    const m = line.match(/at (?:(.+?) )?\(?([^():]+):(\d+):(\d+)\)?$/);
    if (!m) continue;
    frames.push({
      function: m[1] || "<anonymous>",
      filename: m[2],
      lineno: parseInt(m[3], 10),
      colno: parseInt(m[4], 10),
      in_app: !/node_modules|chrome-extension:/.test(m[2]),
    });
  }
  return frames.length ? frames : undefined;
}

// Default export for convenience with older bundlers.
export default { init, pageview, track, identify, reset, captureException, log, flush };
