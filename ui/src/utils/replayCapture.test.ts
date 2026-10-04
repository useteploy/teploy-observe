import assert from "node:assert/strict";
import { test } from "node:test";
import { extractCaptured, upTo, formatOffset } from "./replayCapture.ts";

const ev = (t: number, type: string, data: unknown) => ({
  timestamp: new Date(t).toISOString(),
  event_type: type,
  data: typeof data === "string" ? data : JSON.stringify(data),
});

test("offsets use the earliest event as the player origin", () => {
  const out = extractCaptured([
    ev(10_000, "click", {}),
    ev(12_500, "console", { level: "warn", message: "hi" }),
    ev(11_000, "network", { method: "GET", url: "https://a.test/x", status: 200, duration_ms: 5, size: 9, kind: "fetch" }),
  ]);
  assert.equal(out.console[0].offsetMs, 2500);
  assert.equal(out.network[0].offsetMs, 1000);
  assert.equal(out.network[0].size, 9);
});

test("malformed, unknown-level and non-object payloads are dropped", () => {
  const out = extractCaptured([
    ev(1000, "console", "not json"),
    ev(1000, "console", { level: "debug", message: "x" }),
    ev(1000, "console", { level: "log", message: 5 }),
    ev(1000, "console", "[1,2]"),
    ev(1000, "network", { method: "GET" }),
    { timestamp: "garbage", event_type: "console", data: "{}" },
  ]);
  assert.equal(out.console.length, 0);
  assert.equal(out.network.length, 0);
});

test("render caps bound hostile stored data", () => {
  const many = Array.from({ length: 900 }, (_, i) => ev(1000 + i, "console", { level: "log", message: "m".repeat(5000) }));
  const out = extractCaptured(many);
  assert.equal(out.console.length, 200);
  assert.equal(out.console[0].message.length, 1024);
});

test("upTo returns the prefix at or before the clock", () => {
  const items = [0, 100, 100, 500].map((offsetMs) => ({ offsetMs }));
  assert.equal(upTo(items, -1).length, 0);
  assert.equal(upTo(items, 100).length, 3);
  assert.equal(upTo(items, 10_000).length, 4);
});

test("formatOffset", () => {
  assert.equal(formatOffset(65_400), "1:05");
  assert.equal(formatOffset(0), "0:00");
});
