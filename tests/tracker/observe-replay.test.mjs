// Contract tests for the classic replay tracker (AUD-056 partial, round 2).
// Loads the REAL served script in a vm sandbox with a stubbed DOM and
// asserts externally observable behavior: lifecycle, delivery retention,
// rage-click reporting, per-click page context, and beacon policy.
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";
import vm from "node:vm";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const src = readFileSync(path.join(root, "cmd/observe/tracker/observe-replay.js"), "utf8");

function makeSandbox({ fetchImpl, apiKey } = {}) {
  const listeners = [];
  const document = {
    currentScript: {
      src: "https://observe.test/t/observe-replay.js",
      getAttribute: (n) => (n === "data-api-key" ? apiKey ?? null : null),
    },
    addEventListener: (t, fn) => listeners.push(["document", t, fn]),
    removeEventListener: (t, fn) => {
      const i = listeners.findIndex(([, tt, ff]) => tt === t && ff === fn);
      if (i >= 0) listeners.splice(i, 1);
    },
    visibilityState: "visible",
    doctype: { name: "html" },
    documentElement: {
      nodeType: 1, tagName: "HTML", attributes: [], childNodes: [
        { nodeType: 1, tagName: "BODY", attributes: [], childNodes: [
          { nodeType: 3, textContent: "hello" },
        ] },
      ],
    },
    body: { nodeType: 1, tagName: "BODY", attributes: [], childNodes: [] },
  };
  const history = { pushState: function () {}, replaceState: function () {} };
  const bodies = [];
  const sandbox = {
    console,
    document,
    history,
    location: { origin: "https://app.test", pathname: "/page", href: "https://app.test/page?q=1" },
    navigator: { userAgent: "node-test" },
    setInterval: () => 1,
    clearInterval: () => {},
    setTimeout,
    localStorage: { getItem: () => null },
    addEventListener: (t, fn) => listeners.push(["window", t, fn]),
    removeEventListener: (t, fn) => {
      const i = listeners.findIndex(([, tt, ff]) => tt === t && ff === fn);
      if (i >= 0) listeners.splice(i, 1);
    },
    fetch:
      fetchImpl ||
      ((url, opts) => {
        bodies.push({ url, body: JSON.parse(opts.body) });
        return Promise.resolve({ ok: true });
      }),
    XMLHttpRequest: function () { throw new Error("XHR not expected in these tests"); },
    Blob: class { constructor(parts) { this.size = String(parts[0]).length; } },
    TextEncoder,
    URL,
  };
  sandbox.window = sandbox;
  vm.runInNewContext(src, sandbox, { filename: "observe-replay.js" });
  return { sandbox, listeners, bodies };
}

function click(listeners, x, y) {
  const target = { nodeType: 1, tagName: "BUTTON", id: "", className: "", attributes: [] };
  const entry = listeners.filter(([, t]) => t === "click").at(-1);
  entry[2]({ target, clientX: x, clientY: y });
}

// AUD-027: stop() removes the listeners it installed.
test("stop removes the recorder's listeners", async () => {
  const { sandbox, listeners } = makeSandbox();
  const before = listeners.filter(([, t]) => t === "click").length;
  assert.ok(before >= 1, "click listener installed");
  sandbox.observeReplay.stop();
  const after = listeners.filter(([, t]) => t === "click").length;
  assert.equal(after, 0, "all click listeners removed on stop");
});

// AUD-024: a rejected response retains the batch instead of deleting it.
test("failed send retains events and reports through the hook", async () => {
  let fail = true;
  const bodies = [];
  let reported = null;
  const { sandbox, listeners } = makeSandbox({
    fetchImpl: (_url, opts) => {
      bodies.push(JSON.parse(opts.body));
      return fail ? Promise.resolve({ ok: false, status: 503 }) : Promise.resolve({ ok: true });
    },
  });
  sandbox.observeReplay.onError((e) => { reported = e; });
  click(listeners, 5, 5);
  sandbox.observeReplay.stop();
  await new Promise((r) => setTimeout(r, 10));
  assert.ok(bodies.length >= 1, "a flush was attempted");
  assert.ok(bodies[0].events.some((e) => e.type === "click"), "the click was in the failed batch");
  assert.match(String(reported), /503/, "failure surfaced through the error hook");
});

// AUD-034: one rage burst reports exactly once.
test("rage clicks report once per burst", async () => {
  const bodies = [];
  const { sandbox, listeners } = makeSandbox({
    fetchImpl: (_url, opts) => { bodies.push(JSON.parse(opts.body)); return Promise.resolve({ ok: true }); },
  });
  for (let i = 0; i < 4; i++) click(listeners, 1, 1);
  sandbox.observeReplay.stop();
  await new Promise((r) => setTimeout(r, 10));
  const rage = bodies.flatMap((b) => (b.events || []).filter((e) => e.type === "rage_click"));
  assert.equal(rage.length, 1, "one burst reports exactly once");
});

// AUD-033: clicks carry page_url captured at click time, sanitized to
// origin+path.
test("click events carry sanitized page context", async () => {
  const bodies = [];
  const { sandbox, listeners } = makeSandbox({
    fetchImpl: (_url, opts) => { bodies.push(JSON.parse(opts.body)); return Promise.resolve({ ok: true }); },
  });
  click(listeners, 3, 4);
  sandbox.observeReplay.stop();
  await new Promise((r) => setTimeout(r, 10));
  const clickEv = bodies.flatMap((b) => b.events).find((e) => e.type === "click");
  assert.ok(clickEv, "click captured");
  assert.equal(clickEv.data.page_url, "https://app.test/page");
});

