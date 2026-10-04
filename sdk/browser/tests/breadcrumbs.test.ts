/**
 * Breadcrumb recorder + SDK integration tests. The recorder is exercised
 * against stubbed browser globals so patching/unpatching is observable.
 */

import { test, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";

import { createRecorder, describeElement, sanitizeUrl } from "../src/breadcrumbs.js";
import { init, addBreadcrumb, captureException, captureMessage, clearBreadcrumbs } from "../src/index.js";

const g: any = globalThis;
const saved: Record<string, any> = {};
const KEYS = ["window", "document", "history", "location", "console", "fetch", "XMLHttpRequest"];

class FakeXHR {
  static listeners: Array<() => void> = [];
  status = 0;
  private ls: Record<string, Array<() => void>> = {};
  open(_m: string, _u: string): void {}
  send(): void {}
  addEventListener(t: string, fn: () => void): void { (this.ls[t] ??= []).push(fn); }
  fire(status: number): void { this.status = status; (this.ls["loadend"] ?? []).forEach((f) => f()); }
}

let docListeners: Record<string, Array<(e: any) => void>>;
let winListeners: Record<string, Array<(e: any) => void>>;
let consoleCalls: any[][];
let origPush: any;

function stubEnv(): void {
  for (const k of KEYS) saved[k] = Object.getOwnPropertyDescriptor(g, k);
  docListeners = {};
  winListeners = {};
  consoleCalls = [];
  origPush = function (this: any, ..._a: any[]) { g.location.href = "https://app.test" + (_a[2] ?? "/"); return "pushed"; };
  g.location = { href: "https://app.test/home?token=secret#frag", origin: "https://app.test", pathname: "/home", search: "?token=secret" };
  g.history = { pushState: origPush, replaceState: function () { return undefined; } };
  g.document = {
    referrer: "", title: "t",
    addEventListener: (t: string, f: any) => { (docListeners[t] ??= []).push(f); },
    removeEventListener: (t: string, f: any) => { docListeners[t] = (docListeners[t] ?? []).filter((x) => x !== f); },
  };
  g.console = {
    log: (...a: any[]) => consoleCalls.push(["log", ...a]),
    info: (...a: any[]) => consoleCalls.push(["info", ...a]),
    warn: (...a: any[]) => consoleCalls.push(["warn", ...a]),
    error: (...a: any[]) => consoleCalls.push(["error", ...a]),
    debug: (...a: any[]) => consoleCalls.push(["debug", ...a]),
  };
  g.fetch = async () => ({ ok: true, status: 201 });
  g.XMLHttpRequest = FakeXHR;
  g.window = {
    fetch: g.fetch,
    XMLHttpRequest: FakeXHR,
    addEventListener: (t: string, f: any) => { (winListeners[t] ??= []).push(f); },
    removeEventListener: (t: string, f: any) => { winListeners[t] = (winListeners[t] ?? []).filter((x) => x !== f); },
  };
  // The recorder patches window.fetch; mirror it onto the global like a browser.
  Object.defineProperty(g, "fetch", { get: () => g.window.fetch, set: (v) => { g.window.fetch = v; }, configurable: true });
}

function restoreEnv(): void {
  for (const k of KEYS) {
    delete g[k];
    if (saved[k]) Object.defineProperty(g, k, saved[k]);
  }
}

beforeEach(stubEnv);
afterEach(restoreEnv);

test("console levels are recorded and the original still runs", () => {
  const origWarn = g.console.warn;
  const r = createRecorder();
  assert.equal(r.install(), true);
  g.console.log("hello", { a: 1 });
  g.console.warn("careful");
  g.console.error(new Error("boom"));
  g.console.debug("dbg");
  const s = r.snapshot();
  assert.deepEqual(s.map((c) => [c.category, c.level]), [
    ["console", "info"], ["console", "warning"], ["console", "error"], ["console", "debug"],
  ]);
  assert.equal(s[0].message, 'hello {"a":1}');
  assert.equal(s[2].message, "Error: boom");
  assert.equal(consoleCalls.length, 4, "original console methods ran");
  assert.equal(typeof s[0].timestamp, "number");
  r.uninstall();
  assert.equal(g.console.warn, origWarn, "console restored");
});

test("click describes the target without text by default", () => {
  const r = createRecorder();
  r.install();
  const el = { tagName: "BUTTON", id: "save", className: "btn primary extra more", textContent: "Pay $100 for jane@x.com", getAttribute: () => null };
  docListeners["click"][0]({ target: el });
  const [c] = r.snapshot();
  assert.equal(c.message, "button#save.btn.primary.extra");
  assert.equal(JSON.stringify(c).includes("jane@x.com"), false);
  assert.equal(c.data?.text, undefined);
  r.uninstall();

  const r2 = createRecorder({ clickText: true });
  r2.install();
  docListeners["click"][0]({ target: el });
  assert.equal(r2.snapshot()[0].data?.text, "Pay $100 for jane@x.com");
  r2.uninstall();
});

test("navigation: pushState and popstate record sanitized urls", () => {
  const r = createRecorder();
  r.install();
  g.history.pushState({}, "", "/next?x=1");
  assert.equal(g.history.pushState === origPush, false);
  g.location.href = "https://app.test/back?y=2";
  g.location.pathname = "/back";
  winListeners["popstate"][0]({});
  const s = r.snapshot();
  assert.equal(s.length, 2);
  assert.deepEqual(s[0].data, { from: "https://app.test/home", to: "https://app.test/next" });
  assert.equal(s[1].message, "https://app.test/back");
  assert.equal(JSON.stringify(s).includes("secret"), false);
  r.uninstall();
  assert.equal(g.history.pushState, origPush, "pushState restored");
});

test("fetch records method, status and url without query; promise passes through", async () => {
  const seen: any[] = [];
  g.window.fetch = async (...a: any[]) => { seen.push(a); return { ok: false, status: 503 }; };
  const r = createRecorder();
  r.install();
  const res = await g.window.fetch("https://api.test/v1/items?api_key=SECRET&q=1", { method: "post" });
  assert.equal(res.status, 503);
  assert.equal(seen.length, 1);
  await Promise.resolve();
  const [c] = r.snapshot();
  assert.equal(c.message, "POST https://api.test/v1/items");
  assert.deepEqual(c.data, { method: "POST", url: "https://api.test/v1/items", status_code: 503 });
  assert.equal(c.level, "error");
  assert.equal(JSON.stringify(c).includes("SECRET"), false);
  r.uninstall();
});

test("fetch rejection is recorded and still rejects to the caller; ignored urls are skipped", async () => {
  g.window.fetch = async () => { throw new TypeError("network down"); };
  const r = createRecorder({ ignoreUrls: ["https://observe.test"] });
  r.install();
  await assert.rejects(() => g.window.fetch("https://api.test/x"), /network down/);
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(r.snapshot()[0].data?.status_code, 0);
  r.clear();
  await assert.rejects(() => g.window.fetch("https://observe.test/api/v1/errors"), /network down/);
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(r.snapshot().length, 0);
  r.uninstall();
});

test("xhr records method, status and url without query", () => {
  const r = createRecorder();
  r.install();
  const x = new FakeXHR();
  x.open("get", "https://api.test/a?token=SECRET");
  x.send();
  x.fire(404);
  const [c] = r.snapshot();
  assert.equal(c.message, "GET https://api.test/a");
  assert.equal(c.data?.status_code, 404);
  assert.equal(JSON.stringify(c).includes("SECRET"), false);
  r.uninstall();
  assert.equal((FakeXHR.prototype as any).open.name === "open", true, "open restored");
});

test("ring buffer drops the oldest beyond the cap", () => {
  const r = createRecorder({ maxBreadcrumbs: 3 });
  for (let i = 0; i < 5; i++) r.add({ message: "m" + i });
  assert.deepEqual(r.snapshot().map((c) => c.message), ["m2", "m3", "m4"]);
  const d = createRecorder();
  for (let i = 0; i < 150; i++) d.add({ message: "m" + i });
  assert.equal(d.snapshot().length, 100, "default cap is 100");
});

test("beforeBreadcrumb can edit, drop, and a throwing hook fails closed", () => {
  const errs: Error[] = [];
  const r = createRecorder({
    beforeBreadcrumb: (c) => {
      if (c.message === "drop") return null;
      if (c.message === "throw") throw new Error("hook bug");
      return { ...c, message: c.message.toUpperCase() };
    },
  }, (e) => errs.push(e));
  r.add({ message: "keep" });
  r.add({ message: "drop" });
  r.add({ message: "throw" });
  assert.deepEqual(r.snapshot().map((c) => c.message), ["KEEP"]);
  assert.equal(errs.length, 1);
});

test("uninstall is chain-safe when another tool patched on top", () => {
  const r = createRecorder();
  r.install();
  const ours = g.console.log;
  const calls: string[] = [];
  const theirs = function (...a: any[]) { calls.push("theirs"); return ours.apply(g.console, a); };
  g.console.log = theirs;
  r.uninstall();
  assert.equal(g.console.log, theirs, "a later patch is left in place");
  g.console.log("x");
  assert.deepEqual(calls, ["theirs"]);
  assert.equal(r.snapshot().length, 0, "inactive wrapper records nothing");
  assert.equal(consoleCalls.length, 1, "and still passes through");
});

test("install and uninstall are idempotent; no window means no install", () => {
  const r = createRecorder();
  r.install();
  r.install();
  g.console.log("once");
  assert.equal(r.snapshot().length, 1);
  r.uninstall();
  r.uninstall();
  delete g.window;
  const r2 = createRecorder();
  assert.equal(r2.install(), false);
  r2.add({ message: "manual works without window" });
  assert.equal(r2.snapshot().length, 1);
});

test("a throwing console.log argument never breaks host code", () => {
  const r = createRecorder();
  r.install();
  const evil = { toJSON() { throw new Error("nope"); }, toString() { throw new Error("nope2"); } };
  assert.doesNotThrow(() => g.console.log(evil));
  assert.equal(consoleCalls.length, 1);
  r.uninstall();
});

test("data is size-capped and non-serializable data is replaced", () => {
  const r = createRecorder();
  const cyc: any = {}; cyc.self = cyc;
  r.add({ message: "a", data: cyc });
  r.add({ message: "b", data: { big: "x".repeat(5000) } });
  const s = r.snapshot();
  assert.deepEqual(s[0].data, { _unserializable: true });
  assert.equal(s[1].data?._truncated, true);
});

test("describeElement and sanitizeUrl", () => {
  assert.equal(describeElement({ tagName: "A", className: { baseVal: "svg" } }).message, "a");
  assert.equal(sanitizeUrl("https://u:p@a.test/p?q=1#h"), "https://a.test/p");
  assert.equal(sanitizeUrl("javascript:alert(1)"), "");
});

// --- SDK integration -------------------------------------------------------

test("captureException and captureMessage carry breadcrumbs in the ingest shape", async () => {
  delete g.window; // plain Node: manual breadcrumbs only (SSR-safe)
  const sent: any[] = [];
  Object.defineProperty(g, "fetch", { value: async (url: string, o: any) => { sent.push({ url, body: JSON.parse(o.body) }); return { ok: true, json: async () => ({}) }; }, configurable: true, writable: true });
  init({ endpoint: "https://observe.test", siteId: "s", apiKey: "k", breadcrumbs: { maxBreadcrumbs: 2 } });
  addBreadcrumb({ type: "user", category: "checkout", message: "one" });
  addBreadcrumb({ message: "two", level: "warning", data: { n: 2 } });
  addBreadcrumb({ message: "three" });
  await captureException(new Error("bad"));
  await captureMessage("hello", { level: "warning" });
  const errs = sent.filter((s) => s.url.endsWith("/api/v1/errors"));
  assert.equal(errs.length, 2);
  const b = errs[0].body.breadcrumbs;
  assert.deepEqual(b.map((c: any) => c.message), ["two", "three"]);
  assert.deepEqual(Object.keys(b[0]).sort(), ["category", "data", "level", "message", "timestamp", "type"]);
  assert.equal(b[0].level, "warning");
  assert.equal(errs[0].body.level, "error");
  assert.equal(errs[1].body.level, "warning");
  assert.equal(errs[1].body.error_value, "hello");
  assert.equal(errs[1].body.breadcrumbs.length, 2);
  clearBreadcrumbs();
  sent.length = 0;
  await captureException(new Error("again"));
  assert.equal(sent[0].body.breadcrumbs, undefined, "no key when empty");
});
