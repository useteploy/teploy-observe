// Uptime, cron, and infrastructure monitoring API.

import { get, post } from "./helpers.js";

const BASE = "/api/v1";

export interface UptimeMonitor {
  monitor_id: string;
  site_id: string;
  name: string;
  url: string;
  method: string;
  expected_status: number;
  interval_secs: number;
  enabled: boolean;
  created_at: string;
}

export interface UptimeResult {
  result_id: string;
  monitor_id: string;
  site_id: string;
  timestamp: number;
  status_code: number;
  response_ms: number;
  is_up: boolean;
  error_message: string;
}

export interface CronMonitor {
  cron_id: string;
  site_id: string;
  slug: string;
  name: string;
  schedule: string;
  grace_period: number;
  enabled: boolean;
  created_at: string;
}

export interface InfraHost {
  hostname: string;
  cpu_percent: number;
  memory_percent: number;
  disk_percent: number;
  load_1m: number;
  last_seen: string;
}

export interface InfraMetric {
  metric_id: string;
  site_id: string;
  hostname: string;
  timestamp: number;
  cpu_percent: number;
  memory_percent: number;
  memory_used_mb: number;
  memory_total_mb: number;
  disk_percent: number;
  disk_used_gb: number;
  disk_total_gb: number;
  net_rx_bytes: number;
  net_tx_bytes: number;
  load_1m: number;
  load_5m: number;
  load_15m: number;
}

export const monitoringApi = {
  uptimeList: (siteId: string) =>
    get<UptimeMonitor[]>(`${BASE}/monitors?site_id=${siteId}`),
  uptimeCreate: (data: { site_id: string; name: string; url: string; method?: string; expected_status?: number; interval_secs?: number }) =>
    post<UptimeMonitor>(`${BASE}/monitors`, data),
  uptimeResults: (monitorId: string, limit?: number) =>
    get<UptimeResult[]>(`${BASE}/monitors/${monitorId}/results${limit ? `?limit=${limit}` : ""}`),
  cronList: (siteId: string) =>
    get<CronMonitor[]>(`${BASE}/crons?site_id=${siteId}`),
  cronCreate: (data: { site_id: string; slug: string; name: string; schedule: string; grace_period?: number }) =>
    post<CronMonitor>(`${BASE}/crons`, data),
  infraHosts: (siteId: string) =>
    get<InfraHost[]>(`${BASE}/infra/hosts?site_id=${siteId}`),
  infraHistory: (hostname: string, from: string, to: string) =>
    get<InfraMetric[]>(`${BASE}/infra/hosts/${encodeURIComponent(hostname)}/history?from=${from}&to=${to}`),
};
