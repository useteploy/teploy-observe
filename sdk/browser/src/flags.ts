/**
 * Feature-flag evaluation client for @teploy/observe-browser.
 *
 * Wire contract (cmd/observe flagEvaluateHandler, internal/flags):
 *
 *   POST {endpoint}/api/v1/flags/evaluate
 *   { "site_id", "flag_key", "user_id", "context": { string: string } }
 *   -> { "enabled": bool, "variant"?: string, "reason": "evaluated" |
 *        "unavailable" | "invalid", "detail"?: string }
 *
 * The route is public (an X-API-Key is accepted but not required) and is
 * rate limited per IP and per site, so this client caches and coalesces.
 *
 * Failure policy: evaluation never rejects. A network error, timeout,
 * non-2xx status, malformed body, or a server answer whose reason is not
 * "evaluated" (the store was unavailable or the flag config is invalid)
 * yields the caller-supplied default with `source: "default"` and the
 * cause in `error`. Such results are never cached, so the next call
 * retries. Without a caller default the fail-safe is `{ enabled: false }`,
 * matching the server's own fail-safe.
 *
 * Exposure safety: this module never records exposures or sends any other
 * telemetry. Evaluating a flag is not the same as the user seeing the
 * variant; exposure is recorded only when the caller asks (see the
 * `exposure` option on index.ts's evaluateFlag, or call track() yourself
 * at the point the variant is actually rendered). Note that the server
 * itself logs each enabled evaluation, which is why identical calls inside
 * the TTL are served from the cache.
 */

/** What to return when the server cannot give a decision. */
export interface FlagDefault {
  enabled: boolean;
  variant?: string;
}

export interface FlagContext {
  /** Stable user id used for rollout bucketing and variant assignment. */
  userId?: string;
  /** Targeting attributes. Values are sent as strings (the server's
   * context is map[string]string); non-primitive values are skipped. */
  attributes?: Record<string, string | number | boolean>;
  /** Returned when evaluation fails. Default `{ enabled: false }`. */
  default?: FlagDefault;
  /** Per-call cache TTL override in ms. 0 bypasses the cache read. */
  ttlMs?: number;
  /** Per-call timeout override in ms. */
  timeoutMs?: number;
}

export interface FlagResult {
  key: string;
  enabled: boolean;
  variant?: string;
  /** Server reason ("evaluated"), or "default" when the default was used. */
  reason: string;
  detail?: string;
  /** Where the value came from. */
  source: "server" | "cache" | "default";
  /** Why the default was used (only when source is "default"). */
  error?: string;
}

export interface FlagClientOptions {
  endpoint: string;
  siteId?: string;
  apiKey?: string;
  /** Cache TTL in ms. Default 30000; clamped to [0, 3600000]. 0 disables caching. */
  ttlMs?: number;
  /** Request timeout in ms. Default 3000; clamped to [100, 60000]. */
  timeoutMs?: number;
  /** Max cached decisions. Default 500. */
  maxEntries?: number;
  /** Injectable fetch (tests, polyfills). Defaults to globalThis.fetch. */
  fetch?: typeof fetch;
}

export interface FlagClient {
  evaluateFlag(key: string, ctx?: FlagContext): Promise<FlagResult>;
  evaluateFlags(keys: string[], ctx?: FlagContext): Promise<Record<string, FlagResult>>;
  clearCache(): void;
}

const MAX_KEYS_PER_CALL = 50;

function clamp(v: number | undefined, dflt: number, min: number, max: number): number {
  const n = typeof v === "number" && Number.isFinite(v) ? v : dflt;
  return Math.min(max, Math.max(min, n));
}

function normalizeAttributes(attrs: FlagContext["attributes"]): Record<string, string> {
  const out: Record<string, string> = {};
  if (!attrs || typeof attrs !== "object") return out;
  for (const k of Object.keys(attrs).sort()) {
    const v = (attrs as Record<string, unknown>)[k];
    if (typeof v === "string") out[k] = v;
    else if (typeof v === "number" || typeof v === "boolean") out[k] = String(v);
  }
  return out;
}

function fallback(key: string, ctx: FlagContext | undefined, error: string): FlagResult {
  const d = ctx?.default;
  return {
    key,
    enabled: d ? d.enabled === true : false,
    ...(d?.variant ? { variant: d.variant } : {}),
    reason: "default",
    source: "default",
    error,
  };
}

