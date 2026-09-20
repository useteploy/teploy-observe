// Contract tests for the classic analytics tracker (F41 URL/sensitive-data
// contract). Loads the REAL served script in a vm sandbox with a stubbed DOM
// and asserts what leaves the page: origin+path URLs, allowlisted utm fields
// as explicit payload fields, and element text only under opt-in.
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";
import vm from "node:vm";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const src = readFileSync(path.join(root, "cmd/observe/tracker/observe.js"), "utf8");

function makeSandbox({ fetchImpl, attributes = {}, search = "", href = "https://app.test/page" } = {}) {
  // Keyed installs are the only deliverable ones (AUD-002 removed keyless
  // ingest; the classic tracker's keyless beacon fallback went with it,
  // TO-051/R31 parity), so the harness defaults to a configured key.
  if (!("data-api-key" in attributes)) attributes = { "data-api-key": "obs_test_key", ...attributes };
  const listeners = [];
  const document = {
    currentScript: {
      src: "https://observe.test/t/observe.js",
      getAttribute: (n) => (n in attributes ? attributes[n] : null),
    },
    addEventListener: (t, fn) => listeners.push([t, fn]),
    removeEventListener: () => {},
    visibilityState: "visible",
    title: "Page title",
    referrer: "https://ref.test/from?x=1",
  };
  const bodies = [];
  const sandbox = {
    console,
    document,
    history: { pushState: function () {}, replaceState: function () {} },
    location: {
      origin: "https://app.test",
      pathname: "/page",
      href,
      search,
    },
    navigator: { userAgent: "node-test", language: "en-US" },
    screen: { width: 1920, height: 1080 },
    setInterval: () => 1,
    clearInterval: () => {},
    setTimeout: (fn) => { fn(); return 0; },
    clearTimeout: () => {},
    localStorage: { getItem: () => null, setItem: () => {}, removeItem: () => {} },
    addEventListener: (t, fn) => listeners.push(["window:" + t, fn]),
    removeEventListener: () => {},
    fetch:
      fetchImpl ||
      ((url, opts) => {
        bodies.push({ url, body: JSON.parse(opts.body) });
        return Promise.resolve({ ok: true });
      }),
    XMLHttpRequest: function () { throw new Error("XHR not expected in these tests"); },
    Blob: class { constructor(parts) { this.size = String(parts[0]).length; } },
    URL,
    TextEncoder,
  };
  sandbox.window = sandbox;
  vm.runInNewContext(src, sandbox, { filename: "observe.js" });
  return { sandbox, listeners, bodies, document };
}

function firstEvent(bodies) {
  assert.ok(bodies.length >= 1, "a flush was issued");
  return bodies[0].body.events[0];
}

// F41: the pageview URL is origin+path — the query string (and any fragment
// or credentials) never leaves the page; the allowlisted utm params ride as
// explicit fields instead.
test("pageview sends origin+path and explicit utm fields, never the raw query", async () => {
  const { sandbox, bodies } = makeSandbox({
    attributes: { "data-site-id": "s1" },
    search: "?utm_source=hn&utm_campaign=launch&utm_content=cta&email=me@x.io&token=hunter2",
  });
  await new Promise((r) => setTimeout(r, 10));
  const ev = firstEvent(bodies);
  assert.equal(ev.url, "https://app.test/page", "url must be origin+path");
  assert.equal(ev.utm_source, "hn");
  assert.equal(ev.utm_campaign, "launch");
  assert.equal(ev.utm_content, "cta");
  assert.equal(ev.utm_medium, undefined, "unset params are omitted");
  assert.equal(JSON.stringify(ev).includes("hunter2"), false, "non-allowlisted query params never leave the page");
  assert.equal(JSON.stringify(ev).includes("me@x.io"), false);
});

// F41: element text is opt-in — the default autocapture sends no text.
test("autocaptured clicks omit element text by default", async () => {
  const { sandbox, listeners, bodies } = makeSandbox({
    attributes: { "data-site-id": "s1" },
  });
  await new Promise((r) => setTimeout(r, 5));
  const clickFn = listeners.filter(([t]) => t === "click")[0][1];
  clickFn({
    target: {
      tagName: "A",
      id: "",
      className: "",
      getAttribute: () => "https://out.test/x?token=z",
      textContent: "Buy now with secret token inside",
    },
    clientX: 1,
    clientY: 2,
  });
  await new Promise((r) => setTimeout(r, 10));
  const click = bodies.flatMap((b) => b.body.events).find((e) => e.event_type === "click");
  assert.ok(click, "click captured");
  assert.equal(click.properties.text, "", "text omitted without data-capture-text");
  assert.equal(click.properties.href, "https://out.test/x", "href reduced to origin+path");
});

