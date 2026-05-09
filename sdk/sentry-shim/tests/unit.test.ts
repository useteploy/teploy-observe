/**
 * Unit tests for @observe/sentry-shim.
 *
 * These run with `node --test --import tsx tests/*.test.ts`. Tests use a
 * stub fetch passed via `init({ fetch })` so nothing leaves the process.
 */

import { test, beforeEach } from "node:test";
import assert from "node:assert/strict";

import {
  init,
  captureException,
  captureMessage,
  setUser,
  setTag,
  setTags,
  setContext,
  setExtra,
  setFingerprint,
  addBreadcrumb,
  withScope,
  startSpan,
  startTransaction,
  flush,
  close,
  getClient,
  getCurrentScope,
  configureScope,
} from "../src/index.js";

interface CapturedRequest {
  url: string;
  body: any;
  headers: Record<string, string>;
}

let captured: CapturedRequest[] = [];

function stubFetch(): typeof fetch {
  return (async (input: any, opts: any) => {
    captured.push({
      url: String(input),
      body: opts?.body ? JSON.parse(opts.body as string) : null,
      headers: (opts?.headers ?? {}) as Record<string, string>,
    });
    return new Response(JSON.stringify({ ok: true }), { status: 200 });
  }) as typeof fetch;
}

async function settle(): Promise<void> {
  // Capture* calls fire-and-forget; yield once to let microtasks drain.
  await new Promise((r) => setImmediate(r));
  await new Promise((r) => setImmediate(r));
}

beforeEach(() => {
  captured = [];
});

test("init: requires either dsn or endpoint", () => {
  assert.throws(() => init({} as any), /dsn or endpoint is required/);
});

test("init: accepts a direct endpoint+siteId", () => {
  init({ endpoint: "https://obs.example.com/", siteId: "demo", fetch: stubFetch() });
  const cfg = getClient()!;
  assert.equal(cfg.endpoint, "https://obs.example.com");
  assert.equal(cfg.siteId, "demo");
});

test("init: parses Observe-style DSN", () => {
  init({ dsn: "https://obs.example.com/__observe__/myapp", fetch: stubFetch() });
  const cfg = getClient()!;
  assert.equal(cfg.endpoint, "https://obs.example.com");
  assert.equal(cfg.siteId, "myapp");
});

test("init: parses classic Sentry DSN as a fallback", () => {
  init({ dsn: "https://abc@sentry.example.com/42", fetch: stubFetch() });
  const cfg = getClient()!;
  assert.equal(cfg.endpoint, "https://sentry.example.com");
  assert.equal(cfg.siteId, "42");
});

test("captureException: posts to /api/v1/errors with parsed stack", async () => {
  init({
    endpoint: "https://obs.example.com",
    siteId: "default",
    release: "v9.9.9",
    environment: "test",
    fetch: stubFetch(),
  });
  const err = new Error("kapow");
  err.name = "BoomError";
  captureException(err);
  await settle();
  assert.equal(captured.length, 1);
  assert.equal(captured[0].url, "https://obs.example.com/api/v1/errors");
  const body = captured[0].body;
  assert.equal(body.site_id, "default");
  assert.equal(body.error_type, "BoomError");
  assert.equal(body.error_value, "kapow");
  assert.equal(body.release, "v9.9.9");
  assert.equal(body.environment, "test");
  assert.equal(body.level, "error");
  assert.ok(Array.isArray(body.stack_trace) && body.stack_trace.length > 0);
});

test("captureException: wraps non-Error values", async () => {
  init({ endpoint: "https://obs.example.com", fetch: stubFetch() });
  captureException("plain string failure");
  await settle();
  assert.equal(captured[0].body.error_value, "plain string failure");
});

test("captureMessage: posts to /api/v1/logs with mapped level", async () => {
  init({ endpoint: "https://obs.example.com", siteId: "default", fetch: stubFetch() });
  captureMessage("cache miss", "warning");
  await settle();
  assert.equal(captured.length, 1);
  assert.equal(captured[0].url, "https://obs.example.com/api/v1/logs");
  assert.equal(captured[0].body.level, "warn");
  assert.equal(captured[0].body.message, "cache miss");
});

test("setTag + setContext + setUser flow into the next event", async () => {
  init({ endpoint: "https://obs.example.com", fetch: stubFetch() });
  setUser({ id: "u_1", email: "a@b.c" });
  setTag("region", "us-east-1");
  setTags({ shard: "5", host: "node-1" });
  setContext("runtime", { version: "20.10.0" });
  setExtra("request_id", "req_123");
  captureException(new Error("ctx-test"));
  await settle();
  const body = captured[0].body;
  assert.deepEqual(body.contexts.user, { id: "u_1", email: "a@b.c" });
  assert.equal(body.contexts.tags.region, "us-east-1");
  assert.equal(body.contexts.tags.shard, "5");
  assert.equal(body.contexts.runtime.version, "20.10.0");
  assert.equal(body.extra.request_id, "req_123");
});

