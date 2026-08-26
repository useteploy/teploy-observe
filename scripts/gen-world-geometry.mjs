#!/usr/bin/env node
/**
 * Regenerates ui/src/data/world110m.ts from Natural Earth 110m admin-0 data.
 *
 * Natural Earth is in the public domain (CC0-equivalent, no attribution
 * required); the provenance and licence text this writes into the generated
 * file's header is the record of that. Run it only when the geometry needs
 * refreshing — the generated file is committed, so nothing at build or run
 * time touches the network.
 *
 *   node scripts/gen-world-geometry.mjs
 *
 * Optional env:
 *   NE_CACHE_DIR   directory to read/write the downloaded source GeoJSON
 *   NE_TOLERANCE   Douglas-Peucker tolerance in degrees (default 0.12)
 */

import { writeFileSync, readFileSync, existsSync, mkdirSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { tmpdir } from "node:os";

const HERE = dirname(fileURLToPath(import.meta.url));
const OUT = join(HERE, "..", "ui", "src", "data", "world110m.ts");

// Pinned to a tagged Natural Earth release rather than master so a regeneration
// is reproducible.
const NE_REF = "v5.1.2";
const BASE = `https://raw.githubusercontent.com/nvkelso/natural-earth-vector/${NE_REF}/geojson`;
const COUNTRIES_URL = `${BASE}/ne_110m_admin_0_countries.geojson`;
const TINY_URL = `${BASE}/ne_110m_admin_0_tiny_countries.geojson`;

const TOLERANCE = Number(process.env.NE_TOLERANCE || 0.12); // degrees
const PRECISION = 2; // decimal places on lon/lat — 0.01 deg is ~1.1 km
// Rings whose bounding box is smaller than this in both axes are dropped,
// unless the ring is the only thing keeping its country on the map.
const MIN_RING_SPAN = 0.45; // degrees

// Latitude window. Antarctica is dropped outright (no analytics value, and it
// is the single largest contributor of vertices); the south edge sits below
// Cape Horn (-55.98) and Stewart Island (-47.3), the north edge above the tip
// of Greenland (83.6).
const LAT_TOP = 84;
const LAT_BOTTOM = -59;
const VIEWBOX = `-180 ${-LAT_TOP} 360 ${LAT_TOP - LAT_BOTTOM}`;

const SKIP_A3 = new Set(["ATA"]); // Antarctica

async function fetchCached(url, name) {
  const dir = process.env.NE_CACHE_DIR || join(tmpdir(), "ne-110m-cache");
  mkdirSync(dir, { recursive: true });
  const path = join(dir, name);
  if (existsSync(path)) return JSON.parse(readFileSync(path, "utf8"));
  process.stderr.write(`fetching ${url}\n`);
  const res = await fetch(url);
  if (!res.ok) throw new Error(`${url} -> HTTP ${res.status}`);
  const text = await res.text();
  writeFileSync(path, text);
  return JSON.parse(text);
}

/** Perpendicular-distance Douglas-Peucker, operating on [lon, lat] pairs. */
function simplify(points, tolerance) {
  if (points.length < 3) return points;
  const keep = new Uint8Array(points.length);
  keep[0] = 1;
  keep[points.length - 1] = 1;
  const stack = [[0, points.length - 1]];
  const tol2 = tolerance * tolerance;

  while (stack.length) {
    const [first, last] = stack.pop();
    if (last - first < 2) continue;
    const [ax, ay] = points[first];
    const [bx, by] = points[last];
    const dx = bx - ax;
    const dy = by - ay;
    const len2 = dx * dx + dy * dy;
    let bestIdx = -1;
    let bestDist = 0;
    for (let i = first + 1; i < last; i++) {
      const [px, py] = points[i];
      let d2;
      if (len2 === 0) {
        d2 = (px - ax) ** 2 + (py - ay) ** 2;
      } else {
        let t = ((px - ax) * dx + (py - ay) * dy) / len2;
        t = t < 0 ? 0 : t > 1 ? 1 : t;
        d2 = (px - (ax + t * dx)) ** 2 + (py - (ay + t * dy)) ** 2;
      }
      if (d2 > bestDist) {
        bestDist = d2;
        bestIdx = i;
      }
    }
    if (bestDist > tol2 && bestIdx > 0) {
      keep[bestIdx] = 1;
      stack.push([first, bestIdx], [bestIdx, last]);
    }
  }
  return points.filter((_, i) => keep[i] === 1);
}

const round = (n) => {
  const r = Number(n.toFixed(PRECISION));
  return Object.is(r, -0) ? 0 : r;
};

/** Clamp latitude into the rendered window so off-screen detail costs nothing. */
const clampLat = (lat) => (lat > LAT_TOP ? LAT_TOP : lat < LAT_BOTTOM ? LAT_BOTTOM : lat);

function ringToPath(ring) {
  const simplified = simplify(ring, TOLERANCE);
  const pts = [];
  for (const [lon, lat] of simplified) {
    const x = round(lon);
    const y = round(-clampLat(lat)); // SVG y grows downward; latitude grows up
    const prev = pts[pts.length - 1];
    if (prev && prev[0] === x && prev[1] === y) continue;
    pts.push([x, y]);
  }
  // Drop the GeoJSON closing point — `Z` re-closes it.
  if (pts.length > 1) {
    const a = pts[0];
    const b = pts[pts.length - 1];
    if (a[0] === b[0] && a[1] === b[1]) pts.pop();
  }
  if (pts.length < 3) return null;

  let span = 0;
  let minX = Infinity, maxX = -Infinity, minY = Infinity, maxY = -Infinity;
  for (const [x, y] of pts) {
    if (x < minX) minX = x;
    if (x > maxX) maxX = x;
    if (y < minY) minY = y;
    if (y > maxY) maxY = y;
  }
  span = Math.max(maxX - minX, maxY - minY);

  // "M x,y x,y …Z": every pair after the moveto is an implicit lineto, which
  // is the shortest legal encoding.
  const d = "M" + pts.map(([x, y]) => `${x},${y}`).join(" ") + "Z";
  return { d, span, area: (maxX - minX) * (maxY - minY) };
}

function ringsOf(geometry) {
  if (geometry.type === "Polygon") return geometry.coordinates;
  if (geometry.type === "MultiPolygon") return geometry.coordinates.flat();
  return [];
}

function isoA2(p) {
  for (const key of ["ISO_A2_EH", "ISO_A2"]) {
    const v = p[key];
    if (typeof v === "string" && /^[A-Z]{2}$/.test(v)) return v;
  }
  return null;
}

/**
 * Natural Earth's `NAME` is sized for a map label, so it abbreviates
 * ("Dem. Rep. Congo", "Bosnia and Herz."). `NAME_LONG` spells those out but is
 * officious where `NAME` is already clean ("Russian Federation", "Lao PDR").
 * Take the long form only where the short one is abbreviated.
 */
const displayName = (p) => {
  const short = p.NAME || p.NAME_LONG || p.ADM0_A3;
  return short.includes(".") ? p.NAME_LONG || short : short;
};

async function main() {
  const countries = await fetchCached(COUNTRIES_URL, "ne_110m_admin_0_countries.geojson");
  const tiny = await fetchCached(TINY_URL, "ne_110m_admin_0_tiny_countries.geojson");

  const shapes = [];   // [code, name, d] — ISO-coded landmasses
  const uncoded = [];  // [name, d]       — land with no ISO alpha-2 (N. Cyprus, Somaliland)
  const seen = new Map();

  for (const f of countries.features) {
    const p = f.properties;
    if (SKIP_A3.has(p.ADM0_A3)) continue;

    const built = [];
    for (const ring of ringsOf(f.geometry)) {
      const r = ringToPath(ring);
      if (r) built.push(r);
    }
    if (!built.length) continue;

    built.sort((a, b) => b.area - a.area);
    const kept = built.filter((r, i) => i === 0 || r.span >= MIN_RING_SPAN);
    const d = kept.map((r) => r.d).join("");

    const code = isoA2(p);
    const name = displayName(p);
    if (!code) {
      uncoded.push([name, d]);
      continue;
    }
    if (seen.has(code)) {
      // Natural Earth 110m has one feature per ISO alpha-2; merge defensively
      // rather than let a future data revision silently drop a landmass.
      const idx = seen.get(code);
      shapes[idx][2] += d;
      continue;
    }
    seen.set(code, shapes.length);
    shapes.push([code, name, d]);
  }

  // Countries too small to have any polygon at 110m ship as points in Natural
  // Earth's companion "tiny countries" layer. Only the ones that have no
  // landmass of their own are worth a marker; the rest (Azores for PT, the
  // Canaries for ES, Jan Mayen for NO) are already drawn.
  const markers = [];
  const markerSeen = new Set();
  for (const f of tiny.features) {
    const p = f.properties;
    const code = isoA2(p);
    if (!code || seen.has(code) || markerSeen.has(code)) continue;
    const [lon, lat] = f.geometry.coordinates;
    if (lat > LAT_TOP || lat < LAT_BOTTOM) continue;
    markerSeen.add(code);
    markers.push([code, displayName(p), round(lon), round(-lat)]);
  }

  shapes.sort((a, b) => (a[0] < b[0] ? -1 : 1));
  markers.sort((a, b) => (a[0] < b[0] ? -1 : 1));
  uncoded.sort((a, b) => (a[0] < b[0] ? -1 : 1));

  const esc = (s) => JSON.stringify(s);
  const lines = [];
  lines.push(`// GENERATED FILE — do not edit by hand.`);
  lines.push(`// Regenerate with: node scripts/gen-world-geometry.mjs`);
  lines.push(`//`);
  lines.push(`// Source:  Natural Earth, 1:110m Cultural Vectors, Admin 0 - Countries`);
  lines.push(`//          and Admin 0 - Tiny Country Points, via the nvkelso/`);
  lines.push(`//          natural-earth-vector mirror at tag ${NE_REF}.`);
  lines.push(`//          ${COUNTRIES_URL}`);
  lines.push(`//          ${TINY_URL}`);
  lines.push(`// Licence: public domain. Natural Earth's terms of use: "All versions of`);
  lines.push(`//          Natural Earth raster and vector map data found on this website are`);
  lines.push(`//          in the public domain. You may use the maps in any manner, including`);
  lines.push(`//          modifying the content and design, electronic dissemination, and`);
  lines.push(`//          offset printing. The primary authors, Tom Patterson and Nathaniel`);
  lines.push(`//          Vaughn Kelso, and all other contributors renounce all financial`);
  lines.push(`//          claim to the underlying data and iterations of the maps."`);
  lines.push(`//          No attribution is required; see ui/src/data/README.md.`);
  lines.push(`//`);
  lines.push(`// Processing: Antarctica dropped, latitude clipped to [${LAT_BOTTOM}, ${LAT_TOP}],`);
  lines.push(`//          Douglas-Peucker simplified at ${TOLERANCE} degrees, coordinates rounded`);
  lines.push(`//          to ${PRECISION} decimal places, rings under ${MIN_RING_SPAN} degrees dropped unless`);
  lines.push(`//          they are a country's only remaining land.`);
  lines.push(`//`);
  lines.push(`// Path coordinates are degrees, as "longitude,-latitude", so the SVG viewBox`);
  lines.push(`// below IS the equirectangular (plate carree) projection — no runtime maths.`);
  lines.push(``);
  lines.push(`export const WORLD_VIEWBOX = ${esc(VIEWBOX)};`);
  lines.push(``);
  lines.push(`/** ISO-3166-1 alpha-2 code, English name, SVG path data. */`);
  lines.push(`export type WorldShape = readonly [code: string, name: string, d: string];`);
  lines.push(``);
  lines.push(`/** Point-only countries: code, name, x (lon), y (-lat). */`);
  lines.push(`export type WorldMarker = readonly [code: string, name: string, x: number, y: number];`);
  lines.push(``);
  lines.push(`export const WORLD_SHAPES: readonly WorldShape[] = [`);
  for (const [code, name, d] of shapes) lines.push(`  [${esc(code)}, ${esc(name)}, ${esc(d)}],`);
  lines.push(`];`);
  lines.push(``);
  lines.push(`export const WORLD_MARKERS: readonly WorldMarker[] = [`);
  for (const [code, name, x, y] of markers) lines.push(`  [${esc(code)}, ${esc(name)}, ${x}, ${y}],`);
  lines.push(`];`);
  lines.push(``);
  lines.push(`/**`);
  lines.push(` * Landmasses Natural Earth carries with no ISO alpha-2 of their own. They are`);
  lines.push(` * drawn as neutral land so the map has no holes, and can never take a fill.`);
  lines.push(` */`);
  lines.push(`export const WORLD_UNCODED: readonly (readonly [name: string, d: string])[] = [`);
  for (const [name, d] of uncoded) lines.push(`  [${esc(name)}, ${esc(d)}],`);
  lines.push(`];`);
  lines.push(``);

  const out = lines.join("\n");
  mkdirSync(dirname(OUT), { recursive: true });
  writeFileSync(OUT, out);

  const vertices = shapes.reduce((n, s) => n + (s[2].match(/,/g) || []).length, 0);
  process.stderr.write(
    `wrote ${OUT}\n` +
      `  ${shapes.length} coded shapes, ${markers.length} markers, ${uncoded.length} uncoded\n` +
      `  ${vertices} vertices, ${(Buffer.byteLength(out) / 1024).toFixed(1)} KB\n`,
  );
}

main().catch((err) => {
  process.stderr.write(String(err && err.stack ? err.stack : err) + "\n");
  process.exit(1);
});
