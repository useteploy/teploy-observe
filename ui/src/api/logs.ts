// Log search API.

import { get } from "./helpers.js";
import { buildSearchQueryString, normalizeSearchResponse } from "../lib/logQuery.js";
import type { LogSearchPage } from "../lib/logQuery.js";

const BASE = "/api/v1";

export interface LogEntry {
  log_id: string; site_id: string; timestamp: string;
  level: string; message: string; service_name: string;
  trace_id: string; span_id: string; attributes: string;
}

export interface LogStats {
  level: string; count: number;
}

export interface LogHistogramBucket {
  bucket: number; level: string; count: number;
}

export const logsApi = {
  // A non-blank opts.query is sent as `lq` (the log query language) and pages
  // by opts.cursor; without one this is the original offset search.
  search: async (siteId: string, from: string, to: string, opts?: { query?: string; level?: string; service?: string; limit?: number; offset?: number; cursor?: string }): Promise<LogSearchPage<LogEntry>> => {
    const q = buildSearchQueryString({ siteId, from, to, ...opts });
    return normalizeSearchResponse<LogEntry>(await get<unknown>(`${BASE}/logs/search?${q}`));
  },
  stats: (siteId: string, from: string, to: string) =>
    get<LogStats[]>(`${BASE}/logs/stats?site_id=${siteId}&from=${from}&to=${to}`),
  histogram: (siteId: string, from: string, to: string, bucketMs = 5 * 60 * 1000) =>
    get<LogHistogramBucket[]>(`${BASE}/logs/histogram?site_id=${siteId}&from=${from}&to=${to}&bucket_ms=${bucketMs}`),
};
