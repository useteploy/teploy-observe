import { createContext } from "preact";
import { useContext, useEffect, useReducer } from "preact/hooks";
import type { ComponentChildren } from "preact";
import { h } from "preact";

export interface DashboardState {
  siteId: string;
  from: string;
  to: string;
  rangeLabel: string;
  compare: string | null;
  filters: Record<string, string>;
  interval: string;
}

export type Action =
  | { type: "SET_SITE"; siteId: string }
  | { type: "SET_RANGE"; from: string; to: string; label: string }
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
      return { ...state, from: action.from, to: action.to, rangeLabel: action.label };
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

/**
 * Resolve initial siteId from URL `?site_id=`, then localStorage, then "default".
 * Asynchronous fetch of the user's first site happens via `RouteFilterProvider`
 * once the component mounts; this is the synchronous best-effort guess.
 */
function initialSiteId(): string {
  if (typeof window === "undefined") return "default";
  const urlSite = new URLSearchParams(window.location.search).get("site_id");
  if (urlSite) return urlSite;
  try {
    const stored = window.localStorage.getItem(SITE_STORAGE_KEY);
    if (stored) return stored;
  } catch { /* localStorage may be disabled */ }
  return "default";
}

export function FilterProvider({ siteId, children }: { siteId: string; children: ComponentChildren }) {
  const now = new Date();
  const yesterday = new Date(now.getTime() - 86400000);

  const [state, dispatch] = useReducer(reducer, {
    siteId,
    from: yesterday.toISOString(),
    to: now.toISOString(),
    rangeLabel: "Last 24 hours",
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
  const now = new Date();
  const yesterday = new Date(now.getTime() - 86400000);

  const [state, dispatch] = useReducer(reducer, {
    siteId: initialSiteId(),
    from: yesterday.toISOString(),
    to: now.toISOString(),
    rangeLabel: "Last 24 hours",
    compare: null,
    filters: {},
    interval: "hour",
  });

  // Persist + canonicalize whenever siteId changes.
  useEffect(() => {
    if (typeof window === "undefined") return;
    try { window.localStorage.setItem(SITE_STORAGE_KEY, state.siteId); } catch { /* ignore */ }

    const url = new URL(window.location.href);
    if (url.searchParams.get("site_id") !== state.siteId) {
      url.searchParams.set("site_id", state.siteId);
      window.history.replaceState(null, "", url.toString());
    }
  }, [state.siteId]);

  // Async: if neither URL nor localStorage had a value, fetch the user's first
  // site and adopt it. Avoids leaving a stale "default" selection when the
  // tenant has only renamed sites.
  useEffect(() => {
    if (typeof window === "undefined") return;
    const urlHadIt = new URLSearchParams(window.location.search).get("site_id");
    let stored: string | null = null;
    try { stored = window.localStorage.getItem(SITE_STORAGE_KEY); } catch { /* ignore */ }
    if (urlHadIt || stored) return;

    let cancelled = false;
    const token = (() => { try { return window.localStorage.getItem("obs_token"); } catch { return null; } })();
    const headers: Record<string, string> = {};
    if (token) headers["Authorization"] = `Bearer ${token}`;
    fetch("/api/v1/sites", { headers })
      .then(r => r.ok ? r.json() : null)
      .then((sites: Array<{ site_id: string }> | null) => {
        if (cancelled || !sites?.length) return;
        const first = sites[0].site_id;
        if (first && first !== state.siteId) dispatch({ type: "SET_SITE", siteId: first });
      })
      .catch(() => { /* keep default */ });
    return () => { cancelled = true; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return h(FilterContext.Provider, { value: { state, dispatch } }, children);
}

export function useFilters(): FilterContextValue {
  const ctx = useContext(FilterContext);
  if (!ctx) throw new Error("useFilters must be used within FilterProvider");
  return ctx;
}
