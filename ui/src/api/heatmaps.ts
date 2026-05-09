// Click heatmap API — aggregated click coords for a URL.

import { get } from "./helpers.js";

const BASE = "/api/v1/heatmaps";

export interface Click {
  x: number;
  y: number;
  count: number;
}

export const heatmapsApi = {
  query: (siteId: string, url: string, from: string, to: string) => {
    const q = `site_id=${encodeURIComponent(siteId)}&url=${encodeURIComponent(url)}&from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}`;
    return get<Click[]>(`${BASE}?${q}`);
  },
};
