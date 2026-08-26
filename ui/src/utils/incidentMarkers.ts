/**
 * Incident markers are shaded vertical bands drawn behind a time series. There
 * is no bound on how many the API can return for a window, and drawing them
 * one-to-one does not degrade — it inverts. Each band is translucent, so N
 * overlapping bands compose to N times the alpha; past a few dozen the plot is
 * a solid block of colour with the series invisible underneath it, and every
 * band's leading-edge stroke adds a vertical line, so it reads as deliberate
 * striping rather than as "something is wrong with this data".
 *
 * That is not hypothetical. A missed-run detector on a 45s tick put 6,206
 * incident markers into one site's window on the live instance.
 *
 * prepareMarkers is the whole defence, and it is pure so it can be tested
 * without a canvas: clamp to the plot window, merge what would overlap or sit
 * within a few pixels of each other, then cap what is left and report how many
 * were dropped so the chart can say so.
 */

export interface IncidentMarker {
  id: string;
  title: string;
  severity: string;
  started_at: number;
  /** 0 means ongoing — treated as running to the end of the window. */
  ended_at: number;
}

export interface MarkerBand {
  start: number;
  end: number;
  severity: string;
  /** How many incidents this band represents. */
  count: number;
  /** The single incident's title, or a summary when count > 1. */
  label: string;
  /** True when the incident is still open at the end of the window. */
  ongoing: boolean;
}

export interface PrepareOptions {
  /** Left edge of the plot, in epoch ms. */
  minT: number;
  /** Right edge of the plot, in epoch ms. */
  maxT: number;
  /**
   * Bands closer together than this are merged. Callers pass the millisecond
   * equivalent of a few pixels, so the merge threshold follows the zoom level
   * rather than being a fixed duration.
   */
  mergeGapMs: number;
  /** Hard cap on bands returned. */
  maxBands: number;
}

export interface PreparedMarkers {
  bands: MarkerBand[];
  /** Incidents in the window that no returned band represents. */
  hidden: number;
  /** Incidents in the window, before merging and capping. */
  total: number;
}

/** Severity rank, so the more urgent band wins when two are merged. */
function severityRank(s: string): number {
  switch (s) {
    case "critical": return 3;
    case "warning": return 2;
    default: return 1;
  }
}

export function prepareMarkers(
  markers: readonly IncidentMarker[],
  opts: PrepareOptions,
): PreparedMarkers {
  const { minT, maxT } = opts;
  const mergeGapMs = Math.max(0, opts.mergeGapMs);
  const maxBands = Math.max(1, Math.floor(opts.maxBands));
  const span = maxT - minT;
  if (!Number.isFinite(span) || span <= 0) {
    return { bands: [], hidden: 0, total: 0 };
  }

  // Clamp into the window and drop anything that misses it entirely. An
  // ended_at of 0 is an incident still open, so it runs to the right edge.
  const clamped: MarkerBand[] = [];
  for (const m of markers) {
    const rawStart = Number(m.started_at);
    const rawEndedAt = Number(m.ended_at);
    if (!Number.isFinite(rawStart)) continue;
    const ongoing = !Number.isFinite(rawEndedAt) || rawEndedAt === 0;
    const rawEnd = ongoing ? maxT : rawEndedAt;
    if (rawStart > maxT || rawEnd < minT) continue;
    const start = Math.max(minT, Math.min(rawStart, maxT));
    const end = Math.max(start, Math.min(Math.max(rawEnd, rawStart), maxT));
    clamped.push({
      start,
      end,
      severity: m.severity,
      count: 1,
      label: m.title,
      ongoing,
    });
  }
  const total = clamped.length;
  if (total === 0) return { bands: [], hidden: 0, total: 0 };

  // Merge per severity so a merged band keeps a colour that means something.
  // Sorting is by start then end, a total order, so the merge is deterministic
  // whatever order the API returned.
  const bySeverity = new Map<string, MarkerBand[]>();
  for (const b of clamped) {
    const list = bySeverity.get(b.severity);
    if (list) list.push(b);
    else bySeverity.set(b.severity, [b]);
  }

  const merged: MarkerBand[] = [];
  for (const list of bySeverity.values()) {
    list.sort((a, b) => (a.start - b.start) || (a.end - b.end));
    let cur: MarkerBand | null = null;
    for (const b of list) {
      if (cur && b.start <= cur.end + mergeGapMs) {
        cur.end = Math.max(cur.end, b.end);
        cur.count += b.count;
        cur.ongoing = cur.ongoing || b.ongoing;
        continue;
      }
      cur = { ...b };
      merged.push(cur);
    }
  }
  for (const b of merged) {
    if (b.count > 1) b.label = `${b.count} incidents`;
  }

  if (merged.length <= maxBands) {
    merged.sort(byDrawOrder);
    return { bands: merged, hidden: 0, total };
  }

  // Still too many. Keep the widest — they carry the most signal and dropping
  // slivers is what a reader would do — and report everything they do not
  // account for, so the chart never silently under-reports.
  const kept = merged
    .slice()
    .sort((a, b) => ((b.end - b.start) - (a.end - a.start)) || (b.start - a.start))
    .slice(0, maxBands);
  let shown = 0;
  for (const b of kept) shown += b.count;
  kept.sort(byDrawOrder);
  return { bands: kept, hidden: total - shown, total };
}

/**
 * Least urgent first, so a critical band is painted over a warning one rather
 * than under it; then by start, for a stable order.
 */
function byDrawOrder(a: MarkerBand, b: MarkerBand): number {
  return (severityRank(a.severity) - severityRank(b.severity)) || (a.start - b.start);
}

/**
 * The line under the chart legend describing what the bands stand for. Returns
 * "" when there is nothing to say, so the caller can render it unconditionally.
 */
export function markerSummary(prepared: PreparedMarkers): string {
  const { total, bands, hidden } = prepared;
  if (total === 0) return "";
  const noun = total === 1 ? "incident" : "incidents";
  if (hidden > 0) {
    return `${total} ${noun}, ${bands.length} shown, ${hidden} not drawn`;
  }
  if (bands.length < total) {
    return `${total} ${noun} in ${bands.length} ${bands.length === 1 ? "band" : "bands"}`;
  }
  return `${total} ${noun}`;
}
