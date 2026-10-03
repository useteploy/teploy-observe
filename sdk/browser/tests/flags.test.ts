/** Flag client tests against a stubbed fetch (wire contract: POST /api/v1/flags/evaluate). */

import { test, afterEach } from "node:test";
import assert from "node:assert/strict";

import { createFlagClient } from "../src/flags.js";
import { init, evaluateFlag, evaluateFlags, identify, flush } from "../src/index.js";

const g: any = globalThis;
const realFetch = g.fetch;
afterEach(() => { g.fetch = realFetch; });

function ok(body: unknown, status = 200): any {
  return { ok: status >= 200 && status < 300, status, json: async () => body };
}

function mk(handler: (url: string, init: any) => any, extra: Record<string, unknown> = {}) {
  const calls: Array<{ url: string; init: any; body: any }> = [];
  const c = createFlagClient({
    endpoint: "https://observe.test/",
    siteId: "s1",
    fetch: (async (url: string, init: any) => {
      calls.push({ url, init, body: JSON.parse(init.body) });
      return handler(url, init);
    }) as any,
    ...extra,
  });
  return { c, calls };
}

test("sends the documented request and maps the response", async () => {
  const { c, calls } = mk(() => ok({ enabled: true, variant: "b", reason: "evaluated" }));
  const r = await c.evaluateFlag("new-checkout", { userId: "u1", attributes: { plan: "pro", n: 3, b: true, o: {} as any } });
  assert.equal(calls[0].url, "https://observe.test/api/v1/flags/evaluate");
  assert.equal(calls[0].init.method, "POST");
  assert.equal(calls[0].init.credentials, "omit");
  assert.deepEqual(calls[0].body, { site_id: "s1", flag_key: "new-checkout", user_id: "u1", context: { b: "true", n: "3", plan: "pro" } });
  assert.deepEqual(r, { key: "new-checkout", enabled: true, variant: "b", reason: "evaluated", source: "server" });
});

test("api key is sent when configured", async () => {
  const { c, calls } = mk(() => ok({ enabled: false, reason: "evaluated" }), { apiKey: "pk" });
  await c.evaluateFlag("f");
  assert.equal(calls[0].init.headers["X-API-Key"], "pk");
});

test("cache serves repeats within the TTL and expires after it", async () => {
  const { c, calls } = mk(() => ok({ enabled: true, reason: "evaluated" }), { ttlMs: 50 });
  assert.equal((await c.evaluateFlag("f", { userId: "u" })).source, "server");
  assert.equal((await c.evaluateFlag("f", { userId: "u" })).source, "cache");
  assert.equal((await c.evaluateFlag("f", { userId: "other" })).source, "server", "key includes user");
  assert.equal(calls.length, 2);
  await new Promise((r) => setTimeout(r, 70));
  assert.equal((await c.evaluateFlag("f", { userId: "u" })).source, "server");
  assert.equal(calls.length, 3);
});

test("ttlMs 0 per call bypasses the cache", async () => {
  const { c, calls } = mk(() => ok({ enabled: true, reason: "evaluated" }));
  await c.evaluateFlag("f", { userId: "u" });
  await c.evaluateFlag("f", { userId: "u", ttlMs: 0 });
  assert.equal(calls.length, 2);
});

test("concurrent identical calls coalesce into one request", async () => {
  let release!: () => void;
  const gate = new Promise<void>((r) => { release = r; });
  const { c, calls } = mk(async () => { await gate; return ok({ enabled: true, reason: "evaluated" }); });
  const ps = [c.evaluateFlag("f", { userId: "u" }), c.evaluateFlag("f", { userId: "u" }), c.evaluateFlag("f", { userId: "u" })];
  release();
  const rs = await Promise.all(ps);
  assert.equal(calls.length, 1);
  assert.deepEqual(rs.map((r) => r.enabled), [true, true, true]);
});

test("coalesced callers each apply their own default on failure", async () => {
  const { c, calls } = mk(() => ok({}, 500));
  const [a, b] = await Promise.all([
    c.evaluateFlag("f", { userId: "u", default: { enabled: true, variant: "x" } }),
    c.evaluateFlag("f", { userId: "u" }),
  ]);
  assert.equal(calls.length, 1);
  assert.equal(a.enabled, true);
  assert.equal(a.variant, "x");
  assert.equal(b.enabled, false);
  assert.equal(a.source, "default");
});

