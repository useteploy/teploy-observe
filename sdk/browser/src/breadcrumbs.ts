/**
 * Breadcrumb recorder for @teploy/observe-browser.
 *
 * A breadcrumb is a small record of something that happened before an
 * error. The wire shape matches what the server's error ingest accepts
 * (internal/errors Breadcrumb): `type`, `category`, `message`, `data`,
 * `timestamp` (Unix ms), `level`.
 *
 * Automatic capture is OPT-IN (`init({ breadcrumbs: true })`). Every patch
 * the recorder installs is reversible: `uninstall()` restores the original
 * function when the SDK's wrapper is still the outermost one, and otherwise
 * turns the wrapper into a pass-through so a later patcher (another
 * monitoring tool) is never broken by teardown order. No wrapper ever
 * throws into host code: recording is wrapped in try/catch and the
 * original call always runs, with its original `this`, arguments and
 * return value.
 */

export type BreadcrumbLevel = "debug" | "info" | "warning" | "error";

export interface Breadcrumb {
  /** Coarse kind: "default", "console", "user", "navigation", "http". */
  type: string;
  /** Finer label: "console", "click", "navigation", "fetch", "xhr", or your own. */
  category: string;
  message: string;
  /** JSON-serializable detail. Oversized or non-serializable data is replaced. */
  data?: Record<string, unknown>;
  /** Unix epoch milliseconds. */
  timestamp: number;
  level?: BreadcrumbLevel;
}

/** Fields a caller supplies to addBreadcrumb(); timestamp defaults to now. */
export type BreadcrumbInput = Partial<Omit<Breadcrumb, "message">> & { message: string };

export interface BreadcrumbOptions {
  /** Ring buffer size. Default 100; clamped to [1, 1000]. */
  maxBreadcrumbs?: number;
  /** Record console.log/info/warn/error/debug. Default true. */
  console?: boolean;
  /** Record clicks (tag, id, classes; no text). Default true. */
  clicks?: boolean;
  /** Include the clicked element's text (first 64 chars) in click data.
   * Default false: text content can carry personal data. */
  clickText?: boolean;
  /** Record pushState/replaceState/popstate/hashchange navigation. Default true. */
  navigation?: boolean;
  /** Record fetch calls (method, status, url without query). Default true. */
  fetch?: boolean;
  /** Record XMLHttpRequest calls (method, status, url without query). Default true. */
  xhr?: boolean;
  /** Last chance to edit or drop a breadcrumb. Return null/undefined to
   * drop it. Applies to manual and automatic breadcrumbs alike. A hook
   * that throws drops the breadcrumb (fail closed: it is usually a scrubber). */
  beforeBreadcrumb?: (crumb: Breadcrumb) => Breadcrumb | null | undefined;
  /** URL prefixes whose HTTP calls are never recorded. The SDK adds its
   * own ingest endpoint here so it cannot record its own traffic. */
  ignoreUrls?: string[];
}

export const DEFAULT_MAX_BREADCRUMBS = 100;
const MAX_MESSAGE = 256;
const MAX_DATA_BYTES = 1024;
/** Budget for the breadcrumbs attached to one payload; oldest are dropped first. */
const MAX_ATTACH_BYTES = 32 * 1024;

function clampInt(v: unknown, dflt: number, min: number, max: number): number {
  const n = typeof v === "number" && Number.isFinite(v) ? Math.floor(v) : dflt;
  return Math.min(max, Math.max(min, n));
}

function truncate(s: string, n: number): string {
  return s.length > n ? s.slice(0, n) : s;
}

