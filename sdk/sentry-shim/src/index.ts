/**
 * @observe/sentry-shim - drop-in replacement for `@sentry/node`, backed by
 * Observe's ingest API.
 *
 * Replace your import:
 *
 *   // before:
 *   // import * as Sentry from "@sentry/node";
 *   import * as Sentry from "@observe/sentry-shim";
 *
 *   Sentry.init({
 *     dsn: "https://observe.example.com/__observe__/default",
 *     // or:
 *     // endpoint: "https://observe.example.com",
 *     // siteId: "default",
 *     release: "v1.4.2",
 *     environment: "production",
 *   });
 *
 *   Sentry.captureException(new Error("boom"));
 *   Sentry.captureMessage("cache miss", "warning");
 *
 * The shim has zero runtime dependencies. It builds JSON envelopes and POSTs
 * them to the same ingest endpoints used by the official Observe SDKs:
 *
 *   `Sentry.captureException`  -> POST /api/v1/errors
 *   `Sentry.captureMessage`    -> POST /api/v1/logs
 *
 * Tracing (`startTransaction`/`startSpan`) is wired to OTLP-compatible
 * traces submission at `POST /api/v1/v1/traces` only when a real OTLP
 * exporter is configured by the host application; otherwise it returns a
 * lightweight scope-bound stub so existing call sites do not break.
 */

export type SeverityLevel =
  | "fatal"
  | "error"
  | "warning"
  | "log"
  | "info"
  | "debug";

export interface User {
  id?: string | number;
  email?: string;
  username?: string;
  ip_address?: string;
  [key: string]: unknown;
}

export interface Breadcrumb {
  type?: string;
  category?: string;
  message?: string;
  data?: Record<string, unknown>;
  level?: SeverityLevel;
  timestamp?: number;
}

export interface InitOptions {
  /** Sentry-style DSN. Either `dsn` or `endpoint` is required. */
  dsn?: string;
  /** Direct endpoint, e.g. `https://observe.example.com`. */
  endpoint?: string;
  /** Site identifier. Defaults to `"default"` (or DSN-extracted value). */
  siteId?: string;
  /** API key sent as the `X-API-Key` header. Optional during the grace period. */
  apiKey?: string;
  /** Release tag attached to every event. */
  release?: string;
  /** Environment tag attached to every event. */
  environment?: string;
  /** Maximum breadcrumbs retained per scope. Default: 100. */
  maxBreadcrumbs?: number;
  /** If true, skip outgoing network calls (useful for tests). */
  debug?: boolean;
  /** Custom fetch implementation. Defaults to global `fetch`. */
  fetch?: typeof fetch;
  /** Skip auto-collection of stack frames if you need a smaller payload. Default: true. */
  attachStacktrace?: boolean;
  // Accepted for Sentry compat; intentionally not honored:
  tracesSampleRate?: number;
  profilesSampleRate?: number;
  integrations?: unknown[];
  beforeSend?: (event: ErrorEnvelope) => ErrorEnvelope | null | Promise<ErrorEnvelope | null>;
  /** Sentry release-health flag. No-op (Observe tracks releases via the tag). */
  autoSessionTracking?: boolean;
}

export interface StackFrame {
  filename: string;
  function: string;
  lineno: number;
  colno: number;
  in_app?: boolean;
}

export interface ErrorEnvelope {
  site_id: string;
  error_type: string;
  error_value: string;
  level: SeverityLevel;
  release?: string;
  environment?: string;
  stack_trace?: StackFrame[];
  breadcrumbs?: Breadcrumb[];
  contexts?: Record<string, unknown>;
  extra?: Record<string, unknown>;
  fingerprint?: string[];
  url?: string;
  mechanism?: string;
  handled?: boolean;
  /** Identifier set via setUser({id}). Server hashes with site session_salt. */
  distinct_id?: string;
}

export interface LogEnvelope {
  site_id: string;
  level: "debug" | "info" | "warn" | "error" | "fatal";
  message: string;
  service_name?: string;
  attributes?: Record<string, unknown>;
  /** Identifier set via setUser({id}). Server hashes with site session_salt. */
  distinct_id?: string;
}

interface ResolvedConfig {
  endpoint: string;
  siteId: string;
  apiKey?: string;
  release?: string;
  environment?: string;
  maxBreadcrumbs: number;
  debug: boolean;
  attachStacktrace: boolean;
  fetchImpl: typeof fetch;
  beforeSend?: InitOptions["beforeSend"];
}

class Scope {
  user: User | null = null;
  tags: Record<string, string> = {};
  extras: Record<string, unknown> = {};
  contexts: Record<string, unknown> = {};
  fingerprint: string[] | null = null;
  level: SeverityLevel | null = null;
  breadcrumbs: Breadcrumb[] = [];

