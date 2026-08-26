/**
 * The date ranges the dashboard offers, and the label each one carries.
 *
 * These live here rather than inside DatePicker because two things need them
 * and neither should import the other: the picker renders them, and
 * hooks/useFilters rebuilds a persisted selection from its label. Doing that
 * through a registry the picker populated at import time would have made the
 * restore depend on module evaluation order, which code-splitting is free to
 * change.
 */

export interface RangePreset {
  label: string;
  getRange: () => { from: string; to: string };
}

/** Label a hand-picked range carries. Not a preset — it resolves to nothing. */
export const CUSTOM_LABEL = "Custom";

function startOfDay(d: Date): Date {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate());
}

// One rolling window per row of magnitude, the set Plausible and Fathom
// settled on. The calendar-to-date twins this used to carry alongside them
// ("This week" next to "Last 7 days", "This month" next to "Last 30 days",
// "This year" next to "Last 12 months") named almost the same window as their
// neighbour, so the menu asked for a choice that did not change the answer.
// Custom covers the case a to-date window was actually wanted.
//
// The label DOES persist, in localStorage under observe.range — so removing a
// preset from this list has to stay survivable. It is: loadRange falls back to
// the default range when a rolling label no longer resolves here, and keeps the
// instants under the Custom label when a pinned one does not.
export const PRESETS: RangePreset[] = [
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

/**
 * Recompute a preset by label, or null when this build no longer offers it.
 * Rolling by definition: the window is measured from now, not from whenever the
 * label was first chosen.
 */
export function resolvePreset(label: string): { from: string; to: string } | null {
  const preset = PRESETS.find((p) => p.label === label);
  return preset ? preset.getRange() : null;
}

/** localStorage key holding the dashboard's selected date range. */
export const RANGE_STORAGE_KEY = "observe.range";

/** The range every dashboard opens on when nothing has been chosen. */
export const DEFAULT_RANGE_LABEL = "Last 24 hours";

export interface PersistedRange {
  from: string;
  to: string;
  label: string;
  /**
   * True when the label names a rolling window that should be recomputed from
   * "now" on restore, false when from/to are the selection itself. Set at
   * dispatch: picking a preset is rolling, hand-picking a range or stepping the
   * arrows is not.
   */
  rolling: boolean;
}

export function defaultRange(): PersistedRange {
  const now = new Date();
  return {
    from: new Date(now.getTime() - 86400000).toISOString(),
    to: now.toISOString(),
    label: DEFAULT_RANGE_LABEL,
    rolling: true,
  };
}

/**
 * Restore the range chosen on a previous page. The dashboard is several routes
 * over one dataset, and re-picking "Last 30 days" after every navigation is the
 * kind of friction that makes the range feel like it does not belong to you.
 *
 * A ROLLING selection persists as its label, not its instants. "Last 24 hours"
 * names a window relative to now, so restoring a frozen from/to would silently
 * pin it to the twenty-four hours around whenever it was first picked, and the
 * dashboard would quietly stop showing today. A rolling label is therefore
 * recomputed from the live preset on every restore. A pinned selection — a
 * hand-picked Custom range, or one the arrows stepped off the preset — restores
 * its instants verbatim, which is the whole point of having chosen them.
 *
 * Everything about the stored value is treated as hostile: another tab, an older
 * build, or a hand-edited entry can all put something unusable there. A blocked
 * localStorage, unparseable JSON, a missing field, an instant that is not a
 * date, or a reversed range falls back to the default. A ROLLING label that no
 * longer resolves — four presets were removed on 2026-08-25 — also falls back to
 * the default rather than rendering an empty range, because there is no window
 * left to recompute. A PINNED one keeps its instants and is relabelled Custom,
 * since the dates are still real even though the name for them is gone.
 */
export function loadRange(): PersistedRange {
  if (typeof window === "undefined") return defaultRange();
  let raw: string | null = null;
  try {
    raw = window.localStorage.getItem(RANGE_STORAGE_KEY);
  } catch { /* localStorage may be disabled */ }
  if (!raw) return defaultRange();

  let stored: unknown;
  try {
    stored = JSON.parse(raw);
  } catch {
    return defaultRange();
  }
  if (!stored || typeof stored !== "object") return defaultRange();
  const { from, to, label, rolling } = stored as Record<string, unknown>;
  if (typeof label !== "string" || label === "") return defaultRange();

  if (rolling === true) {
    const preset = resolvePreset(label);
    if (!preset) return defaultRange();
    return { ...preset, label, rolling: true };
  }

  if (typeof from !== "string" || typeof to !== "string") return defaultRange();
  const fromMs = Date.parse(from);
  const toMs = Date.parse(to);
  if (!Number.isFinite(fromMs) || !Number.isFinite(toMs) || toMs <= fromMs) {
    return defaultRange();
  }
  const stillOffered = label === CUSTOM_LABEL || resolvePreset(label) !== null;
  return { from, to, label: stillOffered ? label : CUSTOM_LABEL, rolling: false };
}

/**
 * Remember the selection for the next page. Best-effort by design: a browser
 * with storage blocked keeps working, it just does not remember.
 */
export function saveRange(range: PersistedRange) {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.setItem(RANGE_STORAGE_KEY, JSON.stringify({
      from: range.from,
      to: range.to,
      label: range.label,
      rolling: range.rolling,
    } satisfies PersistedRange));
  } catch { /* ignore */ }
}
