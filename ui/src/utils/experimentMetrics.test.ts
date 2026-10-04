import { test } from "node:test";
import assert from "node:assert/strict";
import { formatArmValue, metricKindLabel, secondaryLabel } from "./experimentMetrics.ts";

test("binary shows a percentage", () => {
  assert.equal(formatArmValue("binary", { conversion_rate: 0.1234 }), "12.34%");
  assert.equal(formatArmValue(undefined, { conversion_rate: 0.5 }), "50.00%");
});

test("continuous shows the mean with magnitude-aware precision", () => {
  assert.equal(formatArmValue("mean", { conversion_rate: 0.6, mean: 12.3456 }), "12.35");
  assert.equal(formatArmValue("mean", { conversion_rate: 0.6, mean: 1234.5 }), "1235");
  assert.equal(formatArmValue("count", { conversion_rate: 0.6, mean: 0.0456 }), "0.046");
  assert.equal(formatArmValue("mean", { conversion_rate: 0.6 }), "--");
});

test("labels", () => {
  assert.equal(metricKindLabel("mean"), "mean per user");
  assert.equal(metricKindLabel(undefined), "conversion rate");
  assert.equal(secondaryLabel({ key: "rev", name: "Revenue" }), "Revenue (secondary, exploratory)");
  assert.equal(secondaryLabel({ key: "rev" }), "rev (secondary, exploratory)");
});
