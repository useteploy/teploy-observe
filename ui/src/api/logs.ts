// Log search API.

import { get } from "./helpers.js";

const BASE = "/api/v1";

export interface LogEntry {
  log_id: string; site_id: string; timestamp: string;
  level: string; message: string; service_name: string;
  trace_id: string; span_id: string; attributes: string;
}

export interface LogStats {
  level: string; count: number;
}

export const logsApi = {
  search: (siteId: string, from: string, to: string, opts?: { query?: string; level?: string; service?: string; limit?: number; offset?: number }) => {
    let q = `site_id=${siteId}&from=${from}&to=${to}`;
    if (opts?.query) q += `&q=${encodeURIComponent(opts.query)}`;
    if (opts?.level) q += `&level=${opts.level}`;
    if (opts?.service) q += `&service=${encodeURIComponent(opts.service)}`;
    if (opts?.limit) q += `&limit=${opts.limit}`;
    if (opts?.offset) q += `&offset=${opts.offset}`;
    return get<LogEntry[]>(`${BASE}/logs/search?${q}`);
  },
  stats: (siteId: string, from: string, to: string) =>
    get<LogStats[]>(`${BASE}/logs/stats?site_id=${siteId}&from=${from}&to=${to}`),
};