test("autocaptured clicks include element text under data-capture-text opt-in", async () => {
  const { sandbox, listeners, bodies } = makeSandbox({
    attributes: { "data-site-id": "s1", "data-capture-text": "true" },
  });
  await new Promise((r) => setTimeout(r, 5));
  const clickFn = listeners.filter(([t]) => t === "click")[0][1];
  clickFn({
    target: {
      tagName: "BUTTON",
      id: "",
      className: "",
      getAttribute: () => "",
      textContent: "Confirm purchase",
    },
    clientX: 1,
    clientY: 2,
  });
  await new Promise((r) => setTimeout(r, 10));
  const click = bodies.flatMap((b) => b.body.events).find((e) => e.event_type === "click");
  assert.equal(click.properties.text, "Confirm purchase");
});

// F41: outbound link hrefs are sanitized to origin+path.
test("outbound clicks carry a sanitized href", async () => {
  const { sandbox, listeners, bodies } = makeSandbox({
    attributes: { "data-site-id": "s1" },
  });
  await new Promise((r) => setTimeout(r, 5));
  // The outbound listener is the second click listener registered.
  const clickFns = listeners.filter(([t]) => t === "click").map(([, fn]) => fn);
  const outboundFn = clickFns[clickFns.length - 1];
  const anchor = {
    tagName: "A",
    href: "https://elsewhere.com/lp?gclid=abc123&fbclid=x#frag",
    parentElement: null,
    textContent: "External",
    getAttribute: () => "https://elsewhere.com/lp?gclid=abc123&fbclid=x#frag",
  };
  outboundFn({ target: anchor });
  await new Promise((r) => setTimeout(r, 10));
  const outbound = bodies.flatMap((b) => b.body.events).find((e) => e.event_type === "outbound_click");
  assert.ok(outbound, "outbound captured");
  assert.equal(outbound.properties.href, "https://elsewhere.com/lp");
  assert.equal(JSON.stringify(outbound).includes("gclid"), false);
});

// --- Round 4 (R29/R31/R32/R33) classic-tracker delivery contracts ---

function delayedFetchImpl(responses) {
  // responses: array of {status?, ok?, delayMs?} consumed per request.
  let i = 0;
  const seen = [];
  const impl = (url, opts) => {
    const spec = responses[Math.min(i, responses.length - 1)] || { ok: true };
    i++;
    seen.push({ url, body: JSON.parse(opts.body) });
    const settle = spec.delayMs || 0;
    return new Promise((resolve) => {
      setTimeout(() => resolve(spec), settle);
    });
  };
  impl.seen = seen;
  return impl;
}

test("a malformed percent escape does not abort campaign parsing or capture (R33)", async () => {
  const { bodies } = makeSandbox({
    attributes: { "data-site-id": "s1" },
    search: "?%ZZ=broken&utm_source=valid&broken%2=%F0%9F&utm_medium=ok",
  });
  await new Promise((r) => setTimeout(r, 10));
  const ev = firstEvent(bodies);
  assert.equal(ev.utm_source, "valid", "allowlisted pair after the malformed one survives");
  assert.equal(ev.utm_medium, "ok");
});

test("referrer never crosses the wire raw (R29)", async () => {
  const { bodies } = makeSandbox({
    attributes: { "data-site-id": "s1" },
  });
  await new Promise((r) => setTimeout(r, 10));
  const ev = firstEvent(bodies);
  assert.equal(ev.referrer, "https://ref.test/from", "referrer reduced to origin+path client-side");
});

test("form_submit actions are sanitized and relative actions resolve (R29)", async () => {
  const { listeners, bodies } = makeSandbox({ attributes: { "data-site-id": "s1" } });
  await new Promise((r) => setTimeout(r, 5));
  const submitFn = listeners.filter(([t]) => t === "submit")[0][1];
  submitFn({ target: { tagName: "FORM", id: "checkout", getAttribute: () => "/submit?token=x" } });
  submitFn({ target: { tagName: "FORM", id: "", getAttribute: () => "" } });
  await new Promise((r) => setTimeout(r, 10));
  const forms = bodies.flatMap((b) => b.body.events).filter((e) => e.event_type === "form_submit");
  assert.equal(forms.length, 2);
  assert.equal(forms[0].properties.action, "https://app.test/submit", "relative action resolved, query stripped");
  assert.equal(forms[1].properties.action, "https://app.test/page", "empty action falls back to the page URL");
});

