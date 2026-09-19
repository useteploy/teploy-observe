import { createContext } from "preact";
import { useContext, useEffect, useReducer, useRef, useState } from "preact/hooks";
import type { ComponentChildren } from "preact";
import { h } from "preact";
import { defaultRange, loadRange, saveRange } from "../utils/ranges.js";
import type { PersistedRange } from "../utils/ranges.js";

export interface DashboardState {
  siteId: string;
  from: string;
  to: string;
  rangeLabel: string;
  /** See PersistedRange.rolling. */
  rangeRolling: boolean;
  compare: string | null;
  filters: Record<string, string>;
  interval: string;
}

export type Action =
  | { type: "SET_SITE"; siteId: string }
  | { type: "SET_RANGE"; from: string; to: string; label: string; rolling?: boolean }
  | { type: "SET_COMPARE"; compare: string | null }
  | { type: "SET_FILTER"; key: string; value: string }
  | { type: "REMOVE_FILTER"; key: string }
  | { type: "CLEAR_FILTERS" }
  | { type: "SET_INTERVAL"; interval: string };

function reducer(state: DashboardState, action: Action): DashboardState {
  switch (action.type) {
    case "SET_SITE":
      return { ...state, siteId: action.siteId };
    case "SET_RANGE":
      return {
        ...state,
        from: action.from,
        to: action.to,
        rangeLabel: action.label,
        rangeRolling: action.rolling === true,
      };
    case "SET_COMPARE":
      return { ...state, compare: action.compare };
    case "SET_FILTER":
      return { ...state, filters: { ...state.filters, [action.key]: action.value } };
    case "REMOVE_FILTER": {
      const filters = { ...state.filters };
      delete filters[action.key];
      return { ...state, filters };
    }
    case "CLEAR_FILTERS":
      return { ...state, filters: {} };
    case "SET_INTERVAL":
      return { ...state, interval: action.interval };
    default:
      return state;
  }
}

interface FilterContextValue {
  state: DashboardState;
  dispatch: (action: Action) => void;
}

const FilterContext = createContext<FilterContextValue | null>(null);

/** localStorage key used by the SiteSwitcher to remember the last selected site. */
export const SITE_STORAGE_KEY = "observe.site_id";

export { RANGE_STORAGE_KEY, DEFAULT_RANGE_LABEL } from "../utils/ranges.js";

/** PersistedRange to the DashboardState fields that hold it. */
function rangeState(r: PersistedRange) {
  return { from: r.from, to: r.to, rangeLabel: r.label, rangeRolling: r.rolling };
}

/**
 * Resolve initial siteId from URL `?site_id=`, then localStorage, then "default".
 * Asynchronous fetch of the user's first site happens via `RouteFilterProvider`
 * once the component mounts; this is the synchronous best-effort guess.
 */
function initialSiteId(): string | null {
  if (typeof window === "undefined") return null;
  const urlSite = new URLSearchParams(window.location.search).get("site_id");
  if (urlSite) return urlSite;
  try {
    const stored = window.localStorage.getItem(SITE_STORAGE_KEY);
    if (stored) return stored;
  } catch { /* localStorage may be disabled */ }
  return null;
}

/**
 * Read `?cohort_id=` from the URL on first mount so deep links from
 * /cohorts → /insights round-trip the active cohort filter without an
 * extra click. The cohort lives in the regular `filters` map alongside
 * pathname / browser / etc. so every existing query helper picks it up
 * via the qs() helper without further wiring.
 */
function initialFilters(): Record<string, string> {
  if (typeof window === "undefined") return {};
  const cohortID = new URLSearchParams(window.location.search).get("cohort_id");
  return cohortID ? { cohort_id: cohortID } : {};
}

export function FilterProvider({ siteId, children }: { siteId: string; children: ComponentChildren }) {
  // Bare routes (share links, embeds) deliberately do NOT adopt the operator's
  // remembered range — they render the window their own URL asks for.
  const [state, dispatch] = useReducer(reducer, {
    siteId,
    ...rangeState(defaultRange()),
    compare: null,
    filters: {},
    interval: "hour",
  });

  return h(FilterContext.Provider, { value: { state, dispatch } }, children);
}

/**
 * Layout-level provider used by every non-bare route. Derives initial siteId
 * from URL → localStorage → "default" synchronously, then asynchronously falls
 * back to the user's first site if neither URL nor storage had one.
 *
 * Also keeps URL `?site_id=` and localStorage in sync whenever `SET_SITE`
 * dispatches, so deep-links and reloads round-trip the selection.
 */
export function RouteFilterProvider({ children }: { children: ComponentChildren }) {
  // Capture intent before effects canonicalize the URL or write storage.
  const initialSelection = useRef(initialSiteId());
  const [siteReady, setSiteReady] = useState(Boolean(initialSelection.current));
  const [state, dispatch] = useReducer(reducer, {
    siteId: initialSelection.current || "default",
    ...rangeState(loadRange()),
    compare: null,
    filters: initialFilters(),
    interval: "hour",
  });

  // Persist the selected range, the same way the selected site persists just
  // below. Routing remounts this provider, so without it the range resets to
  // "Last 24 hours" on every navigation.
  useEffect(() => {
    saveRange({
      from: state.from,
      to: state.to,
      label: state.rangeLabel,
      rolling: state.rangeRolling,
    });
  }, [state.from, state.to, state.rangeLabel, state.rangeRolling]);

  // Persist + canonicalize whenever siteId changes.
  useEffect(() => {
    if (typeof window === "undefined" || !siteReady) return;
    try { window.localStorage.setItem(SITE_STORAGE_KEY, state.siteId); } catch { /* ignore */ }

    const url = new URL(window.location.href);
    if (url.searchParams.get("site_id") !== state.siteId) {
      url.searchParams.set("site_id", state.siteId);
      window.history.replaceState(null, "", url.toString());
    }
  }, [state.siteId, siteReady]);

  // Async: if neither URL nor localStorage had a value, fetch the user's first
  // site and adopt it. Avoids leaving a stale "default" selection when the
  // tenant has only renamed sites.
  useEffect(() => {
    if (typeof window === "undefined") return;
    if (initialSelection.current || siteReady) return;
    // A selection made while discovery is pending wins over the fallback.
    if (state.siteId !== "default") { setSiteReady(true); return; }

    let cancelled = false;
    const token = (() => { try { return window.localStorage.getItem("obs_token"); } catch { return null; } })();
    const headers: Record<string, string> = {};
    if (token) headers["Authorization"] = `Bearer ${token}`;
    fetch("/api/v1/sites", { headers })
      .then(r => r.ok ? r.json() : null)
      .then((sites: Array<{ site_id: string }> | null) => {
        if (cancelled || !sites?.length) return;
        // Prefer a configured site over the bootstrap fallback.
        const first = (sites.find(site => site.site_id !== "default") || sites[0]).site_id;
        if (first && first !== state.siteId) dispatch({ type: "SET_SITE", siteId: first });
      })
      .catch(() => { /* keep default */ })
      .finally(() => { if (!cancelled) setSiteReady(true); });
    return () => { cancelled = true; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [state.siteId, siteReady]);

  return h(FilterContext.Provider, { value: { state, dispatch } }, children);
}

export function useFilters(): FilterContextValue {
  const ctx = useContext(FilterContext);
  if (!ctx) throw new Error("useFilters must be used within FilterProvider");
  return ctx;
}
