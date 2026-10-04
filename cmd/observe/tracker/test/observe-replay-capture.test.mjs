// Contract tests for opt-in console/network replay capture. Loads the REAL
// served script in a vm sandbox with fake console/fetch/XHR.
// Run: node --test cmd/observe/tracker/test/
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";
import vm from "node:vm";

const here = path.dirname(fileURLToPath(import.meta.url));
const src = readFileSync(path.join(here, "..", "observe-replay-capture.js"), "utf8");

function makeEnv({ attrs = {}, fetchImpl, sink = true } = {}) {
  const pushed = [];
  const logged = [];
  const consoleObj = {};
  for (const l of ["log", "info", "warn", "error", "debug"]) {
    consoleObj[l] = (...a) => { logged.push([l, a]); };
  }
  class XHR {
    constructor() { this.listeners = {}; this.status = 200; }
    open() { this.opened = true; }
    send() { this.sent = true; }
    addEventListener(t, fn) { (this.listeners[t] ||= []).push(fn); }
    getResponseHeader(h) { return h === "content-length" ? "42" : null; }
    finish(status) { this.status = status; (this.listeners.loadend || []).forEach((f) => f()); }
  }
  const origFetch = fetchImpl || (async () => ({ status: 201, headers: { get: () => "123" } }));
  const window = {
    fetch: origFetch,
    observeReplay: sink ? { pushEvent: (type, data) => { pushed.push({ type, data }); return true; } } : undefined,
  };
  const sandbox = {
    window, console: consoleObj, XMLHttpRequest: XHR, URL, WeakMap, Date, Math, JSON, Object, Array, Error, String, Number, isFinite, parseInt,
    location: { href: "https://app.test/page", origin: "https://app.test" },
    document: { currentScript: { src: "https://observe.test/t/observe-replay-capture.js", getAttribute: (n) => (n in attrs ? attrs[n] : null) } },
  };
  sandbox.Request = class Request {};
  vm.createContext(sandbox);
  vm.runInContext(src, sandbox);
  return { sandbox, window, consoleObj, XHR, pushed, logged, origFetch };
}

const tick = () => new Promise((r) => setImmediate(r));

test("off by default: nothing is patched", () => {
  const env = makeEnv();
  assert.equal(env.window.fetch, env.origFetch);
  assert.equal(env.window.observeReplayCapture, undefined);
  env.consoleObj.log("hi");
  assert.equal(env.pushed.length, 0);
  assert.equal(env.XHR.prototype.open.name, "open");
});

test("console: levels captured, original still called, debug ignored", () => {
  const env = makeEnv({ attrs: { "data-console": "true" } });
  env.consoleObj.log("a", 1, { x: 2 });
  env.consoleObj.warn("w");
  env.consoleObj.debug("d");
  assert.deepEqual(env.pushed.map((p) => p.data.level), ["log", "warn"]);
  assert.equal(env.pushed[0].data.message, "a 1 {x: 2}");
  assert.equal(env.logged.length, 3);
  // network stays off
  assert.equal(env.window.fetch, env.origFetch);
});

test("console: scrub secrets", () => {
  const env = makeEnv({ attrs: { "data-console": "true" } });
  env.consoleObj.error("login password=hunter2", { token: "abc12345" }, "Authorization: Bearer abcdef123456");
  env.consoleObj.log("eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NSJ9.c2lnbmF0dXJl");
  const all = env.pushed.map((p) => p.data.message).join("\n");
  for (const s of ["hunter2", "abc12345", "abcdef123456", "eyJhbGci"]) assert.ok(!all.includes(s), s);
});

test("console: 1 KiB cut (bytes) and truncated flag", () => {
  const env = makeEnv({ attrs: { "data-console": "true" } });
  env.consoleObj.log("é".repeat(5000));
  const d = env.pushed[0].data;
  assert.ok(Buffer.byteLength(d.message, "utf8") <= 1024);
  assert.equal(d.truncated, true);
});

test("console: 200 per session cap", () => {
  const env = makeEnv({ attrs: { "data-console": "true" } });
  for (let i = 0; i < 300; i++) env.consoleObj.log("m" + i);
  assert.equal(env.pushed.length, 200);
  assert.equal(env.logged.length, 300);
});

test("console: circular, deep, getter-throwing args never throw", () => {
  const env = makeEnv({ attrs: { "data-console": "true" } });
  const c = { a: 1 }; c.self = c;
  const bad = { get boom() { throw new Error("no"); } };
  const deep = { a: { b: { c: { d: { e: 1 } } } } };
  assert.doesNotThrow(() => env.consoleObj.log(c, bad, deep, new Error("e"), Symbol("s"), () => 1));
  assert.match(env.pushed[0].data.message, /\[Circular\]/);
  assert.match(env.pushed[0].data.message, /\[Object\]/);
});

