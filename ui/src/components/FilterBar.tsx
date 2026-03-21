import { useFilters } from "../hooks/useFilters.js";

function FilterBar() {
  const { state, dispatch } = useFilters();
  const entries = Object.entries(state.filters);

  if (entries.length === 0) return null;

  return (
    <div class="obs-filter-bar">
      {entries.map(([key, value]) => (
        <span key={key} class="obs-pill">
          <span class="obs-pill-key">{key}:</span> {value}
          <button
            class="obs-pill-remove"
            onClick={() => dispatch({ type: "REMOVE_FILTER", key })}
            title={`Remove ${key} filter`}
          >
            {"\u00D7"}
          </button>
        </span>
      ))}
      {entries.length > 1 && (
        <button
          class="obs-filter-clear"
          onClick={() => dispatch({ type: "CLEAR_FILTERS" })}
        >
          Clear all
        </button>
      )}
    </div>
  );
}

FilterBar.displayName = "FilterBar";
export default FilterBar;
