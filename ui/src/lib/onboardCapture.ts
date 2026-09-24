// Onboarding capture validation (O13): pure helpers behind the onboard
// step-3 UI. The old UI had two honesty defects this module pins the fix
// for:
//
//  1. The install snippet pointed at /observe.js — a route the server does
//     not serve (the tracker is at /t/observe.js) — and omitted the
//     data-api-key attribute, which ingest requires on any instance with a
//     provisioned key. A user who followed it exactly could never capture,
//     and step 3 would poll forever saying "waiting".
//  2. Confirmation required a pageview INCREASE over a baseline captured at
//     entry, so a site that was already capturing (the user re-running
//     onboarding, or the tracker already installed) sat in "waiting for
//     first event" forever with data flowing in.

export type CaptureState =
  | { kind: "checking" }
  | { kind: "confirmed"; sinceSetup: boolean }
  | { kind: "waiting"; noneEver: boolean }
  | { kind: "no-capture" };

/**
 * Derive the step-3 state from the only two numbers the stats endpoint
 * gives us: pageviews in the recent window when setup started, and the
 * latest reading of the same window.
 *
 *  - baseline > 0          -> the site already captures: confirmed
 *  - current > baseline    -> an event arrived since setup: confirmed
 *  - current == baseline == 0 after a grace period -> no-capture (remedy)
 *  - otherwise             -> waiting (still polling)
 */
export function captureState(
  baseline: number | null,
  current: number | null,
  waitedMs: number,
  graceMs = 30_000,
): CaptureState {
  const base = baseline ?? 0;
  const cur = current ?? 0;
  if (base > 0) return { kind: "confirmed", sinceSetup: false };
  if (cur > base) return { kind: "confirmed", sinceSetup: true };
  if (waitedMs >= graceMs) return { kind: "no-capture" };
  return { kind: "waiting", noneEver: cur === 0 };
}

/** Build the tracker snippet the onboard page shows. It must match what the
 * server actually serves (/t/observe.js) and what ingest actually requires
 * (a site-scoped API key once any exists). */
export function trackerSnippet(origin: string, siteId: string, apiKey: string): string {
  const base = origin ? origin.replace(/\/+$/, "") : "";
  return `<script defer src="${base}/t/observe.js"\n  data-site-id="${siteId}"\n  data-api-key="${apiKey}"></script>`;
}
