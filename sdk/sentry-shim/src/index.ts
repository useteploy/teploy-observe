/**
 * @observe/sentry-shim — drop-in replacement for @sentry/browser, backed by
 * Observe.
 *
 * Usage (replace your Sentry import):
 *
 *   // before:
 *   // import * as Sentry from "@sentry/browser";
 *   import * as Sentry from "@observe/sentry-shim";
 *
 *   Sentry.init({
 *     dsn: "https://observe.example.com/__observe__/default",
 *     release: "v1.4.2",
 *   });
 *   Sentry.captureException(err);
 *
 * The `dsn` is parsed: the URL before `/__observe__/` becomes the endpoint,
 * the trailing path segment becomes the site id. Or pass `endpoint` + `siteId`
 * directly in a second-arg options bag.
 */

import {
  init as observeInit,
  captureException as observeCaptureException,
  track,
  identify,
  reset,
} from "@observe/browser";

export interface InitOptions {
  dsn?: string;
  endpoint?: string;
  siteId?: string;
  release?: string;
  environment?: string;
  tracesSampleRate?: number; // accepted for compat, not used
  integrations?: unknown[]; // accepted for compat, not used
}

let currentRelease: string | undefined;

function parseDsn(dsn: string): { endpoint: string; siteId: string } | null {
  try {
    const url = new URL(dsn);
    // Observe-style DSN: https://observe.example.com/__observe__/default
    const marker = url.pathname.indexOf("/__observe__/");
    if (marker >= 0) {
      const siteId = url.pathname.slice(marker + "/__observe__/".length).replace(/\/.*$/, "");
      return { endpoint: url.origin + url.pathname.slice(0, marker), siteId };
    }
    // Sentry-style DSN fallback: https://<key>@host/<project>
    // Use the host as endpoint; site id = last path segment.
    const parts = url.pathname.split("/").filter(Boolean);
    return { endpoint: url.origin, siteId: parts[parts.length - 1] || "default" };
  } catch {
    return null;
  }
}

export function init(options: InitOptions): void {
  let endpoint = options.endpoint;
  let siteId = options.siteId;
  if (!endpoint && options.dsn) {
    const parsed = parseDsn(options.dsn);
    if (parsed) { endpoint = parsed.endpoint; siteId ??= parsed.siteId; }
  }
  if (!endpoint) throw new Error("@observe/sentry-shim: dsn or endpoint is required");
  currentRelease = options.release;
  observeInit({ endpoint, siteId: siteId ?? "default" });
}

export function captureException(err: unknown, ctx?: { release?: string; tags?: Record<string, string> }): void {
  const e = err instanceof Error ? err : new Error(String(err));
  void observeCaptureException(e, { release: ctx?.release ?? currentRelease });
}

export function captureMessage(msg: string, level: "info" | "warning" | "error" = "info"): void {
  track("message", { message: msg, level });
}

export function setUser(user: { id?: string; email?: string; [k: string]: unknown } | null): void {
  if (user?.id) identify(user.id, user as Record<string, unknown>);
  else reset();
}

export function setTag(_key: string, _value: unknown): void {
  // No-op: Observe associates tags with individual events, not with a scope.
}

export function setContext(_key: string, _context: unknown): void {
  // No-op: context flows through per-call props, not global scope.
}

export function setExtra(_key: string, _value: unknown): void {
  // No-op.
}

export function addBreadcrumb(_breadcrumb: unknown): void {
  // observe-errors.js captures breadcrumbs automatically.
}

export function withScope<T>(fn: (scope: { setTag: typeof setTag; setExtra: typeof setExtra }) => T): T {
  return fn({ setTag, setExtra });
}

// `@sentry/browser` exports a `Hub` class; shim it enough for typical usage.
export const getCurrentHub = () => ({
  captureException,
  captureMessage,
  setUser,
  setTag,
  setExtra,
  withScope,
});
