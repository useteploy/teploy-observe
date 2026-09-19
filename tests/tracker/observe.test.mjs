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
