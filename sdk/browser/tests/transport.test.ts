/**
 * Adverse-transport tests for @teploy/observe-browser (O11).
 *
 * Runs REAL requests against a loopback fault-injection server
 * (../../testing/faultserver.mjs — see sdk/testing/README.md for the
 * matrix): server 500s, response loss mid-batch, slow responses under a
 * request deadline, redirect refusal, partial per-record rejections,
 * bounded retry, loss counters, and the pagehide flush/loss snapshot.
 */

import { test, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";

import { init, track, flush, getStats } from "../src/index.js";
import { FaultServer } from "../../testing/faultserver.mjs";

const realFetch = globalThis.fetch;
let server: FaultServer;
let errors: Error[];

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

/** Drain the queue by flushing until the stats say it is empty or a loss
 * ended it. Respects the retry-backoff gate with small sleeps. */
async function drain(maxRounds = 200): Promise<void> {
  for (let i = 0; i < maxRounds; i++) {
    const stats = getStats();
    if (!stats || stats.queued === 0) return;
    await flush();
    await sleep(5);
  }
}

beforeEach(async () => {
  errors = [];
  server = new FaultServer();
  await server.start();
});

afterEach(async () => {
  // Drain or dispose so no dangling promise rejects into another test.
  const stats = getStats();
  if (stats && stats.queued > 0) {
    server.mode = { kind: "ok" };
    await drain(20);
  }
  await server.close();
  (globalThis as any).fetch = realFetch;
});

function initClient(overrides: Record<string, unknown> = {}): void {
  init({
    endpoint: server.url(),
    siteId: "s1",
    apiKey: "obs_test",
    disableAutoPageview: true,
    onError: (e) => errors.push(e),
    ...overrides,
  });
}

test("transient 500s: bounded retry with backoff recovers with zero loss", async () => {
  server.mode = { kind: "failN", n: 2 };
  initClient({ maxRetryAttempts: 5, retryBackoffMs: 1 });
  track("e1", {});
  track("e2", {});
  await drain();

  const stats = getStats()!;
  assert.equal(stats.deliveredEvents, 2, `delivered: ${JSON.stringify(stats)}`);
  assert.equal(stats.deliveredBatches, 1);
  assert.ok(stats.retries >= 2, "two failed attempts were retried");
  assert.deepEqual(stats.dropped, {});
  assert.equal(stats.queued, 0);
});

test("permanent 500s: retry budget exhausts into a visible loss counter", async () => {
  server.mode = { kind: "status", code: 500 };
  initClient({ maxRetryAttempts: 2, retryBackoffMs: 1 });
  track("e1", {});
  track("e2", {});
  await drain();

  const stats = getStats()!;
  assert.equal(stats.dropped.retry_exhausted, 2, `dropped: ${JSON.stringify(stats.dropped)}`);
  assert.equal(stats.queued, 0);
  assert.ok(errors.some((e) => e.message.includes("retry_exhausted")), "loss reported through onError");
});

test("partial rejection: accepted neighbors delivered, rejected counted, batch never resent", async () => {
  server.mode = { kind: "partial", ack: { ok: true, accepted: 1, rejected: 1 } };
  initClient();
  track("good", {});
  track("bad", {});
  await drain();
  await sleep(20); // a resent batch would appear as a second request

  const stats = getStats()!;
  assert.equal(server.requests.length, 1, "accepted batch must not be resent");
  assert.equal(stats.deliveredEvents, 1);
  assert.equal(stats.dropped.server_rejected, 1);
  assert.ok(errors.some((e) => e.message.includes("server_rejected")));
});

test("slow endpoint: request deadline aborts the hang, retry delivers", async () => {
  server.mode = { kind: "slow", delayMs: 400 };
  initClient({ requestTimeoutMs: 100, maxRetryAttempts: 5, retryBackoffMs: 1 });
  track("slow", {});
  await flush(); // aborts at the deadline
  const afterFirst = getStats()!;
  assert.ok(afterFirst.retries >= 1, "deadline abort was treated as a failure");

  server.mode = { kind: "ok" };
  await drain();
  const stats = getStats()!;
  assert.equal(stats.deliveredEvents, 1);
  assert.deepEqual(stats.dropped, {});
});

test("slow endpoint under a pinned flush owner: queue cap drops newest, visibly", async () => {
  server.mode = { kind: "slow", delayMs: 5000 };
  initClient({ requestTimeoutMs: 150, maxRetryAttempts: 50, retryBackoffMs: 1, batchSize: 100 });
  // The first 100 events pin the single flush owner on a hanging request;
  // everything past the 200-event queue cap is drop-newest overflow.
  for (let i = 0; i < 230; i++) track("flood", { i });
  await sleep(400); // let the hanging request abort once and the buffer fill

  const stats = getStats()!;
  assert.ok(
    (stats.dropped.queue_full ?? 0) >= 1,
    `queue_full expected: ${JSON.stringify(stats)}`,
  );
  assert.ok(errors.some((e) => e.message.includes("queue_full")));

  server.mode = { kind: "ok" };
  await drain(400);
  const done = getStats()!;
  assert.equal(done.queued, 0);
  const accounted = done.deliveredEvents + Object.values(done.dropped).reduce((a, b) => a + b, 0);
  assert.equal(accounted, 230, "delivery + losses account for every admitted event");
});

test("connection reset mid-exchange follows the retry path", async () => {
  server.mode = { kind: "reset" };
  initClient({ maxRetryAttempts: 5, retryBackoffMs: 1 });
  track("reset", {});
  await flush().catch(() => {}); // ECONNRESET surfaces as a rejection
  assert.ok(server.requests.length >= 1);

  server.mode = { kind: "ok" };
  await drain();
  const stats = getStats()!;
  assert.equal(stats.deliveredEvents, 1);
  assert.ok(stats.retries >= 1);
  assert.deepEqual(stats.dropped, {});
});

test("redirect refusal: credential never forwarded off-origin", async () => {
  server.mode = { kind: "redirect", location: server.url("/credential-leak") };
  initClient({ maxRetryAttempts: 1, retryBackoffMs: 1 });
  track("redirect", {});
  await drain();
  await sleep(20);

  assert.ok(
    server.requests.every((r) => !r.path.includes("credential-leak")),
    "the redirect target must never be reached",
  );
  const stats = getStats()!;
  assert.ok((stats.dropped.retry_exhausted ?? 0) >= 1, "redirect refusal ends in a counted loss");
});

test("oversize event is rejected at admission with a visible counter", async () => {
  initClient();
  track("huge", { blob: "x".repeat(61 * 1024) });
  const stats = getStats()!;
  assert.equal(stats.dropped.admission_oversize, 1);
  assert.equal(stats.queued, 0);
  assert.equal(server.requests.length, 0, "never sent");
});

test("pagehide: keepalive flush + honest undelivered snapshot", async () => {
  const listeners = new Map<string, () => void>();
  const windowStub = {
    setInterval: () => 1,
    clearInterval: () => {},
    addEventListener: (name: string, fn: () => void) => listeners.set(name, fn),
    removeEventListener: () => {},
  };
  const docStub = {
    referrer: "",
    title: "",
    addEventListener: () => {},
    visibilityState: "visible",
  };
  (globalThis as any).window = windowStub;
  (globalThis as any).document = docStub;
  (globalThis as any).location = { origin: "https://example.com", pathname: "/", search: "" };
  try {
    server.mode = { kind: "ok" };
    initClient();
    track("p1", {});
    track("p2", {});
    track("p3", {});

    const pagehide = listeners.get("pagehide");
    assert.ok(pagehide, "pagehide listener registered");
    pagehide!();
    await sleep(50); // let the keepalive flush settle

    const stats = getStats()!;
    assert.equal(stats.undeliveredAtUnload, 3, "snapshot taken before the flush attempt");
    assert.equal(stats.deliveredEvents, 3, "keepalive flush delivered the queue");
    assert.ok(errors.some((e) => e.message.includes("undelivered")), "snapshot reported via onError");
  } finally {
    delete (globalThis as any).window;
    delete (globalThis as any).location;
  }
});

test("getStats() accounts for every admitted event end to end", async () => {
  server.mode = { kind: "ok" };
  initClient();
  for (let i = 0; i < 10; i++) track("accounted", { i });
  await drain();
  const stats = getStats()!;
  const accounted = stats.deliveredEvents + Object.values(stats.dropped).reduce((a, b) => a + b, 0) + stats.queued;
  assert.equal(accounted, 10);
  assert.ok(stats.producerId.length === 32, "producer id exposed for server-side correlation");
});
