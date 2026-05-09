// Error tracking API — issues, events, search, releases.

import { get, post } from "./helpers.js";

const BASE = "/api/v1";

export interface Issue {
  issue_id: string;
  site_id: string;
  group_hash: string;
  title: string;
  culprit: string;
  level: string;
  status: string;
  first_seen: string;
  last_seen: string;
  event_count: number;
  user_count: number;
  release_tag: string;
}

export interface ErrorEvent {
  error_id: string;
  site_id: string;
  session_id: string;
  replay_id: string;
  issue_id: string;
  group_hash: string;
  timestamp: string;
  error_type: string;
  error_value: string;
  mechanism: string;
  handled: boolean;
  level: string;
  release_tag: string;
  environment: string;
  url: string;
  browser: string;
  os: string;
  device: string;
  stack_trace: string;
  breadcrumbs: string;
  contexts: string;
  extra: string;
}

export interface ReleaseHealth {
  release_tag: string;
  error_count: number;
  issue_count: number;
  first_seen: string;
  last_seen: string;
}

export interface DailyCount {
  day: string;
  count: number;
}

export const errorsApi = {
  issues: (siteId: string, status?: string, limit?: number, offset?: number) => {
    let q = `site_id=${siteId}`;
    if (status) q += `&status=${status}`;
    if (limit) q += `&limit=${limit}`;
    if (offset) q += `&offset=${offset}`;
    return get<Issue[]>(`${BASE}/issues?${q}`);
  },
  issue: (issueId: string, siteId: string) =>
    get<Issue>(`${BASE}/issues/${issueId}?site_id=${siteId}`),
  issueEvents: (issueId: string, siteId: string) =>
    get<ErrorEvent[]>(`${BASE}/issues/${issueId}/events?site_id=${siteId}`),
  issueSession: (issueId: string, siteId: string) =>
    get<{ session_id: string; events: unknown[] }>(`${BASE}/issues/${issueId}/session?site_id=${siteId}`),
  updateStatus: (issueId: string, siteId: string, status: string) =>
    post<{ ok: boolean }>(`${BASE}/issues/${issueId}/status`, { site_id: siteId, status }),
  search: (siteId: string, query: string) =>
    get<Issue[]>(`${BASE}/issues/search?site_id=${siteId}&q=${encodeURIComponent(query)}`),
  releases: (siteId: string) =>
    get<ReleaseHealth[]>(`${BASE}/releases?site_id=${siteId}`),
  daily: (siteId: string, days = 14) =>
    get<DailyCount[]>(`${BASE}/issues/daily?site_id=${siteId}&days=${days}`),
};
