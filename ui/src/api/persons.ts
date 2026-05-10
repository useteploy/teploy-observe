// Persons + cohorts API client.
//
// C2 (Wave 4): persons aggregate over events.distinct_id; cohorts are
// behavioural rule definitions evaluated at query time. Backend lives
// in cmd/observe/persons_handlers.go and cohorts_handlers.go.

import { get, post, del } from "./helpers.js";

const BASE = "/api/v1";

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export interface Person {
  distinct_id: string;
  first_seen_ms: number;
  last_seen_ms: number;
  event_count: number;
  session_count: number;
  top_country: string;
  top_browser: string;
}

export interface PersonEvent {
  event_id: string;
  event_type: string;
  url: string;
  pathname: string;
  timestamp: number;
}

export interface PersonDetail {
  person: Person;
  timeline: PersonEvent[];
}

export interface PersonsListResult {
  persons: Person[];
  total: number;
  limit: number;
  offset: number;
}

export interface CohortRule {
  type: "event" | "property";
  name?: string;
  window?: string;
  min_count?: number;
  key?: string;
  operator?: "=" | "!=";
  value?: string;
}

export interface CohortDefinition {
  op: "and";
  rules: CohortRule[];
}

export interface Cohort {
  cohort_id: string;
  site_id: string;
  name: string;
  description: string;
  rule: string; // JSON-stringified CohortDefinition
  member_count: number;
  created_at: number;
  updated_at: number;
}

export interface CohortMembersResult {
  members: string[];
  limit: number;
  offset: number;
}

export interface CohortPreviewResult {
  count: number;
  sample: string[];
}

// parseRule unmarshals the stored rule JSON. The server returns it as
// a string column for simplicity (JSON in TEXT) — UI parses on read.
export function parseRule(raw: string): CohortDefinition {
  if (!raw) return { op: "and", rules: [] };
  try {
    const p = JSON.parse(raw);
    if (p && p.op === "and" && Array.isArray(p.rules)) return p;
  } catch { /* fall through */ }
  return { op: "and", rules: [] };
}

// ---------------------------------------------------------------------------
// Persons API
// ---------------------------------------------------------------------------

export const personsApi = {
  list: (siteId: string, opts?: {
    from?: string;
    to?: string;
    limit?: number;
    offset?: number;
    includeAnonymous?: boolean;
  }) => {
    const enc = encodeURIComponent;
    let q = `site_id=${enc(siteId)}`;
    if (opts?.from) q += `&from=${enc(opts.from)}`;
    if (opts?.to) q += `&to=${enc(opts.to)}`;
    if (opts?.limit) q += `&limit=${opts.limit}`;
    if (opts?.offset) q += `&offset=${opts.offset}`;
    if (opts?.includeAnonymous) q += `&include_anonymous=true`;
    return get<PersonsListResult>(`${BASE}/persons?${q}`);
  },
  detail: (distinctId: string, siteId: string) =>
    get<PersonDetail>(`${BASE}/persons/${encodeURIComponent(distinctId)}?site_id=${encodeURIComponent(siteId)}`),
};

// ---------------------------------------------------------------------------
// Cohorts API
// ---------------------------------------------------------------------------

export const cohortsApi = {
  list: (siteId: string) =>
    get<Cohort[]>(`${BASE}/cohorts?site_id=${encodeURIComponent(siteId)}`),
  get: (cohortId: string, siteId: string) =>
    get<Cohort>(`${BASE}/cohorts/${cohortId}?site_id=${encodeURIComponent(siteId)}`),
  create: (data: { site_id: string; name: string; description?: string; rule: CohortDefinition }) =>
    post<Cohort>(`${BASE}/cohorts`, data),
  update: (cohortId: string, data: { site_id: string; name: string; description?: string; rule: CohortDefinition }) =>
    fetch(`${BASE}/cohorts/${cohortId}`, {
      method: "PUT",
      headers: putHeaders(),
      body: JSON.stringify(data),
    }).then(handlePut<Cohort>),
  delete: (cohortId: string, siteId: string) =>
    del(`${BASE}/cohorts/${cohortId}?site_id=${encodeURIComponent(siteId)}`),
  refresh: (cohortId: string, siteId: string) =>
    post<Cohort>(`${BASE}/cohorts/${cohortId}/refresh?site_id=${encodeURIComponent(siteId)}`, {}),
  preview: (siteId: string, rule: CohortDefinition) =>
    post<CohortPreviewResult>(`${BASE}/cohorts/preview`, { site_id: siteId, rule }),
  members: (cohortId: string, siteId: string, opts?: { limit?: number; offset?: number }) => {
    let q = `site_id=${encodeURIComponent(siteId)}`;
    if (opts?.limit) q += `&limit=${opts.limit}`;
    if (opts?.offset) q += `&offset=${opts.offset}`;
    return get<CohortMembersResult>(`${BASE}/cohorts/${cohortId}/members?${q}`);
  },
};

// PUT helpers — helpers.ts only exports get/post/del. Keep this local
// rather than expanding the shared helper surface for one method.
function putHeaders(): Record<string, string> {
  const token = localStorage.getItem("obs_token");
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (token) headers["Authorization"] = `Bearer ${token}`;
  return headers;
}

async function handlePut<T>(res: Response): Promise<T> {
  if (res.status === 401) {
    localStorage.removeItem("obs_token");
    window.location.href = "/login";
    throw new Error("Unauthorized");
  }
  if (!res.ok) throw new Error(`API ${res.status}: ${res.statusText}`);
  return res.json();
}
