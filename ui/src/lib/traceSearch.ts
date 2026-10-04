// Pure query-string builder for trace search with span-attribute filters.
// Mirrors the server contract in internal/tracing/search.go (GET
// /api/v1/traces/search-advanced): repeated attr=key:op[:value], at most five
// filters, keys limited to [A-Za-z0-9_.\-/]{1,128}. The server re-validates
// everything; this only keeps obviously bad chips from being sent.

export const MAX_ATTR_FILTERS = 5;
export const MAX_ATTR_VALUE_LEN = 256;

export type AttrOp = "eq" | "neq" | "exists" | "contains";
export const ATTR_OPS: AttrOp[] = ["eq", "neq", "exists", "contains"];

export interface AttrChip {
  key: string;
  op: AttrOp;
  value: string;
}

export interface TraceSearchParams {
  siteId: string;
  from: string;
  to: string;
  service?: string;
  operation?: string;
  status?: string;
  minDuration?: number;
  maxDuration?: number;
  attrs?: AttrChip[];
  includeOrphans?: boolean;
}

const KEY_RE = /^[A-Za-z0-9_.\-/]{1,128}$/;

/** Returns an error message for an unusable chip, or null when it is valid. */
export function validateChip(chip: AttrChip): string | null {
  if (!KEY_RE.test(chip.key)) return "Key may use letters, digits, _ . - / (max 128)";
  if (!ATTR_OPS.includes(chip.op)) return "Unknown operator";
  if (chip.op === "exists") return null;
  if (chip.value === "") return "A value is required for " + chip.op;
  if (new TextEncoder().encode(chip.value).length > MAX_ATTR_VALUE_LEN) {
    return "Value is too long (max " + MAX_ATTR_VALUE_LEN + " bytes)";
  }
  return null;
}

/** Returns an error when the chip cannot be added to the current list. */
export function canAddChip(existing: AttrChip[], chip: AttrChip): string | null {
  if (existing.length >= MAX_ATTR_FILTERS) return "At most " + MAX_ATTR_FILTERS + " attribute filters";
  return validateChip(chip);
}

/** The wire form of one chip: key:op or key:op:value. */
export function chipToParam(chip: AttrChip): string {
  return chip.op === "exists" ? chip.key + ":exists" : chip.key + ":" + chip.op + ":" + chip.value;
}

/** Short label for a chip, e.g. `http.status_code = 500`. */
export function chipLabel(chip: AttrChip): string {
  switch (chip.op) {
    case "eq": return chip.key + " = " + chip.value;
    case "neq": return chip.key + " != " + chip.value;
    case "contains": return chip.key + " contains " + chip.value;
    default: return chip.key + " exists";
  }
}

/**
 * Builds the query string (no leading "?"). Invalid chips are dropped and the
 * list is capped at MAX_ATTR_FILTERS so the request is never refused for size.
 * Values are percent-encoded, so ':' ',' '&' and '#' survive intact.
 */
export function buildTraceSearchQuery(p: TraceSearchParams): string {
  const q = new URLSearchParams();
  q.set("site_id", p.siteId);
  q.set("from", p.from);
  q.set("to", p.to);
  if (p.service) q.set("service", p.service);
  if (p.operation) q.set("operation", p.operation);
  if (p.status) q.set("status", p.status);
  if (p.minDuration && p.minDuration > 0) q.set("min_duration", String(Math.floor(p.minDuration)));
  if (p.maxDuration && p.maxDuration > 0) q.set("max_duration", String(Math.floor(p.maxDuration)));
  let n = 0;
  for (const chip of p.attrs ?? []) {
    if (n >= MAX_ATTR_FILTERS) break;
    if (validateChip(chip) !== null) continue;
    q.append("attr", chipToParam(chip));
    n++;
  }
  if (p.includeOrphans) q.set("include_orphans", "true");
  return q.toString();
}
