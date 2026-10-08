import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { provenance, costSplit, tokenSplit, requirePartition, partitionFields, partitionColumns } from "./llmProvenance.ts";

test("OBS26-50 exact successful aggregate wire partition, cost/token independence and CSV parity", () => {
  const row = Object.fromEntries(partitionFields.map(field => [field, "0"])) as ReturnType<typeof requirePartition>;
  Object.assign(row, { total_cost_usd: "6.125", reported_cost_usd: "1.125", estimated_cost_usd: "2", legacy_cost_usd: "3", reported_cost_calls: "1", estimated_cost_calls: "2", legacy_cost_calls: "3", cost_unattributed: "3", total_tokens: "99", reported_tokens_total: "7", estimated_tokens_total: "13", legacy_tokens_total: "79", legacy_tokens_in: "1", legacy_tokens_out: "2", reported_token_calls: "3", estimated_token_calls: "1", legacy_token_calls: "2" });
  assert.equal(requirePartition(row), row);
  assert.match(costSplit(row), /\$3 legacy \/ unattributed, 3 calls/);
  assert.match(costSplit(row), /\$1.125 reported, 1 calls/);
  assert.match(tokenSplit(row), /79 legacy \/ unattributed: 1 in, 2 out, 2 calls/);
  assert.match(tokenSplit(row), /7 reported: 0 in, 0 out, 3 calls/);
  assert.deepEqual(partitionColumns.map(c => c.key), [...partitionFields]);
  for (const source of [undefined, "", "unknown", "Reported", "legacy"]) assert.equal(provenance(source), "legacy / unattributed");
  assert.equal(provenance("estimated"), "estimated");
  assert.equal(provenance("reported"), "reported");
});

test("OBS26-50 missing/null/malformed backend fields are unavailable, never fabricated zeros", () => {
  const row = Object.fromEntries(partitionFields.map(field => [field, "0"])) as ReturnType<typeof requirePartition>;
  assert.equal(requirePartition(row), row);
  for (const field of partitionFields) {
    for (const value of [undefined, null, 0, "", "NaN", "Infinity", "-1"]) {
      assert.throws(() => requirePartition({ ...row, [field]: value } as any), /unavailable/);
    }
  }
});


test("Stats/model receivers validate required partitions and both CSV surfaces use full wire columns", () => {
  const source = readFileSync(new URL("../routes/llm.tsx", import.meta.url), "utf8");
  assert.match(source, /interface LLMStats extends ProvenanceTotals/);
  assert.match(source, /interface ModelStats extends ProvenanceTotals/);
  assert.match(source, /\/stats\?[^\n]+\.then\(requirePartition\)/);
  assert.match(source, /return rows.map\(requirePartition\)/);
  for (const surface of ["costs", "models"]) assert.match(source, new RegExp(`filename={\\x60llm-${surface}[^\\n]+\\.\\.\\.partitionColumns`));
  assert.match(source, /key: "cost_source", label: "cost_source"/);
  assert.match(source, /key: "token_source", label: "token_source"/);
  assert.doesNotMatch(source, /!stats \? null/);
});
