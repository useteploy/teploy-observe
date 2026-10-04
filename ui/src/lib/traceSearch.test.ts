import { test } from "node:test";
import assert from "node:assert/strict";
import {
  buildTraceSearchQuery,
  validateChip,
  canAddChip,
  chipToParam,
  chipLabel,
  MAX_ATTR_FILTERS,
} from "./traceSearch.ts";
import type { AttrChip } from "./traceSearch.ts";

const base = { siteId: "s1", from: "2026-01-01T00:00:00Z", to: "2026-01-02T00:00:00Z" };

test("minimal query carries site and window only", () => {
  const q = new URLSearchParams(buildTraceSearchQuery(base));
  assert.equal(q.get("site_id"), "s1");
  assert.equal(q.get("from"), base.from);
  assert.equal(q.has("attr"), false);
  assert.equal(q.has("include_orphans"), false);
});

test("attr chips become repeated attr params in order", () => {
  const attrs: AttrChip[] = [
    { key: "http.status_code", op: "eq", value: "500" },
    { key: "db.system", op: "exists", value: "" },
    { key: "http.route", op: "contains", value: "/api/v1" },
    { key: "http.method", op: "neq", value: "GET" },
  ];
  const q = new URLSearchParams(buildTraceSearchQuery({ ...base, attrs }));
  assert.deepEqual(q.getAll("attr"), [
    "http.status_code:eq:500",
    "db.system:exists",
    "http.route:contains:/api/v1",
    "http.method:neq:GET",
  ]);
});

test("values with colons, commas, ampersands and hashes round-trip", () => {
  const value = "a:b,c&d=e#f g";
  const qs = buildTraceSearchQuery({ ...base, attrs: [{ key: "k", op: "eq", value }] });
  assert.equal(new URLSearchParams(qs).get("attr"), "k:eq:" + value);
  assert.equal(qs.includes("&d=e"), false); // the raw ampersand must be encoded
  assert.equal(qs.includes("#"), false);
});

test("invalid chips are dropped and the list is capped", () => {
  const attrs: AttrChip[] = [
    { key: "bad key", op: "eq", value: "x" },
    { key: "k", op: "eq", value: "" },
    ...Array.from({ length: 8 }, (_, i): AttrChip => ({ key: "k" + i, op: "exists", value: "" })),
  ];
  const got = new URLSearchParams(buildTraceSearchQuery({ ...base, attrs })).getAll("attr");
  assert.equal(got.length, MAX_ATTR_FILTERS);
  assert.equal(got[0], "k0:exists");
});

test("validateChip mirrors the server rules", () => {
  assert.equal(validateChip({ key: "a.b-c/d_e", op: "eq", value: "x" }), null);
  assert.equal(validateChip({ key: "k", op: "exists", value: "" }), null);
  assert.notEqual(validateChip({ key: "", op: "eq", value: "x" }), null);
  assert.notEqual(validateChip({ key: "a'b", op: "eq", value: "x" }), null);
  assert.notEqual(validateChip({ key: "a:b", op: "eq", value: "x" }), null);
  assert.notEqual(validateChip({ key: "a".repeat(129), op: "eq", value: "x" }), null);
  assert.notEqual(validateChip({ key: "k", op: "contains", value: "" }), null);
  assert.notEqual(validateChip({ key: "k", op: "eq", value: "v".repeat(257) }), null);
  assert.notEqual(validateChip({ key: "k", op: "regex" as never, value: "x" }), null);
});

test("canAddChip enforces the five-filter bound", () => {
  const full: AttrChip[] = Array.from({ length: MAX_ATTR_FILTERS }, (_, i) => ({ key: "k" + i, op: "exists", value: "" }));
  assert.notEqual(canAddChip(full, { key: "x", op: "exists", value: "" }), null);
  assert.equal(canAddChip(full.slice(1), { key: "x", op: "exists", value: "" }), null);
});

test("orphans flag, durations and labels", () => {
  const q = new URLSearchParams(
    buildTraceSearchQuery({ ...base, includeOrphans: true, minDuration: 10.9, maxDuration: 0, service: "api" }),
  );
  assert.equal(q.get("include_orphans"), "true");
  assert.equal(q.get("min_duration"), "10");
  assert.equal(q.has("max_duration"), false);
  assert.equal(q.get("service"), "api");
  assert.equal(chipToParam({ key: "k", op: "exists", value: "ignored" }), "k:exists");
  assert.equal(chipLabel({ key: "k", op: "neq", value: "v" }), "k != v");
  assert.equal(chipLabel({ key: "k", op: "exists", value: "" }), "k exists");
});
