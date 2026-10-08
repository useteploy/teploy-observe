export function provenance(source?: string): string {
  return source === "estimated" ? "estimated" : source === "reported" ? "reported" : "legacy / unattributed";
}

export const partitionFields = [
  "total_cost_usd", "estimated_cost_usd", "reported_cost_usd", "legacy_cost_usd",
  "total_tokens", "cost_unattributed",
  "reported_tokens_in", "reported_tokens_out", "estimated_tokens_in", "estimated_tokens_out", "legacy_tokens_in", "legacy_tokens_out",
  "reported_tokens_total", "estimated_tokens_total", "legacy_tokens_total",
  "reported_cost_calls", "estimated_cost_calls", "legacy_cost_calls",
  "reported_token_calls", "estimated_token_calls", "legacy_token_calls",
] as const;
export type ProvenanceTotals = Record<typeof partitionFields[number], string>;
export const partitionColumns = partitionFields.map(key => ({ key, label: key }));

export function requirePartition<T extends ProvenanceTotals>(row: T): T {
  if (!row || typeof row !== "object" || partitionFields.some(field =>
    typeof row[field] !== "string" || !(field.endsWith("_usd") ? /^\d+(?:\.\d+)?$/ : /^\d+$/).test(row[field]))) {
    throw new Error("LLM provenance breakdown is unavailable. Update the server and retry.");
  }
  return row;
}

export function costSplit(row: ProvenanceTotals): string {
  // Preserve decimal-string wire values; no subtraction guesses legacy spend.
  return `$${row.total_cost_usd} USD ($${row.reported_cost_usd} reported, ${row.reported_cost_calls} calls; $${row.estimated_cost_usd} estimated, ${row.estimated_cost_calls} calls; $${row.legacy_cost_usd} legacy / unattributed, ${row.legacy_cost_calls} calls)`;
}
export function tokenSplit(row: ProvenanceTotals): string {
  return `${row.total_tokens} total (${["reported", "estimated", "legacy"].map(source => {
    const key = source as "reported" | "estimated" | "legacy";
    return `${row[`${key}_tokens_total`]} ${source === "legacy" ? "legacy / unattributed" : source}: ${row[`${key}_tokens_in`]} in, ${row[`${key}_tokens_out`]} out, ${row[`${key}_token_calls`]} calls`;
  }).join("; ")})`;
}