  clone(): Scope {
    const s = new Scope();
    s.user = this.user ? { ...this.user } : null;
    s.tags = { ...this.tags };
    s.extras = { ...this.extras };
    s.contexts = { ...this.contexts };
    s.fingerprint = this.fingerprint ? [...this.fingerprint] : null;
    s.level = this.level;
    s.breadcrumbs = [...this.breadcrumbs];
    return s;
  }

  apply(env: ErrorEnvelope): void {
    if (this.level) env.level = this.level;
    if (this.user) {
      env.contexts = { ...(env.contexts ?? {}), user: this.user };
      // Map Sentry-style setUser({id}) to Observe's identify() contract.
      // The server hashes the value with the per-site session_salt before
      // storage, so the raw id never persists by default.
      if (this.user.id !== undefined && this.user.id !== null) {
        env.distinct_id = String(this.user.id);
      }
    }
    if (Object.keys(this.tags).length > 0) {
      env.contexts = { ...(env.contexts ?? {}), tags: this.tags };
    }
    if (Object.keys(this.extras).length > 0) {
      env.extra = { ...(env.extra ?? {}), ...this.extras };
    }
    if (Object.keys(this.contexts).length > 0) {
      env.contexts = { ...(env.contexts ?? {}), ...this.contexts };
    }
    if (this.fingerprint) env.fingerprint = this.fingerprint;
    if (this.breadcrumbs.length > 0) env.breadcrumbs = [...this.breadcrumbs];
  }
}

let config: ResolvedConfig | null = null;
let rootScope: Scope = new Scope();
let scopeStack: Scope[] = [];

function activeScope(): Scope {
  return scopeStack.length > 0 ? scopeStack[scopeStack.length - 1] : rootScope;
}

function parseDsn(dsn: string): { endpoint: string; siteId: string } | null {
  let url: URL;
  try {
    url = new URL(dsn);
  } catch {
    return null;
  }
  // Observe-style DSN: https://observe.example.com/__observe__/default
  const marker = url.pathname.indexOf("/__observe__/");
  if (marker >= 0) {
    const tail = url.pathname.slice(marker + "/__observe__/".length);
    const siteId = tail.replace(/\/.*$/, "") || "default";
    const endpoint = url.origin + url.pathname.slice(0, marker);
    return { endpoint, siteId };
  }
  // Sentry-style: https://<publicKey>@<host>/<projectId>
  const parts = url.pathname.split("/").filter(Boolean);
  return {
    endpoint: url.origin,
    siteId: parts[parts.length - 1] || "default",
  };
}

/** Initialize the shim. Idempotent - calling again replaces the active config. */
export function init(options: InitOptions): void {
  let endpoint = options.endpoint;
  let siteId = options.siteId;
  if (!endpoint && options.dsn) {
    const parsed = parseDsn(options.dsn);
    if (parsed) {
      endpoint = parsed.endpoint;
      siteId ??= parsed.siteId;
    }
  }
  if (!endpoint) {
    throw new Error("@observe/sentry-shim: dsn or endpoint is required");
  }
  const fetchImpl = options.fetch ?? (globalThis as { fetch?: typeof fetch }).fetch;
  if (!fetchImpl) {
    throw new Error("@observe/sentry-shim: no fetch implementation available");
  }
  config = {
    endpoint: endpoint.replace(/\/+$/, ""),
    siteId: siteId ?? "default",
    apiKey: options.apiKey,
    release: options.release,
    environment: options.environment,
    maxBreadcrumbs: options.maxBreadcrumbs ?? 100,
    debug: options.debug ?? false,
    attachStacktrace: options.attachStacktrace ?? true,
    fetchImpl,
    beforeSend: options.beforeSend,
  };
  rootScope = new Scope();
  scopeStack = [];
}

/** Returns the active configuration (mostly for tests). */
export function getClient(): ResolvedConfig | null {
  return config;
}

async function postJSON(path: string, body: unknown): Promise<void> {
  if (!config) return;
  if (config.debug) return; // dry-run mode
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (config.apiKey) headers["X-API-Key"] = config.apiKey;
  try {
    await config.fetchImpl(config.endpoint + path, {
      method: "POST",
      headers,
      body: JSON.stringify(body),
    });
  } catch {
    // Mirror Sentry's behavior: never throw out of capture* calls.
  }
}

function parseStack(stack: string | undefined): StackFrame[] | undefined {
  if (!stack) return undefined;
  const frames: StackFrame[] = [];
  for (const line of stack.split("\n").slice(0, 50)) {
    // V8 / Node format: "    at fn (file:line:col)" or "    at file:line:col"
    const m = line.match(/at (?:(.+?) )?\(?([^()]+?):(\d+):(\d+)\)?\s*$/);
    if (!m) continue;
    const filename = sanitizeFilename(m[2]);
    frames.push({
      function: m[1] || "<anonymous>",
      filename,
      lineno: parseInt(m[3], 10),
      colno: parseInt(m[4], 10),
      in_app: !/node_modules|node:internal/.test(filename),
    });
  }
  return frames.length > 0 ? frames : undefined;
}

