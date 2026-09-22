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
    // TO-035: the flush owner reads the batch acknowledgment.
    return Promise.resolve({ ok: true, json: async () => ({ ok: true }) });
  };
  // Node's navigator has no sendBeacon, so post() falls through to fetch.
  (globalThis as any).location = {
    href: "https://example.com/pricing?utm_source=hn",
    origin: "https://example.com",
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
  // F41: url is origin+path; the utm param rides as an explicit field.
  assert.equal(e.url, "https://example.com/pricing");
  assert.equal(e.utm_source, "hn");
  assert.equal(e.referrer, "https://news.ycombinator.com/");
  assert.equal(e.title, "Pricing");
  assert.equal(e.properties, undefined, "an unannotated pageview carries no properties");
});

test("pageview carries an explicit pathname as a property", async () => {
  pageview("/checkout/step-2");
  const e = await sentEvent();

  assert.equal(e.url, "https://example.com/pricing");
  assert.deepEqual(e.properties, { pathname: "/checkout/step-2" });
});

test("identify stamps distinct_id without duplicating the raw id into properties", async () => {
  identify("u_123", { plan: "pro" });
  const e = await sentEvent();

  assert.equal(e.event_type, "$identify");
  assert.equal(e.distinct_id, "u_123");
  // Audit F33: the raw id travels ONLY in the top-level field the server
  // hashes; identity-shaped trait keys never reach stored properties.
  assert.deepEqual(e.properties, { plan: "pro" });

  // Subsequent events carry the distinct_id too.
  track("checkout");
  const next = await sentEvent();
  assert.equal(next.distinct_id, "u_123");
});

test("identify filters identity-shaped trait keys", async () => {
  identify("u_123", { user_id: "u_123", distinct_id: "u_123", email: "a@b.c", plan: "pro" });
  const e = await sentEvent();
  assert.deepEqual(e.properties, { plan: "pro" });
});

// Audit F29: analytics flush used to omit the configured API key, so keyed
// installs 401'd on the batch endpoint even though error/log sends worked.
test("flush sends the configured API key on the events batch", async () => {
  init({ endpoint: "https://observe.example.com", siteId: "s1", apiKey: "obs_test_key" });
  let sawHeader: string | undefined;
  (globalThis as any).fetch = (url: string, opts: any) => {
    sawHeader = opts.headers["X-API-Key"];
    sent.push({ url, body: JSON.parse(opts.body) });
    return Promise.resolve({ ok: true, json: async () => ({ ok: true }) });
  };
  track("evt");
  await flush();
  assert.equal(sawHeader, "obs_test_key");
  assert.equal(sent.length, 1);
});