test("failures resolve to the caller default and are not cached", async () => {
  let n = 0;
  const { c } = mk(() => {
    n++;
    if (n === 1) throw new Error("network down");
    if (n === 2) return ok({}, 503);
    if (n === 3) return ok("garbage");
    return ok({ enabled: true, reason: "evaluated" });
  });
  const d = { enabled: true, variant: "safe" };
  const r1 = await c.evaluateFlag("f", { default: d });
  assert.deepEqual([r1.enabled, r1.variant, r1.source, r1.error], [true, "safe", "default", "network down"]);
  assert.equal((await c.evaluateFlag("f", { default: d })).error, "status 503");
  assert.equal((await c.evaluateFlag("f", { default: d })).error, "malformed response");
  assert.equal((await c.evaluateFlag("f", { default: d })).source, "server", "recovers; failures were not cached");
});

test("server fail-safe reasons (unavailable/invalid) use the default, not the server's answer", async () => {
  const { c } = mk(() => ok({ enabled: false, reason: "unavailable", detail: "flags config read failed" }));
  const r = await c.evaluateFlag("f", { default: { enabled: true } });
  assert.equal(r.enabled, true);
  assert.equal(r.source, "default");
  assert.match(r.error!, /unavailable/);
});

test("timeout aborts and falls back, even if fetch ignores the signal", async () => {
  const { c } = mk(() => new Promise(() => {}), { timeoutMs: 100 });
  const t0 = Date.now();
  const r = await c.evaluateFlag("f", { timeoutMs: 100 });
  assert.equal(r.source, "default");
  assert.match(r.error!, /timeout/);
  assert.ok(Date.now() - t0 < 2000);
});

test("evaluateFlags: batch, duplicate keys collapse, per-key results", async () => {
  const { c, calls } = mk((_u, init) => {
    const k = JSON.parse(init.body).flag_key;
    return k === "bad" ? ok({}, 500) : ok({ enabled: k === "on", reason: "evaluated" });
  });
  const rs = await c.evaluateFlags(["on", "off", "bad", "on"], { userId: "u" });
  assert.deepEqual(Object.keys(rs).sort(), ["bad", "off", "on"]);
  assert.equal(rs.on.enabled, true);
  assert.equal(rs.off.enabled, false);
  assert.equal(rs.bad.source, "default");
  assert.equal(calls.length, 3);
});

test("empty key resolves to default without a request; client never rejects", async () => {
  const { c, calls } = mk(() => ok({}));
  assert.equal((await c.evaluateFlag("")).source, "default");
  assert.equal(calls.length, 0);
});

test("cache is bounded", async () => {
  const { c, calls } = mk(() => ok({ enabled: true, reason: "evaluated" }), { maxEntries: 2 });
  for (const k of ["a", "b", "c"]) await c.evaluateFlag(k);
  await c.evaluateFlag("a");
  assert.equal(calls.length, 4, "oldest entry was evicted");
});

// --- through the SDK singleton ---------------------------------------------

test("SDK evaluateFlag uses identify()d user, never exposes unless asked", async () => {
  const sent: any[] = [];
  g.fetch = async (url: string, o: any) => {
    sent.push({ url, body: JSON.parse(o.body) });
    if (url.endsWith("/flags/evaluate")) return ok({ enabled: true, variant: "b", reason: "evaluated" });
    return ok({ ok: true });
  };
  init({ endpoint: "https://observe.test", siteId: "s9", disableAutoPageview: true });
  identify("user-7");
  await flush();
  sent.length = 0;

  const r = await evaluateFlag("exp", { default: { enabled: false } });
  assert.equal(r.variant, "b");
  assert.equal(sent.length, 1);
  assert.equal(sent[0].body.user_id, "user-7");
  assert.equal(sent[0].body.site_id, "s9");
  await flush();
  assert.equal(sent.filter((s) => s.url.endsWith("/events/batch")).length, 0, "no exposure event by default");

  await evaluateFlag("exp", { exposure: true });
  await evaluateFlag("exp", { exposure: true });
  await flush();
  const batches = sent.filter((s) => s.url.endsWith("/events/batch"));
  assert.equal(batches.length, 1);
  assert.equal(batches[0].body.events.length, 1, "exposure recorded once per flag/user/variant");
  assert.equal(batches[0].body.events[0].event_type, "flag_exposure");

  const many = await evaluateFlags(["exp", "other"], { default: { enabled: false } });
  assert.deepEqual(Object.keys(many).sort(), ["exp", "other"]);
});

test("exposure is not recorded for a defaulted result", async () => {
  const sent: any[] = [];
  g.fetch = async (url: string, o: any) => {
    sent.push({ url });
    if (url.endsWith("/flags/evaluate")) return ok({}, 500);
    return ok({ ok: true });
  };
  init({ endpoint: "https://observe.test", disableAutoPageview: true });
  const r = await evaluateFlag("f", { exposure: true });
  assert.equal(r.source, "default");
  await flush();
  assert.equal(sent.filter((s) => s.url.endsWith("/events/batch")).length, 0);
});
