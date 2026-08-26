// Analytics stats API — pageviews, visitors, breakdowns, etc.

import { get, post, put, del, qs } from "./helpers.js";

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

// How much of the selected range the visitor figures actually describe.
// Pageviews come from the rollups (a year, then indefinitely) while a unique
// count has to be counted from a table holding one row per thing counted —
// raw events, or the session-grain sessions table — and those are pruned
// sooner. `exact: false` means the number is real but covers a shorter window
// than the one picked, and `note` is the sentence to show.
export interface UniqueCoverage {
  source: "events" | "sessions";
  exact: boolean;
  range_from: number;
  covered_from: number;
  covered_days: number;
  note: string;
}

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
/** A conversion goal.
 *
 *  `goal_value` is the MATCHER — the pathname or event_type a conversion is
 *  recognised by — and has nothing to do with money. The money is
 *  `value_minor`, an integer count of `currency`'s ISO-4217 minor units
 *  (cents for USD, whole yen for JPY). Format it with utils/money.ts; never
 *  divide by 100 by hand. */
export interface Goal {
  goal_id: string; site_id: string; name: string; goal_type: string; goal_value: string;
  value_minor: number;
  /** ISO-4217 code, or "" when the goal carries no value. Never assume USD. */
  currency: string;
  /** "fixed" — every conversion is worth value_minor.
   *  "event" — each event carries its own amount in value_property. */
  value_source: string;
  value_property: string;
  created_at?: string;
}
export interface GoalConversion {
  goal: Goal;
  /** Distinct sessions that converted. */
  conversions: number;
  /** Conversion events. A session that buys twice is one conversion and two
   *  events; money is summed over events, so this is what the value matches. */
  conversion_events: number;
  visitors: number;
  rate: number;
  /** Period total in goal.currency's minor units. */
  total_value_minor: number;
}
export interface GoalInput {
  site_id: string; name: string; goal_type: string; goal_value: string;
  value_minor?: number; currency?: string;
  value_source?: string; value_property?: string;
}
export interface Correlation {
  property: string; value: string; uplift: number;
  occurrences: number; conversions: number; rate: number;
  baseline_rate: number; significant: boolean;
}

export interface PropertyStat {
  key: string; value: string; count: number; visitors: number;
}

// Multi-touch attribution row. Sessions/Conversions are floats because
// the linear model splits credit fractionally (1/N per unique source).
export type AttributionModel = "first" | "last" | "linear";
export interface AttributionRow {
  source: string;
  sessions: number;
  conversions: number;
  conversion_pct: number;
}

export const analyticsApi = {
  realtime: (siteId: string, minutes = 5) =>
    get<RealtimeResult>(`${BASE}/realtime?site_id=${siteId}&minutes=${minutes}`),
  uniqueCoverage: (siteId: string, from: string, to: string, filters?: Record<string, string>) =>
    get<UniqueCoverage>(`${BASE}/unique-coverage?${qs(siteId, from, to, { filters })}`),
  overview: (siteId: string, from: string, to: string, compare?: string | null, filters?: Record<string, string>) =>
    get<OverviewResponse | OverviewStats>(
      `${BASE}/overview?${qs(siteId, from, to, { compare: compare || undefined, filters })}`
    ).then((r): OverviewResponse => {
      // Normalize: backend returns flat stats when no compare, {current, previous} when compare is set.
      if (r && typeof r === "object" && "current" in r) return r as OverviewResponse;
      return { current: r as OverviewStats };
    }),
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
  eventProperties: (siteId: string, from: string, to: string, eventType: string) =>
    get<PropertyStat[]>(`${BASE}/event-properties?${qs(siteId, from, to)}&event_type=${encodeURIComponent(eventType)}`),
  funnel: (siteId: string, from: string, to: string, steps: FunnelStep[]) =>
    post<FunnelResult[]>(`${BASE}/funnel`, { site_id: siteId, from, to, steps }),
  funnelBreakdown: (siteId: string, from: string, to: string, steps: FunnelStep[], breakdownBy: string, minSize = 5) =>
    post<Array<{ breakdown: string; results: FunnelResult[] }>>(
      `${BASE}/funnel/breakdown`,
      { site_id: siteId, from, to, steps, breakdown_by: breakdownBy, min_size: minSize },
    ),
  retention: (siteId: string, from: string, to: string, periodDays?: number) =>
    get<RetentionCohort[]>(`${BASE}/retention?${qs(siteId, from, to)}${periodDays ? `&period_days=${periodDays}` : ""}`),
  journeys: (siteId: string, from: string, to: string) =>
    get<JourneyResult>(`${BASE}/journeys?${qs(siteId, from, to)}`),
  // from/to matter: conversions and their value are counted over the window
  // the page is showing, and omitting them silently fell back to the server's
  // default range while the UI claimed it was the selected period.
  goals: (siteId: string, from?: string, to?: string) =>
    get<GoalConversion[]>(
      `/api/v1/goals?site_id=${encodeURIComponent(siteId)}` +
      (from ? `&from=${encodeURIComponent(from)}` : "") +
      (to ? `&to=${encodeURIComponent(to)}` : ""),
    ),
  createGoal: (data: GoalInput) =>
    post<Goal>(`/api/v1/goals`, data),
  updateGoal: (goalId: string, data: GoalInput) =>
    put<Goal>(`/api/v1/goals/${encodeURIComponent(goalId)}`, data),
  deleteGoal: (goalId: string, siteId: string) =>
    del(`/api/v1/goals/${encodeURIComponent(goalId)}?site_id=${encodeURIComponent(siteId)}`),
  correlations: (siteId: string, from: string, to: string, target?: string) =>
    get<Correlation[]>(`${BASE}/correlations?${qs(siteId, from, to)}${target ? `&target=${encodeURIComponent(target)}` : ""}`),
  // Attribution endpoint lives outside /stats — it's at /api/v1/attribution.
  attribution: (siteId: string, from: string, to: string, model: AttributionModel) =>
    get<AttributionRow[]>(`/api/v1/attribution?${qs(siteId, from, to)}&model=${encodeURIComponent(model)}`),
};
