// Alerts API — rules, history.

import { get, post, del } from "./helpers.js";

const BASE = "/api/v1/platform";

export interface AlertRule {
  rule_id: string;
  site_id: string;
  name: string;
  metric: string;
  operator: string;
  threshold: number;
  window_minutes: number;
  check_interval: number;
  cooldown: number;
  enabled: boolean;
  created_by: string;
  created_at: string;
}

export interface AlertHistoryEntry {
  alert_id: string;
  rule_id: string;
  site_id: string;
  triggered_at: string;
  metric_value: number;
  threshold: number;
  status: string;
}

export const alertsApi = {
  rules: (siteId: string) =>
    get<AlertRule[]>(`${BASE}/alerts/rules?site_id=${siteId}`),
  createRule: (data: {
    site_id: string; name: string; metric: string; operator: string;
    threshold: number; window_minutes?: number; cooldown?: number;
  }) => post<AlertRule>(`${BASE}/alerts/rules`, data),
  deleteRule: (ruleId: string) =>
    del(`${BASE}/alerts/rules/${ruleId}`),
  history: (siteId: string) =>
    get<AlertHistoryEntry[]>(`${BASE}/alerts/history?site_id=${siteId}`),
};