test("setFingerprint overrides grouping fingerprint", async () => {
  init({ endpoint: "https://obs.example.com", fetch: stubFetch() });
  setFingerprint(["custom", "group"]);
  captureException(new Error("fp-test"));
  await settle();
  assert.deepEqual(captured[0].body.fingerprint, ["custom", "group"]);
});

test("addBreadcrumb: caps at maxBreadcrumbs and rides the next event", async () => {
  init({ endpoint: "https://obs.example.com", maxBreadcrumbs: 3, fetch: stubFetch() });
  for (let i = 0; i < 10; i++) {
    addBreadcrumb({ category: "test", message: `crumb-${i}` });
  }
  captureException(new Error("bc-test"));
  await settle();
  const crumbs = captured[0].body.breadcrumbs;
  assert.equal(crumbs.length, 3);
  assert.equal(crumbs[0].message, "crumb-7");
  assert.equal(crumbs[2].message, "crumb-9");
});

test("withScope: mutations are isolated to the callback", async () => {
  init({ endpoint: "https://obs.example.com", fetch: stubFetch() });
  setTag("outer", "yes");
  withScope((scope) => {
    setTag("inner", "yes");
    assert.equal(scope.tags.inner, "yes");
    assert.equal(scope.tags.outer, "yes");
    captureException(new Error("inner"));
  });
  captureException(new Error("outer"));
  await settle();
  assert.equal(captured.length, 2);
  // Inner scope keeps both tags; outer scope only kept "outer".
  assert.equal(captured[0].body.contexts.tags.inner, "yes");
  assert.equal(captured[0].body.contexts.tags.outer, "yes");
  assert.equal(captured[1].body.contexts.tags.outer, "yes");
  assert.equal(captured[1].body.contexts.tags.inner, undefined);
});

test("startSpan: emits a log entry with duration on finish", async () => {
  init({ endpoint: "https://obs.example.com", fetch: stubFetch() });
  const result = startSpan({ name: "db.query", op: "db" }, (span) => {
    span.setData("query", "SELECT 1");
    return 42;
  });
  await settle();
  assert.equal(result, 42);
  assert.equal(captured.length, 1);
  assert.equal(captured[0].url, "https://obs.example.com/api/v1/logs");
  assert.equal(captured[0].body.message, "span: db.query");
  assert.equal(captured[0].body.attributes.op, "db");
  assert.equal(captured[0].body.attributes.query, "SELECT 1");
  assert.equal(typeof captured[0].body.attributes.duration_ms, "number");
});

test("startTransaction: returns a usable Span stub", async () => {
  init({ endpoint: "https://obs.example.com", fetch: stubFetch() });
  const tx = startTransaction({ name: "GET /api/v1/things", op: "http.server" });
  tx.setStatus("ok");
  tx.setTag("http.status_code", 200);
  tx.finish();
  // Calling finish twice should be a no-op.
  tx.finish();
  await settle();
  assert.equal(captured.length, 1);
});

test("apiKey is sent as X-API-Key header", async () => {
  init({ endpoint: "https://obs.example.com", apiKey: "secret-key", fetch: stubFetch() });
  captureException(new Error("k"));
  await settle();
  assert.equal(captured[0].headers["X-API-Key"], "secret-key");
});

test("debug: skips network calls", async () => {
  init({ endpoint: "https://obs.example.com", debug: true, fetch: stubFetch() });
  captureException(new Error("nope"));
  captureMessage("nope");
  await settle();
  assert.equal(captured.length, 0);
});

test("getCurrentScope reflects active scope", () => {
  init({ endpoint: "https://obs.example.com", fetch: stubFetch() });
  configureScope((scope) => {
    scope.tags.foo = "bar";
  });
  assert.equal(getCurrentScope().tags.foo, "bar");
});

test("flush + close resolve cleanly", async () => {
  init({ endpoint: "https://obs.example.com", fetch: stubFetch() });
  assert.equal(await flush(), true);
  assert.equal(await close(), true);
  assert.equal(getClient(), null);
});

test("captureException: never throws even when fetch rejects", async () => {
  init({
    endpoint: "https://obs.example.com",
    fetch: (async () => {
      throw new Error("network down");
    }) as any,
  });
  // Must not throw.
  const id = captureException(new Error("swallowed"));
  await settle();
  assert.equal(typeof id, "string");
  assert.equal(id.length, 32);
});

test("beforeSend: can drop or mutate events", async () => {
  init({
    endpoint: "https://obs.example.com",
    fetch: stubFetch(),
    beforeSend: (event) => {
      event.error_value = "[redacted]";
      return event;
    },
  });
  captureException(new Error("contains-secret"));
  await settle();
  assert.equal(captured[0].body.error_value, "[redacted]");

  captured = [];
  init({
    endpoint: "https://obs.example.com",
    fetch: stubFetch(),
    beforeSend: () => null,
  });
  captureException(new Error("dropped"));
  await settle();
  assert.equal(captured.length, 0);
});