test("console: observe-replay diagnostics are not fed back", () => {
  const env = makeEnv({ attrs: { "data-console": "true" } });
  env.consoleObj.warn("observe-replay delivery failed:", new Error("x"));
  assert.equal(env.pushed.length, 0);
});

test("never throws into the page when the sink throws or is absent", () => {
  const env = makeEnv({ attrs: { "data-console": "true" } });
  env.window.observeReplay.pushEvent = () => { throw new Error("sink"); };
  assert.doesNotThrow(() => env.consoleObj.log("x"));
  const none = makeEnv({ attrs: { "data-console": "true", "data-network": "true" }, sink: false });
  assert.doesNotThrow(() => none.consoleObj.log("x"));
  assert.equal(none.pushed.length, 0);
});

test("network fetch: method, stripped URL, status, duration, size; no body/headers", async () => {
  const env = makeEnv({ attrs: { "data-network": "true" } });
  const res = await env.window.fetch("https://api.test/v1/items?token=SECRET#frag", { method: "post", body: "secret-body", headers: { Authorization: "x" } });
  assert.equal(res.status, 201);
  await tick();
  assert.equal(env.pushed.length, 1);
  const d = env.pushed[0].data;
  assert.equal(env.pushed[0].type, "network");
  assert.equal(d.method, "POST");
  assert.equal(d.url, "https://api.test/v1/items");
  assert.equal(d.status, 201);
  assert.equal(d.size, 123);
  assert.equal(d.kind, "fetch");
  assert.deepEqual(Object.keys(d).sort(), ["duration_ms", "kind", "method", "size", "status", "url"]);
  assert.ok(!JSON.stringify(d).includes("SECRET"));
});

test("network fetch: rejection recorded as status 0 and still rejects", async () => {
  const env = makeEnv({ attrs: { "data-network": "true" }, fetchImpl: async () => { throw new TypeError("net"); } });
  await assert.rejects(env.window.fetch("/rel/path"), TypeError);
  await tick();
  assert.equal(env.pushed[0].data.status, 0);
  assert.equal(env.pushed[0].data.url, "https://app.test/rel/path");
});

test("network: opaque path segments masked; observe's own ingest skipped; non-http dropped", async () => {
  const env = makeEnv({ attrs: { "data-network": "true" } });
  await env.window.fetch("https://api.test/reset/Abcdef0123456789Abcdef0123456789/go");
  await env.window.fetch("https://observe.test/api/v1/replays");
  await env.window.fetch("data:text/plain,hello");
  await tick();
  assert.equal(env.pushed.length, 1);
  assert.equal(env.pushed[0].data.url, "https://api.test/reset/:redacted/go");
});

test("network XHR captured on loadend", () => {
  const env = makeEnv({ attrs: { "data-network": "true" } });
  const x = new env.sandbox.XMLHttpRequest();
  x.open("get", "https://api.test/x?a=1");
  x.send("body-not-recorded");
  assert.ok(x.opened && x.sent);
  x.finish(500);
  const d = env.pushed[0].data;
  assert.deepEqual([d.method, d.url, d.status, d.size, d.kind], ["GET", "https://api.test/x", 500, 42, "xhr"]);
});

test("network: 500 per session cap", async () => {
  const env = makeEnv({ attrs: { "data-network": "true" } });
  for (let i = 0; i < 600; i++) await env.window.fetch("https://api.test/n/" + i);
  await tick();
  assert.equal(env.pushed.length, 500);
});

test("reversible: stop() restores console, fetch and XHR and silences capture", async () => {
  const env = makeEnv({ attrs: { "data-console": "true", "data-network": "true" } });
  const patchedLog = env.consoleObj.log;
  const protoOpen = env.XHR.prototype.open;
  assert.notEqual(env.window.fetch, env.origFetch);
  env.window.observeReplayCapture.stop();
  assert.equal(env.window.fetch, env.origFetch);
  assert.notEqual(env.consoleObj.log, patchedLog);
  assert.notEqual(env.XHR.prototype.open, protoOpen);
  env.consoleObj.log("after");
  await env.window.fetch("https://api.test/after");
  assert.equal(env.pushed.length, 0);
});

test("stop() leaves a later third-party wrapper in place", () => {
  const env = makeEnv({ attrs: { "data-network": "true" } });
  const theirs = () => {};
  env.window.fetch = theirs;
  env.window.observeReplayCapture.stop();
  assert.equal(env.window.fetch, theirs);
});
