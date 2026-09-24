/**
 * O11 tests for @teploy/observe-sentry-shim: request-local scope
 * (AsyncLocalStorage), honest no-op diagnostics, visible loss counters,
 * and bounded flush.
 */

import { test, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";

import {
  init,
  captureException,
  captureMessage,
  setUser,
  startSession,
  withRequestScope,
  flush,
  getStats,
} from "../src/index.js";
import { FaultServer } from "../../testing/faultserver.mjs";

let server: FaultServer;
let errors: Error[];
const realWarn = console.warn;
let warnings: string[];

beforeEach(async () => {
  errors = [];
  warnings = [];
  server = new FaultServer();
  await server.start();
  console.warn = (...args: unknown[]) => {
    warnings.push(args.map(String).join(" "));
  };
});

afterEach(async () => {
  await server.close();
  console.warn = realWarn;
});

function initShim(overrides: Record<string, unknown> = {}): void {
  init({
    endpoint: server.url(),
    siteId: "s1",
    apiKey: "obs_test",
    onError: (e) => errors.push(e),
    ...overrides,
  });
}

async function settle(): Promise<void> {
  for (let i = 0; i < 4; i++) await new Promise((r) => setImmediate(r));
}

test("request-local scope: concurrent requests cannot leak user context", async () => {
  initShim();
  // Two interleaved async "requests". Without request-local scope, the
  // second setUser overwrites the first and BOTH captures carry user-2.
  const req1 = withRequestScope(async () => {
    setUser({ id: "user-1" });
    await new Promise((r) => setTimeout(r, 20));
    captureException(new Error("from request 1")); // still user-1 here
  });
  const req2 = withRequestScope(async () => {
    await new Promise((r) => setTimeout(r, 5));
    setUser({ id: "user-2" });
    await new Promise((r) => setTimeout(r, 20));
    captureException(new Error("from request 2"));
  });
  await Promise.all([req1, req2]);
  await settle();

  const bodies = server.requests.map((r) => JSON.parse(r.bodyText));
  const e1 = bodies.find((b) => b.error_value === "from request 1");
  const e2 = bodies.find((b) => b.error_value === "from request 2");
  assert.ok(e1 && e2, `both captures sent: ${JSON.stringify(bodies.map((b) => b.error_value))}`);
  assert.equal(e1.distinct_id, "user-1", "request 1 must not see request 2's user");
  assert.equal(e2.distinct_id, "user-2", "request 2 must not see request 1's user");
  assert.equal(getStats().delivered, 2);
});

test("request-local scope: request state does not leak into the global scope", async () => {
  initShim();
  await withRequestScope(async () => {
    setUser({ id: "scoped" });
    await new Promise((r) => setImmediate(r));
  });
  captureException(new Error("outside any request"));
  await settle();

  const bodies = server.requests.map((r) => JSON.parse(r.bodyText));
  const outside = bodies.find((b) => b.error_value === "outside any request");
  assert.ok(outside, "capture sent");
  assert.equal(outside.distinct_id, undefined, "request-scoped user must not leak globally");
});

test("without withRequestScope the shared global scope is used (documented)", async () => {
  initShim();
  setUser({ id: "global-user" });
  captureMessage("via global scope");
  await settle();
  const bodies = server.requests.map((r) => JSON.parse(r.bodyText));
  assert.equal(bodies[0].distinct_id, "global-user");
});

test("intentional no-ops warn once per class with a pointer to the table", async () => {
  initShim({
    tracesSampleRate: 0.5,
    profilesSampleRate: 0.1,
    integrations: [],
    autoSessionTracking: true,
  });
  startSession();
  startSession(); // second call must NOT warn again
  startSession();
  await settle();

  const noOpWarnings = warnings.filter((w) => w.includes("observe-sentry-shim"));
  assert.ok(noOpWarnings.length >= 5, `expected option + session warnings, got ${JSON.stringify(warnings)}`);
  assert.ok(noOpWarnings.every((w) => w.includes("COMPATIBILITY.md")), "warnings point at the compat table");
  const sessionWarnings = noOpWarnings.filter((w) => w.includes("startSession()"));
  assert.equal(sessionWarnings.length, 1, `startSession warns exactly once, got ${sessionWarnings.length}`);
});

test("debug dry-run stays silent", async () => {
  initShim({ debug: true, tracesSampleRate: 0.5 });
  startSession();
  await settle();
  assert.equal(warnings.length, 0);
});

test("server 500 counts a visible loss; recovery delivers", async () => {
  server.mode = { kind: "status", code: 500 };
  initShim();
  captureMessage("lost to a 500");
  await flush(500);

  let stats = getStats();
  assert.equal(stats.lost.http_500, 1, `lost: ${JSON.stringify(stats.lost)}`);
  assert.ok(errors.some((e) => e.message.includes("http_500")), "loss reported through onError");

  server.mode = { kind: "ok" };
  captureMessage("delivered");
  await flush(500);
  stats = getStats();
  assert.equal(stats.delivered, 1);
  assert.equal(stats.lost.http_500, 1);
});

test("flush(timeout) counts unconfirmed sends against a slow endpoint", async () => {
  server.mode = { kind: "slow", delayMs: 500 };
  initShim();
  captureMessage("slow one");
  const ok = await flush(50);
  assert.equal(ok, false, "flush must not claim success with unconfirmed sends");
  const stats = getStats();
  assert.equal(stats.unconfirmedAtFlush, 1, `stats: ${JSON.stringify(stats)}`);
  assert.ok(errors.some((e) => e.message.includes("unconfirmed")));
  // Wait out the server so afterEach teardown is clean.
  await new Promise((r) => setTimeout(r, 600));
});
