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

export interface MetricSeries {
  labels: Record<string, string>;
  points: MetricPoint[];
}

// Phase-1 reducers + Phase-2 rate / histogram quantiles.
export type Aggregation =
  | "last" | "avg" | "sum" | "min" | "max"
  | "rate" | "p50" | "p95" | "p99";

export type StepDuration = "15s" | "30s" | "60s" | "5m" | "1h" | "1d";

export interface QueryOpts {
  step?: StepDuration;
  groupBy?: string[]; // label keys to fan series out by
  labels?: Record<string, string>;
}

const enc = encodeURIComponent;

function buildQuery(
  siteId: string,
  name: string,
  fromMs: number,
  toMs: number,
  agg: Aggregation,
  opts: QueryOpts = {},
): string {
  let q = `site_id=${enc(siteId)}&name=${enc(name)}&from=${fromMs}&to=${toMs}&agg=${enc(agg)}`;
  if (opts.step) q += `&step=${enc(opts.step)}`;
  if (opts.groupBy && opts.groupBy.length > 0) q += `&group_by=${enc(opts.groupBy.join(","))}`;
  for (const [k, v] of Object.entries(opts.labels ?? {})) {
    if (v) q += `&label.${enc(k)}=${enc(v)}`;
  }
  return q;
}

export const metricsApi = {
  list(siteId: string): Promise<MetricInfo[]> {
    return get<MetricInfo[]>(`${BASE}/list?site_id=${enc(siteId)}`);
  },

  // Single collapsed series — Phase-1 contract preserved.
  query(
    siteId: string,
    name: string,
    fromMs: number,
    toMs: number,
    agg: Aggregation,
    labels: Record<string, string> = {},
    step?: StepDuration,
  ): Promise<MetricPoint[]> {
    return get<MetricPoint[]>(`${BASE}/query?${buildQuery(siteId, name, fromMs, toMs, agg, { labels, step })}`);
  },

  // Phase-2 fan-out: one Series per distinct label combination.
  series(
    siteId: string,
    name: string,
    fromMs: number,
    toMs: number,
    agg: Aggregation,
    opts: QueryOpts = {},
  ): Promise<MetricSeries[]> {
    return get<MetricSeries[]>(`${BASE}/series?${buildQuery(siteId, name, fromMs, toMs, agg, opts)}`);
  },
};
