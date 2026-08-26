// Routes the command palette can jump to, and the matcher that ranks them.
// Extracted from CommandPalette.tsx so the search can be tested without
// rendering Preact — the palette is the only way to reach several of these
// pages by name, so what it does and does not match is behaviour, not detail.

export interface PaletteRoute {
  label: string;
  path: string;
  description?: string;
  /** Extra words to match on. The palette searches label + description +
   *  path + keywords, so this is where the vocabulary a user actually types
   *  goes when it does not belong in a label. */
  keywords?: string;
}

export const ROUTES: PaletteRoute[] = [
  { label: "Dashboard", path: "/", description: "Site overview" },
  { label: "Get started", path: "/onboard", description: "Onboarding wizard" },
  { label: "Dashboards", path: "/dashboards", description: "Custom panels" },
  { label: "Events", path: "/events" },
  { label: "Campaigns", path: "/campaigns", description: "UTM breakdown" },
  {
    label: "Funnels & Goals",
    path: "/insights",
    description: "Conversion funnels, goals and revenue, retention, journeys",
    // The words people type when they are looking for this page and do not
    // know Observe calls any of it "insights".
    keywords: "insights conversion conversions convert funnel funnels goal goals revenue value correlation correlations retention journeys drop-off dropoff",
  },
  { label: "Errors", path: "/errors" },
  { label: "Releases", path: "/releases", description: "Error health per release" },
  { label: "Traces", path: "/traces", description: "APM / distributed tracing" },
  { label: "Logs", path: "/logs" },
  { label: "Flags", path: "/flags", description: "Feature flags" },
  { label: "Experiments", path: "/experiments" },
  { label: "Sessions", path: "/sessions", description: "Replay player" },
  { label: "Monitoring", path: "/monitoring", description: "Uptime & infra" },
  { label: "Alerts", path: "/alerts" },
  { label: "LLM", path: "/llm" },
  { label: "Surveys", path: "/surveys" },
  { label: "Integrations", path: "/integrations" },
  { label: "Reports", path: "/reports" },
  { label: "Explorer", path: "/explorer", description: "SQL console" },
  { label: "Docs", path: "/docs" },
  { label: "API reference", path: "/api/docs", description: "OpenAPI / Swagger UI" },
  { label: "Settings", path: "/settings" },
];

/** The searchable text of a route: everything a query may match against. */
export function routeKeywords(r: PaletteRoute): string {
  return [r.label, r.description ?? "", r.path, r.keywords ?? ""].join(" ");
}

export function fuzzyScore(query: string, text: string): number {
  if (!query) return 1;
  const q = query.toLowerCase();
  const t = text.toLowerCase();
  if (t === q) return 100;
  if (t.startsWith(q)) return 50;
  if (t.includes(q)) return 25;
  // fallback: subsequence match
  let qi = 0;
  for (let ti = 0; ti < t.length && qi < q.length; ti++) {
    if (t[ti] === q[qi]) qi++;
  }
  if (qi === q.length) return 10;
  return 0;
}

/** Routes matching query, best first. Used by the palette and by its tests. */
export function searchRoutes(query: string): PaletteRoute[] {
  return ROUTES.map((r) => ({ r, score: fuzzyScore(query, routeKeywords(r)) }))
    .filter((x) => x.score > 0)
    .sort((a, b) => b.score - a.score)
    .map((x) => x.r);
}
