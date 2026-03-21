// API client for Observe backend endpoints.
// In production, the dashboard is served from the same binary,
// so all fetches are same-origin (no CORS needed).

const BASE = "/api/v1/stats";

export interface OverviewStats {
  pageviews: number;
  visitors: number;
  sessions: number;
  bounce_rate: number;
  avg_duration: number;
}

export interface OverviewResponse {
  current: OverviewStats;
  previous?: OverviewStats;
}

export interface TimeSeriesPoint {
  bucket: number;
  pageviews: number;
  visitors: number;
}

export interface TopPage {
  pathname: string;
  pageviews: number;
  visitors: number;
}

export interface TopReferrer {
  referrer: string;
  visitors: number;
}

export interface BrowserStat {
  browser: string;
  visitors: number;
}

export interface CountryStat {
  country: string;
  visitors: number;
}

export interface OSStat {
  os: string;
  visitors: number;
}

export interface DeviceStat {
  device: string;
  visitors: number;
}

export interface ChannelStat {
  channel: string;
  visitors: number;
}

export interface LanguageStat {
  language: string;
  visitors: number;
}

export interface ScreenStat {
  screen: string;
  visitors: number;
}

export interface UTMStat {
  value: string;
  visitors: number;
}

export interface EntryPageStat {
  pathname: string;
  visitors: number;
}

export interface ExitPageStat {
  pathname: string;
  visitors: number;
}

export interface CustomEventStat {
  event_type: string;
  count: number;
  visitors: number;
}

export interface RealtimeResult {
  active_visitors: number;
}

const enc = encodeURIComponent;

function qs(siteId: string, from: string, to: string, opts?: {
  limit?: number;
  compare?: string;
  filters?: Record<string, string>;
  interval?: string;
  type?: string;
}): string {
  let q = `site_id=${enc(siteId)}&from=${enc(from)}&to=${enc(to)}`;
  if (opts?.limit) q += `&limit=${opts.limit}`;
  if (opts?.compare) q += `&compare=${enc(opts.compare)}`;
  if (opts?.interval) q += `&interval=${enc(opts.interval)}`;
  if (opts?.type) q += `&type=${enc(opts.type)}`;
  if (opts?.filters) {
    for (const [k, v] of Object.entries(opts.filters)) {
      if (v) q += `&${enc(k)}=${enc(v)}`;
    }
  }
  return q;
}

async function get<T>(path: string): Promise<T> {
  const res = await fetch(path);
  if (!res.ok) throw new Error(`API ${res.status}: ${res.statusText}`);
  return res.json();
}

export const api = {
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
};
