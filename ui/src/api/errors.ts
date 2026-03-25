// Error tracking API — issues, events, search, releases.

import { get, post } from "./helpers.js";

const BASE = "/api/v1";

export interface Issue {
  issue_id: string; site_id: string; group_hash: string;
  title: string; culprit: string; level: string; status: string;
  first_seen: string; last_seen: string; event_count: string;
  user_count: string; release_tag: string;
}

export interface ErrorEvent {
  error_id: string; site_id: string; issue_id: string;
  timestamp: string; error_type: string; error_value: string;
  mechanism: string; handled: string; level: string;
  release_tag: string; environment: string; url: string;
  browser: string; os: string; device: string;
  stack_trace: string; breadcrumbs: string;
  contexts: string; extra: string;
}

export interface ReleaseHealth {
  release_tag: string; error_count: number; first_seen: string; last_seen: string;
}

export const errorsApi = {
  issues: (siteId: string, status?: string) =>
    get<Issue[]>(`${BASE}/issues?site_id=${siteId}${status ? `&status=${status}` : ""}`),
  issue: (issueId: string) =>
    get<Issue>(`${BASE}/issues/${issueId}`),
  issueEvents: (issueId: string) =>
    get<ErrorEvent[]>(`${BASE}/issues/${issueId}/events`),
  issueSession: (issueId: string) =>
    get<any>(`${BASE}/issues/${issueId}/session`),
  updateStatus: (issueId: string, status: string) =>
    post<{ok: boolean}>(`${BASE}/issues/${issueId}/status`, { status }),
  search: (siteId: string, query: string) =>
    get<Issue[]>(`${BASE}/issues/search?site_id=${siteId}&q=${encodeURIComponent(query)}`),
  releases: (siteId: string) =>
    get<ReleaseHealth[]>(`${BASE}/releases?site_id=${siteId}`),
};