// Audit F30: the old stack regex excluded ':' from the filename group, so
// ordinary https:// frames never matched and web errors lost their stacks.
test("stack parser accepts URL, port, and file frames", async () => {
  const stack = [
    "Error: boom",
    "    at checkout (https://shop.example/app.js:42:7)",
    "    at https://shop.example:8443/app.js:43:9",
    "    at checkout (C:\\app\\main.js:44:11)",
    "checkout@https://shop.example/app.js:45:13",
  ].join("\n");
  const fabricated = new Error("boom");
  fabricated.stack = stack;
  const { captureException } = await import("../src/index.js");
  await captureException(fabricated, { release: "v1" });
  // captureException sends immediately; inspect the last sent payload.
  const errPayload = sent[sent.length - 1].body;
  assert.ok(errPayload.stack_trace.length >= 4, `expected >=4 frames, got ${errPayload.stack_trace.length}`);
  const [f1, f2, f3, f4] = errPayload.stack_trace;
  assert.equal(f1.filename, "https://shop.example/app.js");
  assert.equal(f1.lineno, 42);
  assert.equal(f2.filename, "https://shop.example:8443/app.js");
  assert.equal(f3.filename, "C:\\app\\main.js");
  assert.equal(f4.function, "checkout");
  // O01 §5.6: error payloads carry the producer-stable event_id the
  // server dedupes on.
  assert.match(errPayload.event_id, /^[0-9a-f]{32}$/, "error event_id is sent");
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

// AUD-021 (round 2): a failed send must REJECT so flushClient retains the
// batch — the old transport swallowed the rejection and the events were
// deleted as if delivered.
test("failed batch is retained and reported, not deleted", async () => {
  const errors: string[] = [];
  init({ endpoint: "https://observe.example.com", siteId: "s1", retryBackoffMs: 0, onError: (e) => errors.push(e.message) });
  (globalThis as any).fetch = () => Promise.resolve({ ok: false, status: 503 });

  track("kept-event");
  await flush();
  // Re-deliver with a working transport: the retained request must arrive
  // (byte-identical). The retry gate opens after the (1 ms clamped)
  // backoff.
  (globalThis as any).fetch = (url: string, opts: any) => {
    sent.push({ url, body: JSON.parse(opts.body) });
    return Promise.resolve({ ok: true, json: async () => ({ ok: true }) });
  };
  await new Promise((r) => setTimeout(r, 10));
  await flush();
  assert.equal(sent.length, 1);
  assert.equal(sent[0].body.events[0].event_type, "kept-event");
  assert.ok(errors.some((m) => m.includes("503")), "onError must fire for the failed send");
});

// AUD-022 (round 2): overlapping flush calls must not send duplicate
// prefixes or delete events neither call sent.
test("overlapping flushes send each event exactly once", async () => {
  init({ endpoint: "https://observe.example.com", siteId: "s1" });
  const pending: Array<() => void> = [];
  let first = true;
  (globalThis as any).fetch = (url: string, opts: any) => {
    sent.push({ url, body: JSON.parse(opts.body) });
    if (first) {
      first = false;
      return new Promise((resolve) => pending.push(() => resolve({ ok: true, json: async () => ({ ok: true }) })));
    }
    return Promise.resolve({ ok: true, json: async () => ({ ok: true }) });
  };

  track("e1");
  const a = flush();
  const b = flush(); // overlaps while a's request is pending
  track("e2");
  // b must be the same single-flight promise — no second concurrent send.
  assert.strictEqual(a, b);
  await new Promise((r) => setTimeout(r, 0)); // let the flush owner issue its request
  assert.equal(sent.length, 1, "only one request may be in flight");
  pending[0]();
  await a;
  const types = sent.flatMap((s) => s.body.events.map((e: any) => e.event_type));
  assert.deepEqual([...new Set(types)], types, "no event may be sent twice");
  assert.ok(types.includes("e1") && types.includes("e2"), "both events must be delivered");
});

// AUD-023 (round 2): re-init sends the old client's events as flat arrays,
// never a nested events[][] payload.
test("reinit flushes old buffer with flat event objects", async () => {
  init({ endpoint: "https://observe.example.com", siteId: "s1" });
  track("legacy-1");
  track("legacy-2");
  init({ endpoint: "https://observe.example.com", siteId: "s2" });
  await new Promise((r) => setTimeout(r, 20));
  const bodies = sent.map((s) => s.body);
  assert.ok(bodies.length >= 1);
  for (const body of bodies) {
    assert.ok(Array.isArray(body.events));
    for (const ev of body.events) {
      assert.equal(typeof ev, "object", `events entries must be objects, got ${JSON.stringify(ev)}`);
      assert.ok(!Array.isArray(ev), "events entries must not be arrays");
    }
  }
  assert.ok(bodies.some((b) => b.events.some((e: any) => e.event_type === "legacy-1")));
});

// AUD-026 (round 2): chunk packing respects the encoded-byte budget.
test("oversized events are dropped individually, valid neighbors delivered", async () => {
  init({ endpoint: "https://observe.example.com", siteId: "s1" });
  const errors: string[] = [];
  (globalThis as any).fetch = (url: string, opts: any) => {
    sent.push({ url, body: JSON.parse(opts.body) });
    return Promise.resolve({ ok: true });
  };
  // Build a client-level monster via properties bigger than 1 MiB; the
  // old client's onError must report the drop when re-init flushes it.
  const errs: string[] = [];
  init({ endpoint: "https://observe.example.com", siteId: "s1", onError: (e) => errs.push(e.message) });
  const huge = "x".repeat(1100 * 1024);
  track("small-neighbor");
  track("monster", { blob: huge });
  track("after");
  const { init: reinit } = await import("../src/index.js");
  reinit({ endpoint: "https://observe.example.com", siteId: "s1" });
  await new Promise((r) => setTimeout(r, 20));
  const delivered = sent.flatMap((s) => s.body.events.map((e: any) => e.event_type));
  assert.ok(delivered.includes("small-neighbor") && delivered.includes("after"), "valid events must survive");
  assert.ok(!delivered.includes("monster"), "the oversized event must be dropped");
  // TO-033: the monster is now rejected at admission (per-event byte cap).
  assert.ok(errs.some((m) => m.includes("bytes and was dropped")), "the drop must be reported");
});

// F12 (protocol v2): every event carries a producer-assigned stable
// event_id, and the batch envelope identifies the producer and the batch.
test("events carry stable producer ids and a v2 batch envelope", async () => {
  init({ endpoint: "https://observe.example.com", siteId: "s1" });
  track("one");
  track("two");
  await flush();
  assert.equal(sent[0].body.v, 2);
  assert.ok(sent[0].body.producer_id, "producer_id must be set");
  // TO-032: batch identity is allocated per frozen request, not derived
  // from the first event (a repacked request must never travel under a
  // batch id that already acknowledged different content).
  assert.match(sent[0].body.batch_id, /^[0-9a-f]{32}$/, "batch_id is a fresh 32-hex id");
  for (const e of sent[0].body.events) {
    assert.match(e.event_id, /^[0-9a-f]{32}$/, "event_id is a 32-hex producer id");
  }
  assert.notEqual(sent[0].body.events[0].event_id, sent[0].body.events[1].event_id);
});

// F12: a failed-then-retried batch must reuse the SAME batch_id (and the
// same event ids), so the server recognizes the retry instead of
// double-counting it.
test("retried batch reuses its batch id and event ids", async () => {
  init({ endpoint: "https://observe.example.com", siteId: "s1", retryBackoffMs: 0 });
  let failedBody: any = null;
  (globalThis as any).fetch = (_url: string, opts: any) => {
    failedBody = JSON.parse(opts.body);
    return Promise.resolve({ ok: false, status: 503 });
  };
  track("retry-me");
  await flush();
  assert.ok(failedBody, "first attempt must have been issued");
  (globalThis as any).fetch = (url: string, opts: any) => {
    sent.push({ url, body: JSON.parse(opts.body) });
    return Promise.resolve({ ok: true, json: async () => ({ ok: true }) });
  };
  await new Promise((r) => setTimeout(r, 10)); // past the 1 ms clamped gate
  await flush();
  assert.equal(sent.length, 1);
  // The retried batch is bit-identical in identity: same batch_id and the
  // same event ids, so a server dedupe keyed on either recognizes it.
  assert.equal(sent[0].body.batch_id, failedBody.batch_id);
  assert.deepEqual(
    sent[0].body.events.map((e: any) => e.event_id),
    failedBody.events.map((e: any) => e.event_id),
  );
});

// F32 (retry half): a failed batch is retried automatically with a
// doubling backoff — nothing is sent before the delay elapses, and the
// retry that does go out keeps the batch identity intact.
test("failed batch retries after backoff with intact identity", async () => {
  const retries: Array<{ attempt: number; delayMs: number; batchSize: number }> = [];
  init({
    endpoint: "https://observe.example.com",
    siteId: "s1",
    retryBackoffMs: 40,
    onRetry: (info) => retries.push({ attempt: info.attempt, delayMs: info.delayMs, batchSize: info.batchSize }),
  });
  let failedBody: any = null;
  let attempts = 0;
  (globalThis as any).fetch = (_url: string, opts: any) => {
    failedBody = JSON.parse(opts.body);
    attempts++;
    return Promise.resolve({ ok: false, status: 503 });
  };
  track("backoff-event");
  await flush();
  assert.equal(attempts, 1, "the first flush makes exactly one attempt");

  // While the backoff gate is held, flushes are no-ops.
  await flush();
  assert.equal(attempts, 1, "a flush inside the backoff window must not send");

  await new Promise((r) => setTimeout(r, 60));
  (globalThis as any).fetch = (url: string, opts: any) => {
    attempts++;
    sent.push({ url, body: JSON.parse(opts.body) });
    return Promise.resolve({ ok: true, json: async () => ({ ok: true }) });
  };
  await flush();
  assert.equal(sent.length, 1, "the retry after the window delivers the batch");
  assert.equal(sent[0].body.batch_id, failedBody.batch_id, "retry keeps the batch id");
  assert.equal(retries.length, 1, "onRetry fired for the scheduled retry");
  assert.equal(retries[0].attempt, 1);
  assert.equal(retries[0].delayMs, 40);
  assert.equal(retries[0].batchSize, 1);
});

// F32: the backoff doubles per consecutive failure of the same batch.
test("retry backoff doubles across attempts", async () => {
  const delays: number[] = [];
  init({
    endpoint: "https://observe.example.com",
    siteId: "s1",
    retryBackoffMs: 10,
    onRetry: (info) => delays.push(info.delayMs),
  });
  (globalThis as any).fetch = () => Promise.resolve({ ok: false, status: 500 });
  track("doubling");
  await flush(); // attempt 1 -> delay 10
  await new Promise((r) => setTimeout(r, 15));
  await flush(); // attempt 2 -> delay 20
  assert.deepEqual(delays, [10, 20]);
});

// F32: after maxRetryAttempts the batch is dropped with a loud report, the
// buffer moves on, and a later healthy flush delivers NEW events.
test("retry budget exhausts, drops the batch, and recovers", async () => {
  const errors: string[] = [];
  init({
    endpoint: "https://observe.example.com",
    siteId: "s1",
    retryBackoffMs: 5,
    maxRetryAttempts: 2,
    onError: (e) => errors.push(e.message),
  });
  (globalThis as any).fetch = () => Promise.resolve({ ok: false, status: 503 });
  track("doomed");
  await flush(); // attempt 1, delay 5
  await new Promise((r) => setTimeout(r, 10));
  await flush(); // attempt 2, delay 10
  await new Promise((r) => setTimeout(r, 15));
  await flush(); // attempt 3 > budget: give up and drop
  assert.ok(errors.some((m) => m.includes("gave up on a batch after 2 attempts")), `expected the give-up report, got ${JSON.stringify(errors)}`);

  // The dropped batch must not poison the client: a new event goes through.
  (globalThis as any).fetch = (url: string, opts: any) => {
    sent.push({ url, body: JSON.parse(opts.body) });
    return Promise.resolve({ ok: true, json: async () => ({ ok: true }) });
  };
  track("after-recovery");
  await new Promise((r) => setTimeout(r, 10)); // past any residual gate
  await flush();
  const delivered = sent.flatMap((s) => s.body.events.map((e: any) => e.event_type));
  assert.ok(delivered.includes("after-recovery"), `new events must deliver after recovery, got ${JSON.stringify(delivered)}`);
  assert.ok(!delivered.includes("doomed"), "the given-up batch must not resurrect");
});

// TO-034: under sustained failure (or a hanging request) the queue is
// bounded at ADMISSION time — recording past the count budget drops the
// newest event with a report instead of growing memory without limit.
test("sustained failure bounds the queue at admission", async () => {
  const errors: string[] = [];
  init({
    endpoint: "https://observe.example.com",
    siteId: "s1",
    retryBackoffMs: 5,
    maxRetryAttempts: 50,
    onError: (e) => errors.push(e.message),
  });
  (globalThis as any).fetch = () => Promise.resolve({ ok: false, status: 503 });
  // Queue far beyond the 200-event admission budget while nothing drains.
  for (let i = 0; i < 230; i++) track(`flood-${i}`);
  await new Promise((r) => setTimeout(r, 10)); // past the backoff gate
  await flush();
  assert.ok(errors.some((m) => m.includes("queue budget reached")), `expected the queue-budget report, got ${JSON.stringify(errors.slice(0, 3))}`);
});

// F41 URL contract: the raw query string never leaves the browser — only
// the allowlisted campaign params, as explicit fields.
test("pageview strips the query and sends only allowlisted utm fields", async () => {
  (globalThis as any).location = {
    href: "https://example.com/lp?utm_source=news&utm_medium=email&utm_campaign=launch&utm_term=t&utm_content=c&email=me@x.io&token=hunter2#frag",
    origin: "https://example.com",
    pathname: "/lp",
    search: "?utm_source=news&utm_medium=email&utm_campaign=launch&utm_term=t&utm_content=c&email=me@x.io&token=hunter2",
  };
  // Earlier tests leave in-flight sends that land on later microtasks;
  // this test asserts on the request its own flush produces.
  sent = [];
  pageview();
  await flush();
  assert.ok(sent.length >= 1, "a flush was issued");
  const e = sent[sent.length - 1].body.events[sent[sent.length - 1].body.events.length - 1];

  assert.equal(e.url, "https://example.com/lp");
  assert.equal(e.utm_source, "news");
  assert.equal(e.utm_medium, "email");
  assert.equal(e.utm_campaign, "launch");
  assert.equal(e.utm_term, "t");
  assert.equal(e.utm_content, "c");
  const raw = JSON.stringify(e);
  assert.equal(raw.includes("hunter2"), false, "non-allowlisted params never leave the browser");
  assert.equal(raw.includes("me@x.io"), false);
});

test("track callers can pass explicit utm fields as reserved top-level fields", async () => {
  track("purchase", { utm_campaign: "launch", amount: 42 });
  const e = await sentEvent();
  assert.equal(e.utm_campaign, "launch");
  assert.equal(e.properties.amount, 42);
});

// --- Round 3 ---

// TO-032: the response-lost repack. Server accepts [A] but the response is
// lost; event B is recorded; the retry must resend A's EXACT bytes under
// A's original batch id (so the server dedupes it), and B must be delivered
// in its own request — the old repack sent [A,B] under A's batch id and the
// admission cache swallowed B whole.
test("repacked batch after response loss delivers the new tail (TO-032)", async () => {
  init({ endpoint: "https://observe.example.com", siteId: "s1", retryBackoffMs: 1 });
  const errors: string[] = [];
  init({ endpoint: "https://observe.example.com", siteId: "s1", retryBackoffMs: 1, onError: (e) => errors.push(e.message) });
  let attempt = 0;
  let firstBody: any = null;
  (globalThis as any).fetch = (_url: string, opts: any) => {
    attempt++;
    if (attempt === 1) {
      firstBody = JSON.parse(opts.body);
      // Simulate a lost response: the server accepted it, the client saw a
      // network error.
      return Promise.reject(new TypeError("network error"));
    }
    sent.push({ url: _url, body: JSON.parse(opts.body) });
    return Promise.resolve({ ok: true, json: async () => ({ ok: true }) });
  };
  track("A");
  await flush();
  // Event B arrives while A's retry is pending.
  track("B");
  await new Promise((r) => setTimeout(r, 10));
  await flush();
  await new Promise((r) => setTimeout(r, 10));
  await flush();

  const delivered = sent.map((s) => s.body);
  // A's retry is byte-identical to the lost request.
  const retryA = delivered.find((b) => b.events.some((e: any) => e.event_type === "A"));
  assert.ok(retryA, "the lost batch must be retried");
  assert.equal(retryA.batch_id, firstBody.batch_id, "the retry keeps the frozen batch id");
  assert.deepEqual(retryA.events.map((e: any) => e.event_id), firstBody.events.map((e: any) => e.event_id));
  // B is delivered in its own request, never merged into A's.
  const reqB = delivered.find((b) => b.events.some((e: any) => e.event_type === "B"));
  assert.ok(reqB, "the new tail must be delivered");
  assert.equal(reqB.batch_id === firstBody.batch_id, false, "B rides its own request identity");
  assert.equal(reqB.events.length, 1);
});

// TO-033: an unserializable record is isolated at admission — the good
// neighbors still deliver, and identity fields cannot be overridden through
// props.
test("unserializable and identity-overriding records are isolated (TO-033)", async () => {
  const errors: string[] = [];
  init({ endpoint: "https://observe.example.com", siteId: "s1", onError: (e) => errors.push(e.message) });
  const circular: any = { self: null };
  circular.self = circular;
  track("good-before");
  track("poison", { payload: circular });
  track("sneaky", { site_id: "evil-site", event_id: "forged", event_type: "forged-type" });
  track("good-after");
  await flush();
  const delivered = sent.flatMap((s) => s.body.events.map((e: any) => e.event_type));
  assert.ok(delivered.includes("good-before") && delivered.includes("good-after"), "valid neighbors deliver");
  assert.ok(!delivered.includes("poison"), "the unserializable record is dropped, not the flush");
  assert.ok(errors.some((m) => m.includes("not JSON-serializable")), "the poison is reported");
  const sneaky = sent.flatMap((s) => s.body.events).find((e: any) => e.event_type === "sneaky");
  assert.ok(sneaky, "the sneaky record itself delivers");
  assert.equal(sneaky.site_id, "s1", "site_id cannot be overridden");
  assert.notEqual(sneaky.event_id, "forged", "event_id cannot be overridden");
});

// TO-035: a 200 acknowledgment reporting per-event rejections surfaces
// through onError and is NOT retried.
test("partial rejection is reported and not retried (TO-035)", async () => {
  const errors: string[] = [];
  init({ endpoint: "https://observe.example.com", siteId: "s1", retryBackoffMs: 1, onError: (e) => errors.push(e.message) });
  let calls = 0;
  (globalThis as any).fetch = (_url: string, opts: any) => {
    calls++;
    sent.push({ url: _url, body: JSON.parse(opts.body) });
    return Promise.resolve({ ok: true, json: async () => ({ ok: true, accepted: 1, rejected: 1 }) });
  };
  track("ok-one");
  track("rejected-one");
  await flush();
  await new Promise((r) => setTimeout(r, 10));
  await flush();
  assert.equal(calls, 1, "a partially rejected batch must not be resent");
  assert.ok(errors.some((m) => m.includes("rejected 1 of 2")), `expected the partial-rejection report, got ${JSON.stringify(errors)}`);
});

// TO-036: an old client's in-flight drain keeps ITS producer identity and
// endpoint — a re-init must not relabel or redirect it.
test("old client drain keeps its own producer identity (TO-036)", async () => {
  init({ endpoint: "https://observe.example.com", siteId: "s1" });
  track("old-event");
  const seen: any[] = [];
  (globalThis as any).fetch = (_url: string, opts: any) => {
    seen.push({ url: _url, body: JSON.parse(opts.body) });
    return Promise.resolve({ ok: true, json: async () => ({ ok: true }) });
  };
  // Re-init while the old buffer is undelivered.
  init({ endpoint: "https://observe.example.net", siteId: "s2" });
  await new Promise((r) => setTimeout(r, 20));
  const old = seen.find((s) => s.body.events.some((e: any) => e.event_type === "old-event"));
  assert.ok(old, "the old client's event must drain");
  assert.match(old.url, /observe\.example\.com/, "drain goes to the OLD endpoint");
  // The replacement client's own traffic carries a different producer id
  // and a different endpoint — the drain was never relabeled or redirected.
  track("new-event");
  await flush();
  const fresh = seen.find((s) => s.url.includes("example.net") && s.body.events.some((e: any) => e.event_type === "new-event"));
  assert.ok(fresh, "the new client sends to the new endpoint");
  assert.notEqual(fresh.body.producer_id, old.body.producer_id, "the drain keeps the OLD producer identity");
});

// TO-037: fetch is invoked with redirect:"error" — a redirecting ingest
// endpoint fails the request instead of forwarding the API key.
test("transport refuses redirects (TO-037)", async () => {
  init({ endpoint: "https://observe.example.com", siteId: "s1", apiKey: "k" });
  let sawRedirect: unknown;
  (globalThis as any).fetch = (_url: string, opts: any) => {
    sawRedirect = opts.redirect;
    sent.push({ url: _url, body: JSON.parse(opts.body) });
    return Promise.resolve({ ok: true, json: async () => ({ ok: true }) });
  };
  track("redirect-probe");
  await flush();
  assert.equal(sawRedirect, "error");
  sawRedirect = undefined;
  const { captureException } = await import("../src/index.js");
  await captureException(new Error("x"));
  assert.equal(sawRedirect, "error", "error reports refuse redirects too");
});
