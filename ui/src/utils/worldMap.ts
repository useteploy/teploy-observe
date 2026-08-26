/**
 * Turns a `{ country, visitors }` breakdown into something the world map can
 * draw, and owns the two things worth testing without a browser: which ISO
 * alpha-2 code resolves to which piece of geometry, and what colour a count
 * gets.
 *
 * The geometry lives in ../data/world110m.ts (Natural Earth, public domain —
 * see ../data/README.md). Its path coordinates are degrees written as
 * "longitude,-latitude", so the SVG viewBox is itself the equirectangular
 * projection and nothing here has to project anything.
 */

import {
  WORLD_MARKERS,
  WORLD_SHAPES,
  WORLD_UNCODED,
  WORLD_VIEWBOX,
  type WorldMarker,
  type WorldShape,
} from "../data/world110m.ts";

export { WORLD_UNCODED, WORLD_VIEWBOX };

export interface CountryDatum {
  country: string;
  visitors: number;
}

/** A country drawn as a filled polygon. */
export interface ShapeCell {
  code: string;
  name: string;
  d: string;
  visitors: number;
  step: number;
}

/**
 * A country Natural Earth has no polygon for at 1:110m — Singapore, Malta,
 * Bahrain and friends. Drawn as a small disc at its official point so a real
 * visitor count is never invisible.
 */
export interface MarkerCell {
  code: string;
  name: string;
  x: number;
  y: number;
  visitors: number;
  step: number;
}

/** A code in the data that no geometry claims. Surfaced, never dropped. */
export interface UnmatchedCode {
  code: string;
  visitors: number;
}

export interface Choropleth {
  shapes: ShapeCell[];
  markers: MarkerCell[];
  unmatched: UnmatchedCode[];
  unmatchedVisitors: number;
  max: number;
}

const SHAPE_BY_CODE = new Map<string, WorldShape>();
for (const shape of WORLD_SHAPES) SHAPE_BY_CODE.set(shape[0], shape);

const MARKER_BY_CODE = new Map<string, WorldMarker>();
for (const marker of WORLD_MARKERS) MARKER_BY_CODE.set(marker[0], marker);

/** Radius of a point-only country, in degrees of the projection. */
export const MARKER_RADIUS = 1.1;

/**
 * How far each ramp step is mixed toward the accent, in percent. Step 0 is not
 * on the ramp: a country with no data is land, not a zero-visitor highlight.
 */
export const RAMP_MIX = [30, 47, 64, 82, 100] as const;

/** Land with no data. Both ends of the ramp are built from suite tokens. */
/**
 * Visitor count below which the ramp refuses to stretch. Under this, intensity
 * is measured against the floor rather than against the maximum, so one visit
 * cannot look like saturation.
 */
export const RAMP_FLOOR = 25;

export const NEUTRAL_FILL = "var(--obs-border)";

/** Normalises whatever the API handed us into an ISO alpha-2 shaped key. */
export function normalizeCode(raw: unknown): string {
  return String(raw ?? "").trim().toUpperCase();
}

/** The name the map knows a code by, or null when nothing claims it. */
export function lookupCountry(raw: unknown): { code: string; name: string; kind: "shape" | "marker" } | null {
  const code = normalizeCode(raw);
  const shape = SHAPE_BY_CODE.get(code);
  if (shape) return { code, name: shape[1], kind: "shape" };
  const marker = MARKER_BY_CODE.get(code);
  if (marker) return { code, name: marker[1], kind: "marker" };
  return null;
}

/**
 * Ramp bucket for a count, 0 (no data) through RAMP_MIX.length.
 *
 * Visitor counts are long-tailed — one home country and a scattering of
 * everything else — so a linear ramp leaves every country but one on the bottom
 * step. A cube root of the ratio lifts the tail onto the ramp while still
 * separating the leaders from each other, which log1p does not: at a 4,820-visitor
 * maximum, log1p puts a 1,210-visitor country on the same top step as the
 * leader. The maximum itself always lands on the last step exactly.
 */
export function stepFor(visitors: number, max: number): number {
  if (!Number.isFinite(visitors) || visitors <= 0) return 0;
  if (!Number.isFinite(max) || max <= 0) return 0;
  if (max < RAMP_FLOOR) {
    // Sparse data. A purely relative scale divides the leader by itself and
    // paints it at full intensity, so a site with a single visitor in one
    // country renders that country exactly as loudly as a site with fifty
    // thousand. Scale against the floor instead, linearly: small counts read
    // small, and the map stays honest until there is enough traffic to rank.
    const step = Math.ceil((Math.min(visitors, RAMP_FLOOR) / RAMP_FLOOR) * RAMP_MIX.length);
    return Math.min(RAMP_MIX.length, Math.max(1, step));
  }
  const ratio = Math.cbrt(Math.min(visitors, max) / max);
  const step = Math.ceil(ratio * RAMP_MIX.length);
  return Math.min(RAMP_MIX.length, Math.max(1, step));
}

/** Fill for a ramp bucket. Step 0 is neutral land. */
export function fillForStep(step: number): string {
  if (step <= 0) return NEUTRAL_FILL;
  const mix = RAMP_MIX[Math.min(RAMP_MIX.length, step) - 1];
  return `color-mix(in srgb, var(--obs-accent) ${mix}%, ${NEUTRAL_FILL})`;
}

/** Fill for a raw count. */
export function fillFor(visitors: number, max: number): string {
  return fillForStep(stepFor(visitors, max));
}

/**
 * Builds every cell the map draws. Countries absent from the data render as
 * neutral land, and codes absent from the geometry are collected rather than
 * silently discarded — Hong Kong and Macao, for instance, are real traffic
 * sources that Natural Earth has no admin-0 feature for at this resolution.
 */
export function buildChoropleth(data: readonly CountryDatum[] | null | undefined): Choropleth {
  const byCode = new Map<string, number>();
  for (const row of data ?? []) {
    const code = normalizeCode(row?.country);
    if (!code) continue;
    const visitors = Number(row?.visitors);
    if (!Number.isFinite(visitors) || visitors <= 0) continue;
    byCode.set(code, (byCode.get(code) ?? 0) + visitors);
  }

  let max = 0;
  for (const [code, visitors] of byCode) {
    if (lookupCountry(code) && visitors > max) max = visitors;
  }

  const shapes: ShapeCell[] = [];
  for (const [code, name, d] of WORLD_SHAPES) {
    const visitors = byCode.get(code) ?? 0;
    shapes.push({ code, name, d, visitors, step: stepFor(visitors, max) });
  }

  const markers: MarkerCell[] = [];
  for (const [code, name, x, y] of WORLD_MARKERS) {
    const visitors = byCode.get(code) ?? 0;
    markers.push({ code, name, x, y, visitors, step: stepFor(visitors, max) });
  }

  const unmatched: UnmatchedCode[] = [];
  let unmatchedVisitors = 0;
  for (const [code, visitors] of byCode) {
    if (lookupCountry(code)) continue;
    unmatched.push({ code, visitors });
    unmatchedVisitors += visitors;
  }
  unmatched.sort((a, b) => b.visitors - a.visitors || (a.code < b.code ? -1 : 1));

  // Drawing order: the busiest country last, so a small bright country is not
  // hidden under a neighbour's edge.
  shapes.sort((a, b) => a.visitors - b.visitors);

  return { shapes, markers, unmatched, unmatchedVisitors, max };
}
