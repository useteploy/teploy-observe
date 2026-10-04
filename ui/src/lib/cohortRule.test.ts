import { test } from "node:test";
import assert from "node:assert/strict";
import {
  validateRuleJson, describeRule, parseIdList,
  MAX_TREE_LEAVES, MAX_STATIC_IDS,
} from "./cohortRule.ts";

const ev = (name: string) => ({ leaf: { type: "event", name } });

test("legacy flat and rule is valid", () => {
  const r = validateRuleJson(JSON.stringify({
    op: "and",
    rules: [{ type: "event", name: "a" }, { type: "property", key: "country", value: "US" }],
  }));
  assert.equal(r.ok, true);
});

test("or / not trees are valid", () => {
  const r = validateRuleJson(JSON.stringify({
    op: "and",
    children: [ev("a"), { op: "not", children: [{ op: "or", children: [ev("b"), ev("c")] }] }],
  }));
  assert.equal(r.ok, true);
});

test("rejects bad shapes", () => {
  const bad: Array<[string, unknown]> = [
    ["not json", "{"],
    ["empty group", { op: "or", children: [] }],
    ["not with two", { op: "not", children: [ev("a"), ev("b")] }],
    ["unknown op", { op: "xor", children: [ev("a")] }],
    ["mixed", { op: "and", rules: [{ type: "event", name: "a" }], children: [ev("b")] }],
    ["bad key", { leaf: { type: "property", key: "password", value: "x" } }],
    ["bad window", { leaf: { type: "event", name: "a", window: "soon" } }],
    ["static", { op: "static" }],
  ];
  for (const [name, v] of bad) {
    const r = validateRuleJson(typeof v === "string" ? v : JSON.stringify(v));
    assert.equal(r.ok, false, name);
  }
});

test("depth limit is 4 levels", () => {
  const nest = (n: number): unknown => {
    let d: unknown = ev("x");
    for (let i = 0; i < n; i++) d = { op: "and", children: [d] };
    return d;
  };
  assert.equal(validateRuleJson(JSON.stringify(nest(3))).ok, true);
  assert.equal(validateRuleJson(JSON.stringify(nest(4))).ok, false);
});

test("leaf limit is 30", () => {
  const kids = (n: number) => ({ op: "or", children: Array.from({ length: n }, (_, i) => ev("e" + i)) });
  assert.equal(validateRuleJson(JSON.stringify(kids(MAX_TREE_LEAVES))).ok, true);
  assert.equal(validateRuleJson(JSON.stringify(kids(MAX_TREE_LEAVES + 1))).ok, false);
});

test("describeRule renders trees", () => {
  const lines = describeRule({
    op: "and",
    children: [{ leaf: { type: "event", name: "a" } }, { op: "not", children: [{ leaf: { type: "property", key: "country", value: "US" } }] }],
  });
  assert.deepEqual(lines, [
    "AND",
    '  did "a" at least 1 time(s) in 30d',
    "  NOT",
    "    country = US",
  ]);
  assert.deepEqual(describeRule({ op: "static" }), ["static member list"]);
});

test("parseIdList handles csv, header, blanks, dups", () => {
  const r = parseIdList("﻿distinct_id,name\r\nu1,A\r\n\r\n\"u2\",B\nu1\n u3 \n");
  assert.deepEqual(r.ids, ["u1", "u2", "u3"]);
  assert.equal(r.error, undefined);
});

test("parseIdList enforces caps", () => {
  assert.ok(parseIdList("x".repeat(257)).error);
  const big = Array.from({ length: MAX_STATIC_IDS + 1 }, (_, i) => "u" + i).join("\n");
  assert.ok(parseIdList(big).error);
});
