import assert from "node:assert/strict";
import { test } from "node:test";
import {
  buildSearchQueryString,
  caretLine,
  LOG_QUERY_HELP,
  normalizeSearchResponse,
  parseQuerySyntaxError,
  queryTooLong,
} from "./logQuery.ts";

test("parseQuerySyntaxError reads the server detail", () => {
  assert.deepEqual(parseQuerySyntaxError("query error at position 7: unexpected end of query"), {
    position: 7,
    message: "unexpected end of query",
  });
  assert.equal(parseQuerySyntaxError("site_id required"), null);
  assert.equal(parseQuerySyntaxError("Bad Request: query error at position x: y"), null);
});

test("normalizeSearchResponse accepts a bare array and the query-language object", () => {
  const legacy = normalizeSearchResponse<{ id: number }>([{ id: 1 }]);
  assert.equal(legacy.logs.length, 1);
  assert.equal(legacy.verification, "legacy");
  assert.equal(legacy.nextCursor, "");

  const ql = normalizeSearchResponse<{ id: number }>({
    logs: [{ id: 1 }, { id: 2 }],
    next_cursor: "1700.abc",
    truncated: true,
    verification: "go_window",
  });
  assert.equal(ql.logs.length, 2);
  assert.equal(ql.nextCursor, "1700.abc");
  assert.equal(ql.truncated, true);
  assert.equal(ql.verification, "go_window");
});

test("normalizeSearchResponse tolerates garbage", () => {
  for (const bad of [null, undefined, 5, "x", {}, { logs: "no" }]) {
    const r = normalizeSearchResponse(bad);
    assert.deepEqual(r.logs, []);
    assert.equal(r.truncated, false);
  }
});

test("a query uses lq and cursor, never offset", () => {
  const qs = buildSearchQueryString({
    siteId: "s 1", from: "a", to: "b", query: "  level:error  ", limit: 50, offset: 100, cursor: "1.x",
  });
  assert.ok(qs.includes("lq=level%3Aerror"));
  assert.ok(qs.includes("cursor=1.x"));
  assert.ok(!qs.includes("offset"));
  assert.ok(qs.includes("site_id=s%201"));
});

test("without a query the request is the original offset search", () => {
  const qs = buildSearchQueryString({
    siteId: "s", from: "a", to: "b", query: "   ", level: "ERROR", service: "api", limit: 50, offset: 50, cursor: "1.x",
  });
  assert.ok(!qs.includes("lq="));
  assert.ok(!qs.includes("cursor"));
  assert.ok(qs.includes("offset=50"));
  assert.ok(qs.includes("level=ERROR"));
  assert.ok(qs.includes("service=api"));
});

test("queryTooLong counts bytes", () => {
  assert.equal(queryTooLong("a".repeat(1024)), false);
  assert.equal(queryTooLong("a".repeat(1025)), true);
  assert.equal(queryTooLong("日".repeat(342)), true); // 3 bytes each
});

test("caretLine points at the position and clamps", () => {
  assert.equal(caretLine("foo AND", 7), "       ^");
  assert.equal(caretLine("foo", 99), "   ^");
  assert.equal(caretLine("foo", -4), "^");
});

test("the help table is non-empty and every row is filled in", () => {
  assert.ok(LOG_QUERY_HELP.length >= 8);
  for (const r of LOG_QUERY_HELP) {
    assert.ok(r.syntax && r.meaning);
  }
});
