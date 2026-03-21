import { useState, useRef, useEffect } from "preact/hooks";
import { useFilters } from "../hooks/useFilters.js";

interface RangePreset {
  label: string;
  getRange: () => { from: string; to: string };
}

function startOfDay(d: Date): Date {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate());
}

function startOfWeek(d: Date): Date {
  const day = d.getDay();
  const diff = d.getDate() - day + (day === 0 ? -6 : 1);
  return new Date(d.getFullYear(), d.getMonth(), diff);
}

function startOfMonth(d: Date): Date {
  return new Date(d.getFullYear(), d.getMonth(), 1);
}

function startOfYear(d: Date): Date {
  return new Date(d.getFullYear(), 0, 1);
}

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
    label: "This week",
    getRange: () => ({
      from: startOfWeek(new Date()).toISOString(),
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
    label: "This month",
    getRange: () => ({
      from: startOfMonth(new Date()).toISOString(),
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
    label: "This year",
    getRange: () => ({
      from: startOfYear(new Date()).toISOString(),
      to: new Date().toISOString(),
    }),
  },
  {
    label: "Last 6 months",
    getRange: () => ({
      from: new Date(Date.now() - 180 * 86400000).toISOString(),
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

function DatePicker() {
  const { state, dispatch } = useFilters();
  const [open, setOpen] = useState(false);
  const dropdownRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    function handleClickOutside(e: MouseEvent) {
      if (dropdownRef.current && !dropdownRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    }
    document.addEventListener("mousedown", handleClickOutside);
    return () => document.removeEventListener("mousedown", handleClickOutside);
  }, []);

  function selectPreset(preset: RangePreset) {
    const { from, to } = preset.getRange();
    dispatch({ type: "SET_RANGE", from, to, label: preset.label });
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
