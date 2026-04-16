// Session replay API — list sessions, view events.

import { get } from "./helpers.js";

const BASE = "/api/v1/replays";

export interface ReplaySession {
  replay_id: string;
  site_id: string;
  session_id: string;
  start_time: string;
  duration_ms: number;
  page_count: number;
  url: string;
  browser: string;
  os: string;
  device: string;
  has_error: boolean;
}

export interface ReplayEvent {
  event_id: string;
  replay_id: string;
  timestamp: string;
  event_type: string;
  data: string;
}

export const replaysApi = {
  list: (siteId: string, from: string, to: string, opts?: { limit?: number; offset?: number }) => {
    let q = `site_id=${siteId}&from=${from}&to=${to}`;
    if (opts?.limit) q += `&limit=${opts.limit}`;
    if (opts?.offset) q += `&offset=${opts.offset}`;
    return get<ReplaySession[]>(`${BASE}?${q}`);
  },
  events: (replayId: string) =>
    get<ReplayEvent[]>(`${BASE}/${replayId}`),
};
