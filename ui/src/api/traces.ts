// Distributed tracing API — services, operations, waterfall, dependencies.

import { get } from "./helpers.js";

const BASE = "/api/v1/traces";

export interface Service {
  service_name: string;
  request_count: number;
  error_count: number;
  avg_duration_ms: number;
  p50_ms: number;
  p95_ms: number;
  p99_ms: number;
  apdex_score: number;
}

export interface Operation {
  operation_name: string;
  request_count: number;
  error_count: number;
  avg_duration_ms: number;
  p50_ms: number;
  p95_ms: number;
  p99_ms: number;
}

export interface TraceSummary {
  trace_id: string;
  root_service: string;
  root_operation: string;
  start_time: string;
  duration_ms: number;
  span_count: number;
  status_code: string;
}

export interface Span {
  trace_id: string;
  span_id: string;
  parent_span_id: string;
  service_name: string;
  operation_name: string;
  span_kind: string;
  start_time: string;
  end_time: string;
  duration_ms: number;
  status_code: string;
  status_message: string;
  attributes: string;
  resource: string;
  events: string;
}

export interface ServiceDependency {
  src_service: string;
  dst_service: string;
  call_count: number;
  error_count: number;
  avg_duration_ms: number;
}

export interface TraceError {
  error_id: string;
  error_type: string;
  error_value: string;
  timestamp: string;
  issue_id: string;
}

export const tracesApi = {
  services: (siteId: string, from: string, to: string) =>
    get<Service[]>(`${BASE}/services?site_id=${siteId}&from=${from}&to=${to}`),
  operations: (siteId: string, serviceName: string, from: string, to: string) =>
    get<Operation[]>(`${BASE}/services/${encodeURIComponent(serviceName)}/operations?site_id=${siteId}&from=${from}&to=${to}`),
  search: (siteId: string, from: string, to: string, opts?: { service?: string; operation?: string; status?: string; min_duration?: number; max_duration?: number }) => {
    let q = `site_id=${siteId}&from=${from}&to=${to}`;
    if (opts?.service) q += `&service=${encodeURIComponent(opts.service)}`;
    if (opts?.operation) q += `&operation=${encodeURIComponent(opts.operation)}`;
    if (opts?.status) q += `&status=${opts.status}`;
    if (opts?.min_duration) q += `&min_duration=${opts.min_duration}`;
    if (opts?.max_duration) q += `&max_duration=${opts.max_duration}`;
    return get<TraceSummary[]>(`${BASE}/search?${q}`);
  },
  trace: (traceId: string, siteId: string) =>
    get<Span[]>(`${BASE}/${traceId}?site_id=${siteId}`),
  traceErrors: (traceId: string, siteId: string) =>
    get<TraceError[]>(`${BASE}/${traceId}/errors?site_id=${siteId}`),
  dependencies: (siteId: string, from: string, to: string) =>
    get<ServiceDependency[]>(`${BASE}/dependencies?site_id=${siteId}&from=${from}&to=${to}`),
};
