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

// B2 phase 1: per-release crash-free %, adoption %, error rate. Powers
// the stat-card grid + 14-day sparkline at the top of /releases.
export interface ReleaseStat {
  release_tag: string;
  sessions: number;
  crashed_sessions: number;
  crash_free_session_pct: number;
  adoption_pct: number;
  errors: number;
  error_rate: number;
  first_seen_ms: number;
  last_seen_ms: number;
}

export interface ReleaseSparklinePoint {
  day_ms: number;
  sessions: number;
  crashed_sessions: number;
  crash_free_session_pct: number;
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
  releaseHealth: (siteId: string, fromIso: string, toIso: string) =>
    get<ReleaseStat[]>(`${BASE}/releases/health?site_id=${siteId}&from=${fromIso}&to=${toIso}`),
  releaseSparkline: (siteId: string, release: string, days = 14) =>
    get<ReleaseSparklinePoint[]>(`${BASE}/releases/sparkline?site_id=${siteId}&release=${encodeURIComponent(release)}&days=${days}`),
  daily: (siteId: string, days = 14) =>
    get<DailyCount[]>(`${BASE}/issues/daily?site_id=${siteId}&days=${days}`),
};
