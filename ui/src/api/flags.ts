// Feature flags + experiments API.

import { get, post } from "./helpers.js";

const BASE = "/api/v1";

export interface FeatureFlag {
  flag_id: string; site_id: string; flag_key: string;
  name: string; description: string; flag_type: string;
  enabled: string; rollout_pct: string;
  variants: string; targeting: string; created_at: string;
}

export interface Experiment {
  experiment_id: string; site_id: string; name: string;
  flag_key: string; status: string; variants: string;
  goal_metric: string; created_at: string; started_at: string; stopped_at: string;
}

export interface ExperimentResults {
  variants: Array<{
    key: string; exposures: number; conversions: number;
    conversion_rate: number; significant: boolean;
  }>;
  winner: string;
}

export const flagsApi = {
  list: (siteId: string) =>
    get<FeatureFlag[]>(`${BASE}/flags?site_id=${siteId}`),
  create: (data: { site_id: string; flag_key: string; name: string; description?: string; flag_type?: string; rollout_pct?: number; variants?: string; targeting?: string }) =>
    post<FeatureFlag>(`${BASE}/flags`, data),
  toggle: (flagId: string, enabled: boolean) =>
    post<{ok: boolean}>(`${BASE}/flags/${flagId}/toggle`, { enabled }),
  evaluate: (siteId: string, flagKey: string, userId: string, context?: Record<string, string>) =>
    post<{ key: string; enabled: boolean; variant?: string }>(`${BASE}/flags/evaluate`, { site_id: siteId, flag_key: flagKey, user_id: userId, context }),
};

export const experimentsApi = {
  list: (siteId: string) =>
    get<Experiment[]>(`${BASE}/experiments?site_id=${siteId}`),
  create: (data: { site_id: string; name: string; flag_key: string; variants: string; goal_metric: string }) =>
    post<Experiment>(`${BASE}/experiments`, data),
  start: (experimentId: string) =>
    post<{ok: boolean}>(`${BASE}/experiments/${experimentId}/start`, {}),
  stop: (experimentId: string) =>
    post<{ok: boolean}>(`${BASE}/experiments/${experimentId}/stop`, {}),
  results: (experimentId: string) =>
    get<ExperimentResults>(`${BASE}/experiments/${experimentId}/results`),
};
