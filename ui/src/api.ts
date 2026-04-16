// Backward-compatible re-export. New code should import from api/ modules directly.
// e.g. import { errorsApi } from "./api/errors.js"

export type {
  OverviewStats, OverviewResponse, TimeSeriesPoint, TopPage, TopReferrer,
  BrowserStat, CountryStat, OSStat, DeviceStat, ChannelStat, LanguageStat,
  ScreenStat, UTMStat, EntryPageStat, ExitPageStat, CustomEventStat, RealtimeResult,
  PropertyStat,
} from "./api/analytics.js";

import { analyticsApi } from "./api/analytics.js";

// Legacy `api` object — maps to analyticsApi for existing dashboard components.
export const api = analyticsApi;