/** Strip query, fragment and credentials; keep origin+path (F41 contract). */
export function sanitizeUrl(raw: unknown): string {
  if (typeof raw !== "string" || raw === "") return "";
  try {
    const base = typeof location !== "undefined" && location.href ? location.href : undefined;
    const u = base ? new URL(raw, base) : new URL(raw);
    if (u.protocol !== "http:" && u.protocol !== "https:") return "";
    return u.origin + u.pathname;
  } catch {
    // Unparseable: cut at the first ? or # as a last resort.
    return truncate(raw.split(/[?#]/)[0], 256);
  }
}

function safeStringify(v: unknown): string {
  if (typeof v === "string") return v;
  if (v instanceof Error) return `${v.name}: ${v.message}`;
  if (v === undefined) return "undefined";
  try {
    const s = JSON.stringify(v);
    return s === undefined ? String(v) : s;
  } catch {
    try {
      return String(v);
    } catch {
      return "[unprintable]";
    }
  }
}

/** Own-data snapshot: JSON round trip, size-capped. Returns undefined when
 * the data cannot be represented. */
function cloneData(data: unknown): Record<string, unknown> | undefined {
  if (data === null || typeof data !== "object") return undefined;
  try {
    const raw = JSON.stringify(data);
    if (raw === undefined) return undefined;
    if (raw.length > MAX_DATA_BYTES) return { _truncated: true, _bytes: raw.length };
    const parsed = JSON.parse(raw);
    return parsed && typeof parsed === "object" && !Array.isArray(parsed) ? parsed : { value: parsed };
  } catch {
    return { _unserializable: true };
  }
}

const LEVELS: ReadonlySet<string> = new Set(["debug", "info", "warning", "error"]);

export interface BreadcrumbRecorder {
  add(input: BreadcrumbInput): void;
  /** Oldest-first copy, bounded to the attach byte budget. */
  snapshot(): Breadcrumb[];
  clear(): void;
  /** Install automatic capture. Idempotent. Returns false without a window. */
  install(): boolean;
  /** Remove every patch and listener this recorder installed. Idempotent. */
  uninstall(): void;
  readonly installed: boolean;
}

type AnyFn = (...args: any[]) => any;

/** Describe a DOM element without its text: `button#save.btn.primary`. */
export function describeElement(el: any): { message: string; data: Record<string, unknown> } {
  const tag = typeof el?.tagName === "string" ? el.tagName.toLowerCase() : "unknown";
  const id = typeof el?.id === "string" && el.id ? el.id : "";
  let classes: string[] = [];
  const cn = el?.className;
  if (typeof cn === "string") classes = cn.split(/\s+/).filter(Boolean);
  classes = classes.slice(0, 3).map((c) => truncate(c, 40));
  const message = truncate(tag + (id ? "#" + truncate(id, 60) : "") + classes.map((c) => "." + c).join(""), 128);
  const data: Record<string, unknown> = { tag };
  if (id) data.id = truncate(id, 60);
  if (classes.length) data.classes = classes;
  const role = typeof el?.getAttribute === "function" ? el.getAttribute("role") : null;
  if (typeof role === "string" && role) data.role = truncate(role, 40);
  return { message, data };
}

export function createRecorder(options: BreadcrumbOptions = {}, report?: (err: Error) => void): BreadcrumbRecorder {
  const max = clampInt(options.maxBreadcrumbs, DEFAULT_MAX_BREADCRUMBS, 1, 1000);
  const ignore = (options.ignoreUrls ?? []).filter((u) => typeof u === "string" && u !== "");
  const ring: Breadcrumb[] = [];
  const undo: Array<() => void> = [];
  let active = false;

  function add(input: BreadcrumbInput): void {
    try {
      if (!input || typeof input.message !== "string") return;
      let crumb: Breadcrumb = {
        type: truncate(String(input.type ?? "default"), 64),
        category: truncate(String(input.category ?? "default"), 64),
        message: truncate(input.message, MAX_MESSAGE),
        timestamp:
          typeof input.timestamp === "number" && Number.isFinite(input.timestamp)
            ? Math.floor(input.timestamp)
            : Date.now(),
        level: LEVELS.has(String(input.level)) ? (input.level as BreadcrumbLevel) : "info",
      };
      const data = cloneData(input.data);
      if (data) crumb.data = data;
      if (options.beforeBreadcrumb) {
        let out: Breadcrumb | null | undefined;
        try {
          out = options.beforeBreadcrumb(crumb);
        } catch (err) {
          report?.(err instanceof Error ? err : new Error(String(err)));
          return;
        }
        if (!out) return;
        // Re-normalize: the hook may hand back anything.
        crumb = {
          type: truncate(String(out.type ?? crumb.type), 64),
          category: truncate(String(out.category ?? crumb.category), 64),
          message: truncate(String(out.message ?? ""), MAX_MESSAGE),
          timestamp: typeof out.timestamp === "number" && Number.isFinite(out.timestamp) ? Math.floor(out.timestamp) : crumb.timestamp,
          level: LEVELS.has(String(out.level)) ? out.level : crumb.level,
        };
        const d = cloneData(out.data);
        if (d) crumb.data = d;
      }
      ring.push(crumb);
      if (ring.length > max) ring.splice(0, ring.length - max);
    } catch {
      /* recording must never throw into host code */
    }
  }

  function snapshot(): Breadcrumb[] {
    const out: Breadcrumb[] = [];
    let bytes = 2;
    for (let i = ring.length - 1; i >= 0; i--) {
      const c = ring[i];
      const size = JSON.stringify(c).length + 1;
      if (bytes + size > MAX_ATTACH_BYTES) break;
      bytes += size;
      out.push({ ...c, ...(c.data ? { data: { ...c.data } } : {}) });
    }
    return out.reverse();
  }

  /** Replace obj[name] with wrap(original); reversible and chain-safe. */
  function patch(obj: any, name: string, wrap: (orig: AnyFn) => AnyFn): void {
    if (!obj || typeof obj[name] !== "function") return;
    const orig: AnyFn = obj[name];
    let wrapper: AnyFn;
    try {
      wrapper = wrap(orig);
      obj[name] = wrapper;
    } catch {
      return;
    }
    if (obj[name] !== wrapper) return; // frozen/readonly: nothing was patched
    undo.push(() => {
      try {
        if (obj[name] === wrapper) obj[name] = orig;
      } catch {
        /* restore best-effort */
      }
    });
  }

  function ignored(url: string): boolean {
    for (const p of ignore) if (url.startsWith(p)) return true;
    return false;
  }

  function listen(target: any, type: string, fn: (e: any) => void, capture = false): void {
    if (!target || typeof target.addEventListener !== "function") return;
    target.addEventListener(type, fn, capture);
    undo.push(() => {
      try {
        target.removeEventListener(type, fn, capture);
      } catch {
        /* ignore */
      }
    });
  }

  function install(): boolean {
    if (active) return true;
    if (typeof window === "undefined") return false;
    active = true;
    const w: any = window;

    if (options.console !== false && typeof console !== "undefined") {
      const levels: Array<[string, BreadcrumbLevel]> = [
        ["log", "info"], ["info", "info"], ["warn", "warning"], ["error", "error"], ["debug", "debug"],
      ];
      for (const [name, level] of levels) {
        patch(console, name, (orig) => function (this: unknown, ...args: unknown[]) {
          if (active) {
            try {
              add({ type: "console", category: "console", level, message: args.map(safeStringify).join(" ") });
            } catch { /* never throw */ }
          }
          return orig.apply(this, args);
        });
      }
    }

    if (options.clicks !== false) {
      listen(typeof document !== "undefined" ? document : null, "click", (e: any) => {
        if (!active) return;
        try {
          const t = e?.target;
          if (!t) return;
          const d = describeElement(t);
          if (options.clickText === true && typeof t.textContent === "string") {
            const text = t.textContent.trim().slice(0, 64);
            if (text) d.data.text = text;
          }
          add({ type: "user", category: "click", message: d.message, data: d.data });
        } catch { /* never throw */ }
      }, true);
    }

    if (options.navigation !== false) {
      const cur = (): string => (typeof location !== "undefined" ? sanitizeUrl(location.href) : "");
      let last = cur();
      const nav = (): void => {
        if (!active) return;
        try {
          const to = cur();
          if (to === last) return;
          add({ type: "navigation", category: "navigation", message: to, data: { from: last, to } });
          last = to;
        } catch { /* never throw */ }
      };
      const h: any = typeof history !== "undefined" ? history : null;
      for (const name of ["pushState", "replaceState"]) {
        patch(h, name, (orig) => function (this: unknown, ...args: unknown[]) {
          const r = orig.apply(this, args);
          nav();
          return r;
        });
      }
      listen(w, "popstate", nav);
      listen(w, "hashchange", nav);
    }

    if (options.fetch !== false && typeof w.fetch === "function") {
      patch(w, "fetch", (orig) => function (this: unknown, ...args: unknown[]) {
        let method = "GET";
        let url = "";
        let skip = true;
        try {
          const input: any = args[0];
          const init: any = args[1];
          const rawUrl = typeof input === "string" ? input : input && typeof input.url === "string" ? input.url : String(input ?? "");
          method = String(init?.method ?? input?.method ?? "GET").toUpperCase();
          url = sanitizeUrl(rawUrl);
          skip = !active || url === "" || ignored(url) || ignored(rawUrl);
        } catch { skip = true; }
        const p = orig.apply(this, args);
        if (!skip && p && typeof p.then === "function") {
          // Observe without altering the caller's promise; both branches
          // swallow so this derived promise can never be unhandled.
          p.then(
            (res: any) => {
              const status = typeof res?.status === "number" ? res.status : 0;
              add({ type: "http", category: "fetch", level: status >= 400 ? "error" : "info", message: `${method} ${url}`, data: { method, url, status_code: status } });
            },
            (err: unknown) => {
              add({ type: "http", category: "fetch", level: "error", message: `${method} ${url}`, data: { method, url, status_code: 0, error: truncate(safeStringify(err), 128) } });
            },
          );
        }
        return p;
      });
    }

    const X: any = (w as any).XMLHttpRequest;
    if (options.xhr !== false && X && X.prototype) {
      const meta = new WeakMap<object, { method: string; url: string; skip: boolean }>();
      patch(X.prototype, "open", (orig) => function (this: any, ...args: any[]) {
        try {
          const rawUrl = String(args[1] ?? "");
          const url = sanitizeUrl(rawUrl);
          meta.set(this, { method: String(args[0] ?? "GET").toUpperCase(), url, skip: url === "" || ignored(url) || ignored(rawUrl) });
        } catch { /* never throw */ }
        return orig.apply(this, args);
      });
      patch(X.prototype, "send", (orig) => function (this: any, ...args: any[]) {
        try {
          const m = meta.get(this);
          if (m && !m.skip && typeof this.addEventListener === "function") {
            const done = () => {
              if (!active) return;
              const status = typeof this.status === "number" ? this.status : 0;
              add({ type: "http", category: "xhr", level: status === 0 || status >= 400 ? "error" : "info", message: `${m.method} ${m.url}`, data: { method: m.method, url: m.url, status_code: status } });
            };
            this.addEventListener("loadend", done, { once: true });
          }
        } catch { /* never throw */ }
        return orig.apply(this, args);
      });
    }
    return true;
  }

  function uninstall(): void {
    active = false;
    while (undo.length) {
      const fn = undo.pop()!;
      try {
        fn();
      } catch { /* ignore */ }
    }
  }

  return {
    add,
    snapshot,
    clear: () => { ring.length = 0; },
    install,
    uninstall,
    get installed() { return active; },
  };
}