test("101+ events drain without a new capture (R31)", async () => {
  const fetchImpl = delayedFetchImpl([{ ok: true }, { ok: true }, { ok: true }]);
  const { sandbox } = makeSandbox({
    attributes: { "data-site-id": "s1", "data-auto-track": "false" },
    fetchImpl,
  });
  await new Promise((r) => setTimeout(r, 5));
  for (let i = 0; i < 101; i++) sandbox.observe.track("burst", { n: i });
  await new Promise((r) => setTimeout(r, 50));
  const batches = fetchImpl.seen.map((s) => s.body);
  const total = batches.reduce((n, b) => n + b.events.length, 0);
  assert.equal(total, 101, "every burst event delivered");
  for (const b of batches) assert.ok(b.events.length <= 100, "server batch cap respected");
});

test("a failed request retries the SAME frozen batch and then drains (R31)", async () => {
  const fetchImpl = delayedFetchImpl([{ ok: false, status: 503 }, { ok: true }, { ok: true }]);
  const { sandbox } = makeSandbox({
    attributes: { "data-site-id": "s1", "data-auto-track": "false" },
    fetchImpl,
  });
  await new Promise((r) => setTimeout(r, 5));
  const props = { text: "hello" };
  sandbox.observe.track("retry-me", props);
  props.text = "changed-after-capture";
  await new Promise((r) => setTimeout(r, 80));
  assert.equal(fetchImpl.seen.length, 2, "one retry then success");
  assert.equal(fetchImpl.seen[0].body.batch_id, fetchImpl.seen[1].body.batch_id, "batch identity stable across retry");
  assert.equal(JSON.stringify(fetchImpl.seen[0].body), JSON.stringify(fetchImpl.seen[1].body), "retried content identical");
  assert.equal(fetchImpl.seen[1].body.events[0].properties.text, "hello", "frozen at capture (R32)");
});

test("cyclic properties are a capture-time rejection, not a flush-time loss (R32)", async () => {
  const fetchImpl = delayedFetchImpl([{ ok: true }]);
  const { sandbox } = makeSandbox({ attributes: { "data-site-id": "s1" }, fetchImpl });
  await new Promise((r) => setTimeout(r, 5));
  const bad = {};
  bad.self = bad;
  assert.doesNotThrow(() => sandbox.observe.track("cyclic", bad));
  sandbox.observe.track("fine", { a: 1 });
  await new Promise((r) => setTimeout(r, 20));
  const types = fetchImpl.seen.flatMap((s) => s.body.events).map((e) => e.event_type);
  assert.ok(types.includes("fine"), "healthy events still deliver");
  assert.ok(!types.includes("cyclic"), "unserializable event never sent");
  assert.ok(sandbox.observe.droppedEvents() >= 1, "drop is visible in diagnostics");
});

test("revenue does not mutate the caller's properties (R32)", async () => {
  const fetchImpl = delayedFetchImpl([{ ok: true }, { ok: true }]);
  const { sandbox } = makeSandbox({ attributes: { "data-site-id": "s1" }, fetchImpl });
  await new Promise((r) => setTimeout(r, 5));
  const props = { plan: "pro" };
  sandbox.observe.revenue(4200, "USD", props);
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(props.amount, undefined, "caller object untouched");
  assert.equal(props.currency, undefined);
  const rev = fetchImpl.seen.flatMap((s) => s.body.events).find((e) => e.event_type === "revenue");
  assert.equal(rev.properties.amount, 4200);
  assert.equal(rev.properties.plan, "pro");
});

test("permanent rejection drops the batch and keeps delivering later events (R31)", async () => {
  const fetchImpl = delayedFetchImpl([{ ok: false, status: 400 }, { ok: true }]);
  const { sandbox } = makeSandbox({
    attributes: { "data-site-id": "s1", "data-auto-track": "false" },
    fetchImpl,
  });
  await new Promise((r) => setTimeout(r, 5));
  sandbox.observe.track("rejected-once");
  await new Promise((r) => setTimeout(r, 20));
  sandbox.observe.track("after");
  await new Promise((r) => setTimeout(r, 30));
  const types = fetchImpl.seen.flatMap((s) => s.body.events).map((e) => e.event_type);
  const rejections = types.filter((t) => t === "rejected-once").length;
  assert.equal(rejections, 1, "permanently rejected batch attempted exactly once, never retried");
  assert.ok(types.includes("after"), "queue keeps working after a permanent rejection");
});

test("keyless installs do not deliver (keyless ingest is gone)", async () => {
  const fetchImpl = delayedFetchImpl([{ ok: true }]);
  const { sandbox } = makeSandbox({
    attributes: { "data-site-id": "s1", "data-api-key": "" },
    fetchImpl,
  });
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(fetchImpl.seen.length, 0, "no keyless requests attempted");
  assert.ok(sandbox.observe.droppedEvents() >= 1);
});