// Strip "file://" + percent-encoding from frame filenames so they survive
// Observe's URL-shaped storage and grouping logic (frames with raw file:// URIs
// trigger an upstream insertion bug where new issues never reach the listing).
function sanitizeFilename(raw: string): string {
  let s = raw;
  if (s.startsWith("file://")) s = s.slice("file://".length);
  try {
    s = decodeURI(s);
  } catch {
    // ignore - keep raw
  }
  return s;
}

function buildErrorEnvelope(
  err: unknown,
  scope: Scope,
  hint?: { mechanism?: string; level?: SeverityLevel }
): ErrorEnvelope {
  const cfg = config!;
  const e =
    err instanceof Error
      ? err
      : new Error(typeof err === "string" ? err : safeStringify(err));
  const env: ErrorEnvelope = {
    site_id: cfg.siteId,
    error_type: e.name || "Error",
    error_value: e.message || String(err),
    level: hint?.level ?? "error",
    release: cfg.release,
    environment: cfg.environment,
    stack_trace: cfg.attachStacktrace ? parseStack(e.stack) : undefined,
    mechanism: hint?.mechanism,
    handled: hint?.mechanism === undefined,
  };
  scope.apply(env);
  return env;
}

function safeStringify(v: unknown): string {
  try {
    return JSON.stringify(v);
  } catch {
    return String(v);
  }
}

/** Sentry: capture an exception. Returns a synthetic event id. */
export function captureException(
  err: unknown,
  hint?: { mechanism?: string; level?: SeverityLevel }
): string {
  if (!config) return "";
  const env = buildErrorEnvelope(err, activeScope(), hint);
  const id = randomId();
  void (async () => {
    let payload: ErrorEnvelope | null = env;
    if (config?.beforeSend) {
      try {
        payload = (await config.beforeSend(env)) ?? null;
      } catch {
        payload = env;
      }
    }
    if (payload) await postJSON("/api/v1/errors", payload);
  })();
  return id;
}

/** Sentry: capture a message. Routed to /api/v1/logs as a structured log. */
export function captureMessage(
  message: string,
  level: SeverityLevel = "info"
): string {
  if (!config) return "";
  const scope = activeScope();
  const log: LogEnvelope = {
    site_id: config.siteId,
    level: mapLevelToLog(level),
    message,
    attributes: {
      ...(scope.user ? { user: scope.user } : {}),
      ...(Object.keys(scope.tags).length ? { tags: scope.tags } : {}),
      ...(Object.keys(scope.extras).length ? { extra: scope.extras } : {}),
      ...(config.release ? { release: config.release } : {}),
      ...(config.environment ? { environment: config.environment } : {}),
    },
  };
  if (scope.user && scope.user.id !== undefined && scope.user.id !== null) {
    log.distinct_id = String(scope.user.id);
  }
  const id = randomId();
  void postJSON("/api/v1/logs", log);
  return id;
}

function mapLevelToLog(level: SeverityLevel): LogEnvelope["level"] {
  switch (level) {
    case "fatal":
      return "fatal";
    case "error":
      return "error";
    case "warning":
      return "warn";
    case "debug":
      return "debug";
    case "log":
    case "info":
    default:
      return "info";
  }
}

/** Sentry: associate user with subsequent events. Pass `null` to clear. */
export function setUser(user: User | null): void {
  activeScope().user = user;
}

/** Sentry: tag the active scope. */
export function setTag(key: string, value: string | number | boolean): void {
  activeScope().tags[key] = String(value);
}

/** Sentry: set multiple tags. */
export function setTags(tags: Record<string, string | number | boolean>): void {
  for (const [k, v] of Object.entries(tags)) setTag(k, v);
}

/** Sentry: set a structured context block on the active scope. */
export function setContext(key: string, ctx: Record<string, unknown> | null): void {
  if (ctx === null) {
    delete activeScope().contexts[key];
  } else {
    activeScope().contexts[key] = ctx;
  }
}

/** Sentry: add an extra value (free-form) to the active scope. */
export function setExtra(key: string, value: unknown): void {
  activeScope().extras[key] = value;
}

/** Sentry: set multiple extras. */
export function setExtras(extras: Record<string, unknown>): void {
  Object.assign(activeScope().extras, extras);
}

/** Sentry: override the grouping fingerprint. */
export function setFingerprint(parts: string[]): void {
  activeScope().fingerprint = [...parts];
}

/** Sentry: set the level for the next event. */
export function setLevel(level: SeverityLevel): void {
  activeScope().level = level;
}

