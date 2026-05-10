// Metrics API — OTLP gauge / sum / histogram series.

import { get } from "./helpers.js";

const BASE = "/api/v1/metrics";

export interface MetricInfo {
  name: string;
  kind: string; // "gauge" | "sum" | "histogram"
}

export interface MetricPoint {
  ts_ms: number;
  value: number;
  labels?: Record<string, string>;
}

export type Aggregation = "last" | "avg" | "sum" | "min" | "max";

const enc = encodeURIComponent;

export const metricsApi = {
  list(siteId: string): Promise<MetricInfo[]> {
    return get<MetricInfo[]>(`${BASE}/list?site_id=${enc(siteId)}`);
  },

  query(
    siteId: string,
    name: string,
    fromMs: number,
    toMs: number,
    agg: Aggregation,
    labels: Record<string, string> = {},
  ): Promise<MetricPoint[]> {
    let q = `site_id=${enc(siteId)}&name=${enc(name)}&from=${fromMs}&to=${toMs}&agg=${enc(agg)}`;
    for (const [k, v] of Object.entries(labels)) {
      if (v) q += `&label.${enc(k)}=${enc(v)}`;
    }
    return get<MetricPoint[]>(`${BASE}/query?${q}`);
  },
};
