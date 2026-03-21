import { createContext } from "preact";
import { useContext, useReducer } from "preact/hooks";
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
  | { type: "SET_RANGE"; from: string; to: string; label: string }
  | { type: "SET_COMPARE"; compare: string | null }
  | { type: "SET_FILTER"; key: string; value: string }
  | { type: "REMOVE_FILTER"; key: string }
  | { type: "CLEAR_FILTERS" }
  | { type: "SET_INTERVAL"; interval: string };

function reducer(state: DashboardState, action: Action): DashboardState {
  switch (action.type) {
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

export function useFilters(): FilterContextValue {
  const ctx = useContext(FilterContext);
  if (!ctx) throw new Error("useFilters must be used within FilterProvider");
  return ctx;
}