export function createFlagClient(options: FlagClientOptions): FlagClient {
  const siteId = options.siteId ?? "default";
  const ttl = clamp(options.ttlMs, 30_000, 0, 3_600_000);
  const timeout = clamp(options.timeoutMs, 3_000, 100, 60_000);
  const maxEntries = clamp(options.maxEntries, 500, 1, 10_000);
  const url = options.endpoint.replace(/\/+$/, "") + "/api/v1/flags/evaluate";

  const cache = new Map<string, { at: number; ttl: number; value: Omit<FlagResult, "source"> }>();
  const inflight = new Map<string, Promise<FlagResult>>();

  function cacheKey(key: string, userId: string, attrs: Record<string, string>): string {
    return JSON.stringify([siteId, key, userId, attrs]);
  }

  async function request(key: string, userId: string, attrs: Record<string, string>, ctx: FlagContext | undefined, ck: string, ttlMs: number, timeoutMs: number): Promise<FlagResult> {
    const doFetch = options.fetch ?? (typeof fetch === "function" ? fetch : undefined);
    if (!doFetch) return fallback(key, ctx, "fetch unavailable");
    const controller = typeof AbortController !== "undefined" ? new AbortController() : null;
    let timer: ReturnType<typeof setTimeout> | undefined;
    // The abort signal cancels the request; the race guarantees the
    // timeout even for a fetch implementation that ignores the signal.
    const timedOut = new Promise<never>((_, reject) => {
      timer = setTimeout(() => {
        controller?.abort();
        reject(new Error(`timeout after ${timeoutMs}ms`));
      }, timeoutMs);
    });
    timedOut.catch(() => {}); // a late rejection after the race is settled is expected
    try {
      const headers: Record<string, string> = { "Content-Type": "application/json" };
      if (options.apiKey) headers["X-API-Key"] = options.apiKey;
      const res = await Promise.race([
        doFetch(url, {
          method: "POST",
          headers,
          body: JSON.stringify({ site_id: siteId, flag_key: key, user_id: userId, context: attrs }),
          credentials: "omit",
          redirect: "error",
          signal: controller?.signal,
        }),
        timedOut,
      ]);
      if (!res.ok) return fallback(key, ctx, `status ${res.status}`);
      const body: any = await Promise.race([res.json(), timedOut]);
      if (!body || typeof body !== "object" || typeof body.enabled !== "boolean") {
        return fallback(key, ctx, "malformed response");
      }
      const reason = typeof body.reason === "string" ? body.reason : "";
      if (reason !== "evaluated") {
        // unavailable / invalid: the server answered its fail-safe, not a decision.
        return fallback(key, ctx, `server reason ${reason || "missing"}`);
      }
      const value: Omit<FlagResult, "source"> = {
        key,
        enabled: body.enabled,
        ...(typeof body.variant === "string" && body.variant ? { variant: body.variant } : {}),
        reason,
        ...(typeof body.detail === "string" && body.detail ? { detail: body.detail } : {}),
      };
      if (ttlMs > 0) {
        cache.delete(ck);
        cache.set(ck, { at: Date.now(), ttl: ttlMs, value });
        while (cache.size > maxEntries) {
          const oldest = cache.keys().next().value;
          if (oldest === undefined) break;
          cache.delete(oldest);
        }
      }
      return { ...value, source: "server" };
    } catch (err) {
      return fallback(key, ctx, err instanceof Error ? err.message : String(err));
    } finally {
      if (timer !== undefined) clearTimeout(timer);
    }
  }

  function evaluateFlag(key: string, ctx?: FlagContext): Promise<FlagResult> {
    try {
      if (typeof key !== "string" || key === "") return Promise.resolve(fallback(String(key), ctx, "flag key required"));
      const userId = typeof ctx?.userId === "string" ? ctx.userId : "";
      const attrs = normalizeAttributes(ctx?.attributes);
      const ttlMs = clamp(ctx?.ttlMs, ttl, 0, 3_600_000);
      const timeoutMs = clamp(ctx?.timeoutMs, timeout, 100, 60_000);
      const ck = cacheKey(key, userId, attrs);

      if (ttlMs > 0) {
        const hit = cache.get(ck);
        if (hit && Date.now() - hit.at < Math.min(hit.ttl, ttlMs)) return Promise.resolve({ ...hit.value, source: "cache" });
        if (hit) cache.delete(ck);
      }
      // Coalesce identical in-flight evaluations into one request. Each
      // caller still gets its own default applied on failure.
      const pending = inflight.get(ck);
      if (pending) {
        return pending.then((r) => (r.source === "default" ? fallback(key, ctx, r.error ?? "failed") : { ...r }));
      }
      const p = request(key, userId, attrs, ctx, ck, ttlMs, timeoutMs).finally(() => {
        if (inflight.get(ck) === p) inflight.delete(ck);
      });
      inflight.set(ck, p);
      return p;
    } catch (err) {
      return Promise.resolve(fallback(String(key), ctx, err instanceof Error ? err.message : String(err)));
    }
  }

  async function evaluateFlags(keys: string[], ctx?: FlagContext): Promise<Record<string, FlagResult>> {
    const out: Record<string, FlagResult> = {};
    const unique = Array.from(new Set(Array.isArray(keys) ? keys.filter((k) => typeof k === "string" && k !== "") : []));
    const used = unique.slice(0, MAX_KEYS_PER_CALL);
    const results = await Promise.all(used.map((k) => evaluateFlag(k, ctx)));
    used.forEach((k, i) => { out[k] = results[i]; });
    // Keys beyond the per-call cap are not sent; they resolve to the default.
    for (const k of unique.slice(MAX_KEYS_PER_CALL)) out[k] = fallback(k, ctx, `more than ${MAX_KEYS_PER_CALL} keys in one call`);
    return out;
  }

  return { evaluateFlag, evaluateFlags, clearCache: () => { cache.clear(); } };
}
