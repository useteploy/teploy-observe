import { test } from "node:test";
import assert from "node:assert/strict";
import { normalizeAssignee, canMerge, MAX_ASSIGNEE_LEN } from "./issueAdmin.ts";

test("normalizeAssignee trims and allows empty", () => {
  assert.equal(normalizeAssignee("  alice@example.com "), "alice@example.com");
  assert.equal(normalizeAssignee("   "), "");
});

test("normalizeAssignee rejects control chars and oversize", () => {
  assert.equal(normalizeAssignee("a\u0000b"), null);
  assert.equal(normalizeAssignee("a\nb"), null);
  assert.equal(normalizeAssignee("x".repeat(MAX_ASSIGNEE_LEN + 1)), null);
  assert.equal(normalizeAssignee("x".repeat(MAX_ASSIGNEE_LEN)), "x".repeat(MAX_ASSIGNEE_LEN));
});

test("canMerge refuses empty and self", () => {
  assert.equal(canMerge("a", ""), false);
  assert.equal(canMerge("a", " a "), false);
  assert.equal(canMerge("a", "b"), true);
});
