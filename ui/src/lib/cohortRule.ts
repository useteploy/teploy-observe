// Cohort rule helpers (pure logic, no DOM): client-side mirror of the server's
// rule-tree validation (internal/cohorts/tree.go) for the JSON editor, plus
// list parsing for static-cohort import. The server stays the authority; this
// only gives fast feedback before a round trip.

export const MAX_TREE_DEPTH = 4;
export const MAX_TREE_LEAVES = 30;
export const MAX_STATIC_IDS = 100000;
export const MAX_ENTITY_ID_LEN = 256;

const PROPERTY_KEYS = [
  "country", "region", "city",
  "browser", "os", "device", "language",
  "utm_source", "utm_medium", "utm_campaign",
  "pathname", "referrer",
];

export interface RuleLeaf {
  type: string;
  name?: string;
  window?: string;
  min_count?: number;
  key?: string;
  operator?: string;
  value?: string;
}

export interface RuleNode {
  op?: string;
  rules?: RuleLeaf[];
  children?: RuleNode[];
  leaf?: RuleLeaf;
}

export type RuleCheck = { ok: true; def: RuleNode } | { ok: false; error: string };

function checkLeaf(r: RuleLeaf): string | null {
  if (!r || typeof r !== "object") return "a condition must be an object";
  if (r.type === "event") {
    if (!r.name || !r.name.trim()) return "event condition requires name";
    if (r.window && !/^[0-9]{1,4}[dhm]$/i.test(r.window.trim())) {
      return `window "${r.window}" must look like 24h, 7d or 90m`;
    }
    return null;
  }
  if (r.type === "property") {
    if (!r.key) return "property condition requires key";
    if (!PROPERTY_KEYS.includes(r.key)) return `property key "${r.key}" is not allowed`;
    if (r.operator && r.operator !== "=" && r.operator !== "!=") {
      return `operator "${r.operator}" is not supported (use = or !=)`;
    }
    return null;
  }
  return `unsupported condition type "${r.type}" (event or property)`;
}

// Returns an error string or null. `depth` counts nodes from the root.
function checkNode(n: RuleNode, depth: number, count: { leaves: number }): string | null {
  if (!n || typeof n !== "object") return "a node must be an object";
  if (depth > MAX_TREE_DEPTH) return `nesting deeper than ${MAX_TREE_DEPTH} levels`;
  if (n.leaf) {
    if (n.op || n.rules?.length || n.children?.length) return "a leaf node carries no op, rules or children";
    count.leaves++;
    return checkLeaf(n.leaf);
  }
  if (n.rules?.length && n.children?.length) return "use either rules or children, not both";
  const op = n.op || "and";
  const kids: Array<RuleNode | RuleLeaf> = [];
  for (const r of n.rules || []) kids.push({ leaf: r });
  for (const c of n.children || []) kids.push(c);
  if (op !== "and" && op !== "or" && op !== "not") return `unknown op "${op}" (and, or, not)`;
  if (op === "not" && kids.length !== 1) return `"not" takes exactly one child, got ${kids.length}`;
  if (kids.length === 0) return `"${op}" group needs at least one child`;
  for (const k of kids) {
    const err = checkNode(k as RuleNode, depth + 1, count);
    if (err) return err;
  }
  return null;
}

// validateRuleJson parses and checks a rule written as JSON text.
export function validateRuleJson(text: string): RuleCheck {
  let def: RuleNode;
  try {
    def = JSON.parse(text);
  } catch (e) {
    return { ok: false, error: `Invalid JSON: ${(e as Error).message}` };
  }
  if (def && (def as RuleNode).op === "static") {
    return { ok: false, error: "static cohorts are created from a member list, not a rule" };
  }
  const count = { leaves: 0 };
  const err = checkNode(def, 1, count);
  if (err) return { ok: false, error: err };
  if (count.leaves > MAX_TREE_LEAVES) {
    return { ok: false, error: `${count.leaves} conditions, limit ${MAX_TREE_LEAVES}` };
  }
  return { ok: true, def };
}

// describeRule renders a rule tree as indented human-readable lines.
export function describeRule(def: RuleNode, indent = 0): string[] {
  const pad = "  ".repeat(indent);
  const leafText = (r: RuleLeaf) =>
    r.type === "event"
      ? `did "${r.name}" at least ${r.min_count ?? 1} time(s) in ${r.window || "30d"}`
      : `${r.key} ${r.operator || "="} ${r.value || ""}`;
  if (def.op === "static") return [`${pad}static member list`];
  if (def.leaf) return [pad + leafText(def.leaf)];
  const op = (def.op || "and").toUpperCase();
  const lines = [`${pad}${op}`];
  for (const r of def.rules || []) lines.push(`${pad}  ${leafText(r)}`);
  for (const c of def.children || []) lines.push(...describeRule(c, indent + 1));
  return lines;
}

// parseIdList splits pasted text or CSV (first column) into distinct trimmed
// ids, skipping a header row and blanks. Mirrors cohorts.ParseCSVIDs.
export function parseIdList(text: string): { ids: string[]; error?: string } {
  const seen = new Set<string>();
  const ids: string[] = [];
  const lines = text.replace(/^﻿/, "").split(/\r?\n/);
  let first = true;
  for (const line of lines) {
    let cell = line.split(",")[0].trim();
    if (cell.startsWith('"') && cell.endsWith('"') && cell.length >= 2) cell = cell.slice(1, -1);
    if (first) {
      first = false;
      if (["id", "ids", "entity_id", "distinct_id", "user_id", "person"].includes(cell.toLowerCase())) continue;
    }
    if (!cell || seen.has(cell)) continue;
    if (cell.length > MAX_ENTITY_ID_LEN) return { ids: [], error: `an id is longer than ${MAX_ENTITY_ID_LEN} characters` };
    if (seen.size >= MAX_STATIC_IDS) return { ids: [], error: `more than ${MAX_STATIC_IDS.toLocaleString()} distinct ids` };
    seen.add(cell);
    ids.push(cell);
  }
  return { ids };
}