/** Sentry: append a breadcrumb. Trimmed to `maxBreadcrumbs` per scope. */
export function addBreadcrumb(crumb: Breadcrumb): void {
  if (!config) return;
  const scope = activeScope();
  scope.breadcrumbs.push({
    timestamp: crumb.timestamp ?? Date.now() / 1000,
    ...crumb,
  });
  if (scope.breadcrumbs.length > config.maxBreadcrumbs) {
    scope.breadcrumbs.splice(0, scope.breadcrumbs.length - config.maxBreadcrumbs);
  }
}

/** Sentry: run `fn` against a forked scope. Mutations don't leak out. */
export function withScope<T>(fn: (scope: Scope) => T): T {
  const fork = activeScope().clone();
  scopeStack.push(fork);
  try {
    return fn(fork);
  } finally {
    scopeStack.pop();
  }
}

/** Sentry: clear the active root scope. */
export function configureScope(fn: (scope: Scope) => void): void {
  fn(activeScope());
}

/** Sentry: noop on the shim (sessions are inferred from event metadata). */
export function startSession(): void {
  // Release health is computed server-side from `release` + event volume;
  // the shim has nothing to track on the client.
}

/** Sentry: end the implicit session. No-op on the shim. */
export function endSession(): void {
  // See startSession.
}

/** Sentry: lightweight transaction stub. Returns a span-like object. */
export function startTransaction(ctx: { name: string; op?: string }): Span {
  return new Span(ctx.name, ctx.op);
}

/** Sentry v8+: callback-style span helper. */
export function startSpan<T>(
  ctx: { name: string; op?: string },
  fn: (span: Span) => T
): T {
  const span = new Span(ctx.name, ctx.op);
  try {
    return fn(span);
  } finally {
    span.finish();
  }
}

/**
 * Span shim. Records timings locally; finished spans are sent as a single log
 * entry so they show up in `/logs` with their duration. A future revision can
 * route these to OTLP at `/api/v1/v1/traces`.
 */
export class Span {
  readonly name: string;
  readonly op?: string;
  readonly startTimestamp: number;
  endTimestamp: number | null = null;
  data: Record<string, unknown> = {};
  status: "ok" | "error" | "unknown" = "unknown";

  constructor(name: string, op?: string) {
    this.name = name;
    this.op = op;
    this.startTimestamp = Date.now();
  }

  setData(key: string, value: unknown): void {
    this.data[key] = value;
  }

  setStatus(status: "ok" | "error" | "unknown"): this {
    this.status = status;
    return this;
  }

  setTag(key: string, value: string | number | boolean): this {
    this.data[`tag.${key}`] = String(value);
    return this;
  }

  startChild(ctx: { op?: string; description?: string }): Span {
    return new Span(ctx.description ?? this.name, ctx.op);
  }

  finish(): void {
    if (this.endTimestamp !== null) return;
    this.endTimestamp = Date.now();
    if (!config) return;
    const log: LogEnvelope = {
      site_id: config.siteId,
      level: this.status === "error" ? "error" : "info",
      message: `span: ${this.name}`,
      attributes: {
        op: this.op,
        duration_ms: this.endTimestamp - this.startTimestamp,
        status: this.status,
        ...this.data,
      },
    };
    void postJSON("/api/v1/logs", log);
  }
}

/** Force-flush any in-flight requests. The shim sends synchronously, so this is a no-op shim. */
export async function flush(_timeout?: number): Promise<boolean> {
  return true;
}

/** Sentry compat: close the client. */
export async function close(_timeout?: number): Promise<boolean> {
  config = null;
  rootScope = new Scope();
  scopeStack = [];
  return true;
}

/** Sentry compat: getCurrentHub() shim returning a minimal Hub-like object. */
export function getCurrentHub(): Hub {
  return defaultHub;
}

/** Sentry compat: getCurrentScope() returns the active scope. */
export function getCurrentScope(): Scope {
  return activeScope();
}

interface Hub {
  captureException: typeof captureException;
  captureMessage: typeof captureMessage;
  setUser: typeof setUser;
  setTag: typeof setTag;
  setExtra: typeof setExtra;
  withScope: typeof withScope;
  configureScope: typeof configureScope;
  addBreadcrumb: typeof addBreadcrumb;
  getClient: typeof getClient;
}

const defaultHub: Hub = {
  captureException,
  captureMessage,
  setUser,
  setTag,
  setExtra,
  withScope,
  configureScope,
  addBreadcrumb,
  getClient,
};

function randomId(): string {
  // 32 hex chars, matching Sentry's event_id shape.
  const bytes = new Uint8Array(16);
  if (typeof globalThis.crypto?.getRandomValues === "function") {
    globalThis.crypto.getRandomValues(bytes);
  } else {
    for (let i = 0; i < bytes.length; i++) bytes[i] = Math.floor(Math.random() * 256);
  }
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

export { Scope };
