// Analytics stats API — pageviews, visitors, breakdowns, etc.

import { get, post, qs } from "./helpers.js";

const BASE = "/api/v1/stats";

export interface OverviewStats {
  pageviews: number; visitors: number; sessions: number;
  bounce_rate: number; avg_duration: number;
}
export interface OverviewResponse { current: OverviewStats; previous?: OverviewStats; }
export interface TimeSeriesPoint { bucket: number; pageviews: number; visitors: number; }
export interface TopPage { pathname: string; pageviews: number; visitors: number; }
export interface TopReferrer { referrer: string; visitors: number; }
export interface BrowserStat { browser: string; visitors: number; }
export interface CountryStat { country: string; visitors: number; }
export interface OSStat { os: string; visitors: number; }
export interface DeviceStat { device: string; visitors: number; }
export interface ChannelStat { channel: string; visitors: number; }
export interface LanguageStat { language: string; visitors: number; }
export interface ScreenStat { screen: string; visitors: number; }
export interface UTMStat { value: string; visitors: number; }
export interface EntryPageStat { pathname: string; visitors: number; }
export interface ExitPageStat { pathname: string; visitors: number; }
export interface CustomEventStat { event_type: string; count: number; visitors: number; }
export interface RealtimeResult { active_visitors: number; }

// Advanced analytics types
export interface FunnelStep { type: string; value: string; }
export interface FunnelResult {
  step: FunnelStep; visitors: number; conversion: number; drop_off: number;
}
export interface RetentionCohort {
  cohort_date: string; cohort_size: number; periods: number[];
}
export interface JourneyStep { from: string; to: string; count: number; }
export interface JourneyPath { path: string[]; count: number; }
export interface JourneyResult {
  transitions: JourneyStep[]; top_paths: JourneyPath[]; total_paths: number;
}
export interface Goal {
  goal_id: string; site_id: string; name: string; goal_type: string; goal_value: string;
}
export interface GoalConversion {
  goal: Goal; conversions: number; visitors: number; rate: number;
}
export interface Correlation {
  property: string; value: string; uplift: number;
  occurrences: number; conversions: number; rate: number;
  baseline_rate: number; significant: boolean;
}

export const analyticsApi = {
  realtime: (siteId: string, minutes = 5) =>
    get<RealtimeResult>(`${BASE}/realtime?site_id=${siteId}&minutes=${minutes}`),
  overview: (siteId: string, from: string, to: string, compare?: string | null, filters?: Record<string, string>) =>
    get<OverviewResponse>(`${BASE}/overview?${qs(siteId, from, to, { compare: compare || undefined, filters })}`),
  timeseries: (siteId: string, from: string, to: string, interval?: string, filters?: Record<string, string>) =>
    get<TimeSeriesPoint[]>(`${BASE}/timeseries?${qs(siteId, from, to, { interval, filters })}`),
  pages: (siteId: string, from: string, to: string, limit = 10, filters?: Record<string, string>) =>
    get<TopPage[]>(`${BASE}/pages?${qs(siteId, from, to, { limit, filters })}`),
  referrers: (siteId: string, from: string, to: string, limit = 10, filters?: Record<string, string>) =>
    get<TopReferrer[]>(`${BASE}/referrers?${qs(siteId, from, to, { limit, filters })}`),
  browsers: (siteId: string, from: string, to: string, limit = 10, filters?: Record<string, string>) =>
    get<BrowserStat[]>(`${BASE}/browsers?${qs(siteId, from, to, { limit, filters })}`),
  countries: (siteId: string, from: string, to: string, limit = 10, filters?: Record<string, string>) =>
    get<CountryStat[]>(`${BASE}/countries?${qs(siteId, from, to, { limit, filters })}`),
  os: (siteId: string, from: string, to: string, limit = 10, filters?: Record<string, string>) =>
    get<OSStat[]>(`${BASE}/os?${qs(siteId, from, to, { limit, filters })}`),
  devices: (siteId: string, from: string, to: string, limit = 10, filters?: Record<string, string>) =>
    get<DeviceStat[]>(`${BASE}/devices?${qs(siteId, from, to, { limit, filters })}`),
  channels: (siteId: string, from: string, to: string, limit?: number, filters?: Record<string, string>) =>
    get<ChannelStat[]>(`${BASE}/channels?${qs(siteId, from, to, { limit, filters })}`),
  languages: (siteId: string, from: string, to: string, limit = 10, filters?: Record<string, string>) =>
    get<LanguageStat[]>(`${BASE}/languages?${qs(siteId, from, to, { limit, filters })}`),
  screens: (siteId: string, from: string, to: string, limit = 10, filters?: Record<string, string>) =>
    get<ScreenStat[]>(`${BASE}/screens?${qs(siteId, from, to, { limit, filters })}`),
  utm: (siteId: string, from: string, to: string, type: string, limit = 10, filters?: Record<string, string>) =>
    get<UTMStat[]>(`${BASE}/utm?${qs(siteId, from, to, { limit, type, filters })}`),
  entryPages: (siteId: string, from: string, to: string, limit = 10, filters?: Record<string, string>) =>
    get<EntryPageStat[]>(`${BASE}/entry-pages?${qs(siteId, from, to, { limit, filters })}`),
  exitPages: (siteId: string, from: string, to: string, limit = 10, filters?: Record<string, string>) =>
    get<ExitPageStat[]>(`${BASE}/exit-pages?${qs(siteId, from, to, { limit, filters })}`),
  customEvents: (siteId: string, from: string, to: string, limit = 20, filters?: Record<string, string>) =>
    get<CustomEventStat[]>(`${BASE}/events?${qs(siteId, from, to, { limit, filters })}`),
  funnel: (siteId: string, from: string, to: string, steps: FunnelStep[]) =>
    post<FunnelResult[]>(`${BASE}/funnel`, { site_id: siteId, from, to, steps }),
  retention: (siteId: string, from: string, to: string, periodDays?: number) =>
    get<RetentionCohort[]>(`${BASE}/retention?${qs(siteId, from, to)}${periodDays ? `&period_days=${periodDays}` : ""}`),
  journeys: (siteId: string, from: string, to: string) =>
    get<JourneyResult>(`${BASE}/journeys?${qs(siteId, from, to)}`),
  goals: (siteId: string) =>
    get<GoalConversion[]>(`/api/v1/goals?site_id=${siteId}`),
  createGoal: (data: { site_id: string; name: string; goal_type: string; goal_value: string }) =>
    post<Goal>(`/api/v1/goals`, data),
  correlations: (siteId: string, from: string, to: string, target?: string) =>
    get<Correlation[]>(`${BASE}/correlations?${qs(siteId, from, to)}${target ? `&target=${encodeURIComponent(target)}` : ""}`),
};
