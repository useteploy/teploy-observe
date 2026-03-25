// Uptime, cron, and infrastructure monitoring API.

import { get, post } from "./helpers.js";

const BASE = "/api/v1";

export interface UptimeMonitor {
  monitor_id: string; site_id: string; name: string;
  url: string; method: string; expected_status: string;
  interval_seconds: string; enabled: string; created_at: string;
}

export interface UptimeResult {
  result_id: string; monitor_id: string; timestamp: string;
  status_code: string; response_ms: string; is_up: string; error_message: string;
}

export interface CronMonitor {
  monitor_id: string; site_id: string; slug: string;
  name: string; schedule: string; grace_seconds: string;
  enabled: string; created_at: string;
}

export interface InfraHost {
  host_id: string; hostname: string; last_report: string;
  cpu_pct: number; memory_pct: number; disk_pct: number;
}

export const monitoringApi = {
  uptimeList: (siteId: string) =>
    get<UptimeMonitor[]>(`${BASE}/monitoring/uptime?site_id=${siteId}`),
  uptimeCreate: (data: { site_id: string; name: string; url: string; method?: string; expected_status?: number; interval_seconds?: number }) =>
    post<UptimeMonitor>(`${BASE}/monitoring/uptime`, data),
  uptimeResults: (monitorId: string) =>
    get<UptimeResult[]>(`${BASE}/monitoring/uptime/${monitorId}/results`),
  cronList: (siteId: string) =>
    get<CronMonitor[]>(`${BASE}/monitoring/cron?site_id=${siteId}`),
  cronCreate: (data: { site_id: string; slug: string; name: string; schedule: string; grace_seconds?: number }) =>
    post<CronMonitor>(`${BASE}/monitoring/cron`, data),
  infraHosts: (siteId: string) =>
    get<InfraHost[]>(`${BASE}/infra/hosts?site_id=${siteId}`),
  infraHistory: (hostId: string, from: string, to: string) =>
    get<any[]>(`${BASE}/infra/hosts/${hostId}/history?from=${from}&to=${to}`),
};
