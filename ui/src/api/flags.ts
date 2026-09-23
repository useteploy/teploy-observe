// Feature flags + experiments API.

import { get, post } from "./helpers.js";

const BASE = "/api/v1";

export interface FeatureFlag {
  flag_id: string;
  site_id: string;
  flag_key: string;
  name: string;
  description: string;
  flag_type: string;
  enabled: boolean;
  rollout_pct: number;
  variants: string;
  targeting: string;
  created_at: string;
}

export interface Experiment {
  experiment_id: string;
  site_id: string;
  name: string;
  flag_key: string;
  status: string;
  variants: string;
  goal_metric: string;
  goal_value: string;
  min_sample: number;
  created_at: string;
  started_at: string;
  ended_at: string;
}

export interface VariantResult {
  variant: string;
  exposures: number;
  conversions: number;
  conversion_rate: number;
  prob_beat_control: number;
  wilson_low: number;
  wilson_high: number;
}

export interface SRMDiagnostic {
  detected: boolean;
  chi_square: number;
  p_value: number;
  note?: string;
}

export interface PairwiseResult {
  variant: string;
  lift_absolute: number;
  lift_relative: number;
  ci_low: number;
  ci_high: number;
  p_value: number;
  holm_adjusted_p: number;
  significant: boolean;
  used_fisher: boolean;
}

export interface AnalysisResult {
  horizon_met: boolean;
  min_arm_exposures: number;
  min_sample_per_arm: number;
  test: string;
  test_note?: string;
  chi_square: number;
  df: number;
  p_value: number;
  srm: SRMDiagnostic;
  pairwise_vs_control: PairwiseResult[];
  winner_rule: string;
}

export interface ExperimentResults {
  experiment: Experiment;
  variants: VariantResult[];
  significant: boolean;
  winner: string;
  analysis: AnalysisResult;
}

export interface FlagHistoryEntry {
  timestamp: number;
  action: "created" | "toggle" | "update";
  enabled: boolean;
  rollout_pct?: number;
  variants?: string;
  targeting?: string;
  changed_by?: string;
}

export const flagsApi = {
  list: (siteId: string) =>
    get<FeatureFlag[]>(`${BASE}/flags?site_id=${siteId}`),
  create: (data: { site_id: string; flag_key: string; name: string; description?: string; flag_type?: string; rollout_pct?: number; variants?: string; targeting?: string }) =>
    post<FeatureFlag>(`${BASE}/flags`, data),
  toggle: (flagId: string, enabled: boolean) =>
    post<{ ok: boolean }>(`${BASE}/flags/${flagId}/toggle`, { enabled }),
  history: (flagId: string) =>
    get<FlagHistoryEntry[]>(`${BASE}/flags/${flagId}/history`),
  evaluate: (siteId: string, flagKey: string, userId: string, context?: Record<string, string>) =>
    post<{ enabled: boolean; variant?: string }>(`${BASE}/flags/evaluate`, { site_id: siteId, flag_key: flagKey, user_id: userId, context }),
};

export const experimentsApi = {
  list: (siteId: string) =>
    get<Experiment[]>(`${BASE}/experiments?site_id=${siteId}`),
  create: (data: { site_id: string; name: string; flag_key: string; variants: string; goal_metric: string; goal_value?: string; min_sample?: number }) =>
    post<Experiment>(`${BASE}/experiments`, data),
  start: (experimentId: string) =>
    post<{ ok: boolean }>(`${BASE}/experiments/${experimentId}/start`, {}),
  stop: (experimentId: string) =>
    post<{ ok: boolean }>(`${BASE}/experiments/${experimentId}/stop`, {}),
  results: (experimentId: string) =>
    get<ExperimentResults>(`${BASE}/experiments/${experimentId}/results`),
};
