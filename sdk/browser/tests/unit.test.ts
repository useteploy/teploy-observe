/**
 * Unit tests for @teploy/observe-browser.
 *
 * Run with `npm test` (`node --test --import tsx tests/*.test.ts`). The SDK is
 * exercised in Node with stubbed browser globals and a stubbed fetch, so the
 * assertions are made against the exact JSON that would go over the wire.
 */

import { test, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";

import { init, track, pageview, identify, flush } from "../src/index.js";

type Sent = { url: string; body: any };

let sent: Sent[] = [];
const realFetch = globalThis.fetch;

function stubBrowser(): void {
  sent = [];
  (globalThis as any).fetch = (url: string, opts: any) => {
    sent.push({ url, body: JSON.parse(opts.body) });
    return Promise.resolve({ ok: true });
  };
  // Node's navigator has no sendBeacon, so post() falls through to fetch.
  (globalThis as any).location = {
    href: "https://example.com/pricing?utm_source=hn",
    pathname: "/pricing",
    search: "?utm_source=hn",
  };
  (globalThis as any).document = { referrer: "https://news.ycombinator.com/", title: "Pricing" };
}

/** Flush the buffer and return the single event that was sent. */
async function sentEvent(): Promise<any> {
  await flush();
  assert.equal(sent.length, 1, "expected exactly one request");
  assert.match(sent[0].url, /\/api\/v1\/events\/batch$/);
  assert.equal(sent[0].body.events.length, 1);
  const event = sent[0].body.events[0];
  sent = [];
  return event;
}

beforeEach(() => {
  stubBrowser();
  init({ endpoint: "https://observe.example.com", siteId: "s1", release: "v1.4.2" });
});

afterEach(() => {
  (globalThis as any).fetch = realFetch;
  delete (globalThis as any).location;
  delete (globalThis as any).document;
});

// Regression: track() used to spread custom props at the TOP LEVEL of the
// payload. The server reads properties only from a nested `properties` object,
// so every custom property was stored as {}.
test("track nests custom props under properties", async () => {
  track("signup", { plan: "pro", seats: 4 });
  const e = await sentEvent();

  assert.deepEqual(e.properties, { plan: "pro", seats: 4 });
  assert.equal(e.plan, undefined, "custom prop must not be a top-level key");
  assert.equal(e.seats, undefined, "custom prop must not be a top-level key");
  assert.equal(e.site_id, "s1");
  assert.equal(e.event_type, "signup");
  assert.equal(e.release, "v1.4.2", "release stays a top-level field");
});

test("track keeps server-read fields at the top level", async () => {
  track("custom", {
    url: "https://example.com/a",
    referrer: "https://ref/",
    title: "A",
    language: "en-US",
    screen: "1920x1080",
    distinct_id: "u_1",
    tier: "gold",
  });
  const e = await sentEvent();

  assert.equal(e.url, "https://example.com/a");
  assert.equal(e.referrer, "https://ref/");
  assert.equal(e.title, "A");
  assert.equal(e.language, "en-US");
  assert.equal(e.screen, "1920x1080");
  assert.equal(e.distinct_id, "u_1");
  assert.deepEqual(e.properties, { tier: "gold" });
});

// The naive "nest everything" fix would break pageview attribution: pageview()
// relies on url/referrer/title reaching the server as real fields.
test("pageview still populates the fields the server reads", async () => {
  pageview();
  const e = await sentEvent();

  assert.equal(e.event_type, "pageview");
  assert.equal(e.url, "https://example.com/pricing?utm_source=hn");
  assert.equal(e.referrer, "https://news.ycombinator.com/");
  assert.equal(e.title, "Pricing");
  assert.equal(e.properties, undefined, "an unannotated pageview carries no properties");
});

test("pageview carries an explicit pathname as a property", async () => {
  pageview("/checkout/step-2");
  const e = await sentEvent();

  assert.equal(e.url, "https://example.com/pricing?utm_source=hn");
  assert.deepEqual(e.properties, { pathname: "/checkout/step-2" });
});

test("identify sends traits as properties and stamps distinct_id", async () => {
  identify("u_123", { plan: "pro" });
  const e = await sentEvent();

  assert.equal(e.event_type, "$identify");
  assert.equal(e.distinct_id, "u_123");
  assert.deepEqual(e.properties, { user_id: "u_123", plan: "pro" });

  // Subsequent events carry the distinct_id too.
  track("checkout");
  const next = await sentEvent();
  assert.equal(next.distinct_id, "u_123");
});

test("properties are capped at the server limit of 50", async () => {
  const props: Record<string, unknown> = {};
  for (let i = 0; i < 80; i++) props[`k${String(i).padStart(2, "0")}`] = i;
  track("spam", props);
  const e = await sentEvent();

  assert.equal(Object.keys(e.properties).length, 50);
  assert.equal(e.properties.k00, 0);
  assert.equal(e.properties.k79, undefined);
});
