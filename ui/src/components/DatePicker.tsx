import { useState, useRef, useEffect } from "preact/hooks";
import { useFilters } from "../hooks/useFilters.js";

interface RangePreset {
  label: string;
  getRange: () => { from: string; to: string };
}

function startOfDay(d: Date): Date {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate());
}

/** Label the dropdown shows while a hand-picked range is active. */
export const CUSTOM_LABEL = "Custom";

// One rolling window per row of magnitude, the set Plausible and Fathom
// settled on. The calendar-to-date twins this used to carry alongside them
// ("This week" next to "Last 7 days", "This month" next to "Last 30 days",
// "This year" next to "Last 12 months") named almost the same window as their
// neighbour, so the menu asked for a choice that did not change the answer.
// Custom covers the case a to-date window was actually wanted.
//
// Nothing persists the label — it lives only in the in-memory dashboard
// state — so no saved view, board or scheduled report can hold a removed one.
const PRESETS: RangePreset[] = [
  {
    label: "Today",
    getRange: () => ({
      from: startOfDay(new Date()).toISOString(),
      to: new Date().toISOString(),
    }),
  },
  {
    label: "Last 24 hours",
    getRange: () => ({
      from: new Date(Date.now() - 86400000).toISOString(),
      to: new Date().toISOString(),
    }),
  },
  {
    label: "Last 7 days",
    getRange: () => ({
      from: new Date(Date.now() - 7 * 86400000).toISOString(),
      to: new Date().toISOString(),
    }),
  },
  {
    label: "Last 30 days",
    getRange: () => ({
      from: new Date(Date.now() - 30 * 86400000).toISOString(),
      to: new Date().toISOString(),
    }),
  },
  {
    label: "Last 90 days",
    getRange: () => ({
      from: new Date(Date.now() - 90 * 86400000).toISOString(),
      to: new Date().toISOString(),
    }),
  },
  {
    label: "Last 12 months",
    getRange: () => ({
      from: new Date(Date.now() - 365 * 86400000).toISOString(),
      to: new Date().toISOString(),
    }),
  },
  {
    label: "All time",
    getRange: () => ({
      from: new Date("2020-01-01").toISOString(),
      to: new Date().toISOString(),
    }),
  },
];

function getRangeDurationMs(from: string, to: string): number {
  return new Date(to).getTime() - new Date(from).getTime();
}

/** ISO instant to the `yyyy-mm-dd` an <input type="date"> expects, in local time. */
function toDateInput(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

function DatePicker() {
  const { state, dispatch } = useFilters();
  const [open, setOpen] = useState(false);
  const [customOpen, setCustomOpen] = useState(false);
  const [customFrom, setCustomFrom] = useState("");
  const [customTo, setCustomTo] = useState("");
  const dropdownRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    function handleClickOutside(e: MouseEvent) {
      if (dropdownRef.current && !dropdownRef.current.contains(e.target as Node)) {
        setOpen(false);
        setCustomOpen(false);
      }
    }
    document.addEventListener("mousedown", handleClickOutside);
    return () => document.removeEventListener("mousedown", handleClickOutside);
  }, []);

  function selectPreset(preset: RangePreset) {
    const { from, to } = preset.getRange();
    dispatch({ type: "SET_RANGE", from, to, label: preset.label });
    setCustomOpen(false);
    setOpen(false);
  }

  function openCustom() {
    setCustomFrom(toDateInput(state.from));
    setCustomTo(toDateInput(state.to));
    setCustomOpen(true);
  }

  function applyCustom() {
    if (!customFrom || !customTo) return;
    const from = new Date(`${customFrom}T00:00:00`);
    // Inclusive of the chosen end day: the query range is half-open.
    const to = new Date(`${customTo}T00:00:00`);
    to.setDate(to.getDate() + 1);
    if (isNaN(from.getTime()) || isNaN(to.getTime()) || to <= from) return;
    dispatch({
      type: "SET_RANGE",
      from: from.toISOString(),
      to: to.toISOString(),
      label: CUSTOM_LABEL,
    });
    setCustomOpen(false);
    setOpen(false);
  }

  function shiftRange(direction: -1 | 1) {
    const duration = getRangeDurationMs(state.from, state.to);
    const shift = duration * direction;
    const newFrom = new Date(new Date(state.from).getTime() + shift).toISOString();
    const newTo = new Date(new Date(state.to).getTime() + shift).toISOString();
    dispatch({ type: "SET_RANGE", from: newFrom, to: newTo, label: state.rangeLabel });
  }

  function toggleCompare() {
    if (state.compare) {
      dispatch({ type: "SET_COMPARE", compare: null });
    } else {
      dispatch({ type: "SET_COMPARE", compare: "previous_period" });
    }
  }

  return (
    <div style="display:flex;align-items:center;gap:8px;">
      <div class="obs-nav-arrows">
        <button class="obs-nav-arrow" onClick={() => shiftRange(-1)} title="Previous period">
          {"\u2190"}
        </button>
        <button class="obs-nav-arrow" onClick={() => shiftRange(1)} title="Next period">
          {"\u2192"}
        </button>
      </div>
      <div class="obs-dropdown" ref={dropdownRef}>
        <button
          class={`obs-btn ${open ? "obs-btn-active" : ""}`}
          onClick={() => setOpen(!open)}
        >
          {state.rangeLabel}
        </button>
        {open && (
          <div class="obs-dropdown-menu">
            {PRESETS.map((preset) => (
              <button
                key={preset.label}
                class={`obs-dropdown-item ${state.rangeLabel === preset.label ? "obs-dropdown-item-active" : ""}`}
                onClick={() => selectPreset(preset)}
              >
                {preset.label}
              </button>
            ))}
            <button
              class={`obs-dropdown-item ${state.rangeLabel === CUSTOM_LABEL ? "obs-dropdown-item-active" : ""}`}
              onClick={openCustom}
            >
              {CUSTOM_LABEL}
            </button>
            {customOpen && (
              <div style="display:flex;flex-direction:column;gap:6px;padding:8px 12px;">
                <input
                  class="obs-input"
                  type="date"
                  value={customFrom}
                  onInput={(e) => setCustomFrom((e.target as HTMLInputElement).value)}
                  aria-label="Start date"
                />
                <input
                  class="obs-input"
                  type="date"
                  value={customTo}
                  onInput={(e) => setCustomTo((e.target as HTMLInputElement).value)}
                  aria-label="End date"
                />
                <button class="obs-btn obs-btn--sm" onClick={applyCustom}>Apply</button>
              </div>
            )}
            <div class="obs-compare-section">
              <div class="obs-compare-toggle" onClick={toggleCompare}>
                <div class={`obs-toggle-track ${state.compare ? "active" : ""}`}>
                  <div class="obs-toggle-thumb" />
                </div>
                Compare to previous period
              </div>
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

DatePicker.displayName = "DatePicker";
export default DatePicker;