// AUD-025: with an API key configured, sendBeacon is never used — it
// cannot carry the key header, so "queued" would still 401.
test("keyed configuration never queues a beacon", async () => {
  let beaconUsed = false;
  const { sandbox } = makeSandbox({
    apiKey: "sk-test",
    fetchImpl: () => Promise.resolve({ ok: true }),
  });
  sandbox.navigator.sendBeacon = () => { beaconUsed = true; return true; };
  sandbox.observeReplay.stop();
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(beaconUsed, false, "beacon must never be used when an API key is configured");
});

// F12/F19: batches carry v2 identity (producer_id + stable batch_id), and a
// failed chunk retries under the SAME batch id so the server ledger can
// dedupe it instead of double-counting children and heatmap clicks.
test("replay batches carry v2 identity and retry under the same batch id", async () => {
  let fail = true;
  const bodies = [];
  const { sandbox, listeners } = makeSandbox({
    fetchImpl: (_url, opts) => {
      bodies.push(JSON.parse(opts.body));
      return fail ? Promise.resolve({ ok: false, status: 503 }) : Promise.resolve({ ok: true });
    },
  });
  click(listeners, 8, 8);
  sandbox.observeReplay.stop(); // first flush -> 503, chunk retained
  await new Promise((r) => setTimeout(r, 10));
  assert.ok(bodies.length >= 1, "first attempt issued");
  assert.equal(bodies[0].v, 2, "protocol v2 stamped");
  assert.ok(bodies[0].producer_id, "producer_id present");
  assert.ok(bodies[0].batch_id, "batch_id present");

  fail = false;
  sandbox.observeReplay.start();
  sandbox.observeReplay.stop(); // retry flush with delivery restored
  await new Promise((r) => setTimeout(r, 10));
  // The retry is the body reusing the failed batch id (a fresh chunk for
  // the second start()'s snapshot may follow it).
  const retry = bodies.find((b, i) => i > 0 && b.batch_id === bodies[0].batch_id);
  assert.ok(retry, "the failed chunk must be retried under its original batch id");
  assert.equal(retry.producer_id, bodies[0].producer_id, "producer id stable");
  assert.deepEqual(
    retry.events.map((e) => [e.type, e.timestamp]),
    bodies[0].events.map((e) => [e.type, e.timestamp]),
    "retry carries the same events",
  );
});

// F39 (the smaller step): mid-session re-snapshots. A long session records
// fresh snapshots periodically and on mutation bursts (throttled), so the
// player's keyframe selection has something newer than the initial DOM to
// replay over. Drives the recorder's own MutationObserver callback.
test("mutation bursts and time trigger throttled re-snapshots", async () => {
  const bodies = [];
  let observerCb = null;
  const listeners = [];
  const document = {
    currentScript: {
      src: "https://observe.test/t/observe-replay.js",
      getAttribute: (n) => (n === "data-resnapshot-burst" ? "20" : n === "data-resnapshot-min-gap" ? "1000" : n === "data-resnapshot-interval" ? "5000" : null),
    },
    addEventListener: (t, fn) => listeners.push(["document", t, fn]),
    removeEventListener: () => {},
    visibilityState: "visible",
    doctype: { name: "html" },
    documentElement: {
      nodeType: 1, tagName: "HTML", attributes: [], childNodes: [
        { nodeType: 1, tagName: "BODY", attributes: [], childNodes: [
          { nodeType: 3, textContent: "hello" },
        ] },
      ],
    },
    body: { nodeType: 1, tagName: "BODY", attributes: [], childNodes: [] },
  };
  const sandbox = {
    console,
    document,
    history: { pushState: function () {}, replaceState: function () {} },
    location: { origin: "https://app.test", pathname: "/page", href: "https://app.test/page?q=1" },
    navigator: { userAgent: "node-test" },
    setInterval: () => 1,
    clearInterval: () => {},
    setTimeout,
    MutationObserver: class {
      constructor(cb) { observerCb = cb; }
      observe() {}
      disconnect() {}
    },
    localStorage: { getItem: () => null },
    addEventListener: (t, fn) => listeners.push(["window", t, fn]),
    removeEventListener: () => {},
    fetch: (_url, opts) => { bodies.push(JSON.parse(opts.body)); return Promise.resolve({ ok: true }); },
    XMLHttpRequest: function () { throw new Error("no XHR"); },
    Blob: class { constructor(parts) { this.size = String(parts[0]).length; } },
    TextEncoder,
    URL,
    Date,
  };
  sandbox.window = sandbox;
  vm.runInNewContext(src, sandbox, { filename: "observe-replay.js" });
  assert.ok(observerCb, "recorder installed a MutationObserver");

  // Cross the min-gap throttle (its parser floor is 1000 ms) before
  // driving the burst.
  await new Promise((r) => setTimeout(r, 1100));
  // 25 mutations: over the data-resnapshot-burst=20 threshold.
  const muts = Array.from({ length: 25 }, () => ({ type: "childList", addedNodes: { length: 1 } }));
  observerCb(muts);
  sandbox.observeReplay.stop();
  await new Promise((r) => setTimeout(r, 10));
  const snapshots = bodies.flatMap((b) => (b.events || []).filter((e) => e.type === "snapshot"));
  assert.ok(snapshots.length >= 2, `expected the initial snapshot plus a burst re-snapshot, got ${snapshots.length}`);
});
