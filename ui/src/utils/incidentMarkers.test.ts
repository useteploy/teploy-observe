import assert from "node:assert/strict";
import { test } from "node:test";
import { prepareMarkers, markerSummary } from "./incidentMarkers.ts";
import type { IncidentMarker } from "./incidentMarkers.ts";

const HOUR = 3_600_000;
const MIN_T = 0;
const MAX_T = 24 * HOUR;

function opts(over: Partial<Parameters<typeof prepareMarkers>[1]> = {}) {
  return { minT: MIN_T, maxT: MAX_T, mergeGapMs: 0, maxBands: 30, ...over };
}

function marker(i: number, start: number, end: number, severity = "warning"): IncidentMarker {
  return { id: `i${i}`, title: `incident ${i}`, severity, started_at: start, ended_at: end };
}

/**
 * The bug this file exists for: 6,206 incident markers in one window, each
 * drawn as a translucent band plus a stroked leading edge, which composes into
 * a solid block of colour with the series invisible underneath.
 *
 * Remove the cap in prepareMarkers and this fails with "drew 6206 bands".
 */
test("thousands of markers never become thousands of bands", () => {
  const flood: IncidentMarker[] = [];
  for (let i = 0; i < 6206; i++) {
    const start = MIN_T + Math.floor((i / 6206) * (MAX_T - MIN_T));
    flood.push(marker(i, start, start + 60_000));
  }
  const prepared = prepareMarkers(flood, opts());
  assert.ok(
    prepared.bands.length <= 30,
    `drew ${prepared.bands.length} bands, want at most 30`,
  );
  assert.equal(prepared.total, 6206);
  // Every incident is accounted for: drawn inside a band, or counted as hidden.
  const shown = prepared.bands.reduce((n, b) => n + b.count, 0);
  assert.equal(shown + prepared.hidden, prepared.total);
});

test("overlapping markers of one severity merge into a single band", () => {
  const prepared = prepareMarkers([
    marker(1, 1 * HOUR, 3 * HOUR),
    marker(2, 2 * HOUR, 4 * HOUR),
    marker(3, 3.5 * HOUR, 5 * HOUR),
  ], opts());
  assert.equal(prepared.bands.length, 1);
  assert.deepEqual(
    { start: prepared.bands[0].start, end: prepared.bands[0].end, count: prepared.bands[0].count },
    { start: 1 * HOUR, end: 5 * HOUR, count: 3 },
  );
  assert.equal(prepared.bands[0].label, "3 incidents");
  assert.equal(prepared.hidden, 0);
});

test("markers closer together than the merge gap are merged", () => {
  const near = [marker(1, 1 * HOUR, 2 * HOUR), marker(2, 2 * HOUR + 60_000, 3 * HOUR)];
  assert.equal(prepareMarkers(near, opts({ mergeGapMs: 0 })).bands.length, 2);
  assert.equal(prepareMarkers(near, opts({ mergeGapMs: 5 * 60_000 })).bands.length, 1);
});

test("severities stay separate so a merged band keeps a meaningful colour", () => {
  const prepared = prepareMarkers([
    marker(1, 1 * HOUR, 4 * HOUR, "warning"),
    marker(2, 2 * HOUR, 3 * HOUR, "critical"),
  ], opts());
  assert.equal(prepared.bands.length, 2);
  // Least urgent first, so critical paints over warning rather than under it.
  assert.deepEqual(prepared.bands.map((b) => b.severity), ["warning", "critical"]);
});

test("an ongoing marker runs to the right edge and no further", () => {
  const [band] = prepareMarkers([marker(1, 2 * HOUR, 0)], opts()).bands;
  assert.equal(band.end, MAX_T);
  assert.equal(band.ongoing, true);
});

test("markers are clamped to the window and ones outside it are dropped", () => {
  const prepared = prepareMarkers([
    marker(1, -50 * HOUR, -40 * HOUR),   // entirely before
    marker(2, 100 * HOUR, 200 * HOUR),   // entirely after
    marker(3, -5 * HOUR, 5 * HOUR),      // straddles the left edge
  ], opts());
  assert.equal(prepared.total, 1);
  assert.deepEqual(
    { start: prepared.bands[0].start, end: prepared.bands[0].end },
    { start: MIN_T, end: 5 * HOUR },
  );
});

test("a degenerate window yields nothing rather than dividing by zero", () => {
  for (const bad of [{ minT: 5, maxT: 5 }, { minT: 10, maxT: 1 }, { minT: NaN, maxT: 10 }]) {
    const prepared = prepareMarkers([marker(1, 5, 5)], opts(bad));
    assert.deepEqual(prepared, { bands: [], hidden: 0, total: 0 });
  }
});

test("markers carrying junk do not take the chart down", () => {
  const junk = [
    { id: "a", title: "a", severity: "warning", started_at: NaN, ended_at: 1 },
    { id: "b", title: "b", severity: "warning", started_at: 1 * HOUR, ended_at: NaN },
  ] as IncidentMarker[];
  const prepared = prepareMarkers(junk, opts());
  // The NaN start is unusable and dropped; the NaN end reads as still open.
  assert.equal(prepared.total, 1);
  assert.equal(prepared.bands[0].end, MAX_T);
});

test("the summary says what was merged and what was not drawn", () => {
  assert.equal(markerSummary({ bands: [], hidden: 0, total: 0 }), "");
  assert.equal(
    markerSummary(prepareMarkers([marker(1, 1 * HOUR, 2 * HOUR)], opts())),
    "1 incident",
  );

  const merged = prepareMarkers([
    marker(1, 1 * HOUR, 3 * HOUR),
    marker(2, 2 * HOUR, 4 * HOUR),
  ], opts());
  assert.equal(markerSummary(merged), "2 incidents in 1 band");

  const flood: IncidentMarker[] = [];
  for (let i = 0; i < 200; i++) {
    const start = MIN_T + i * 2 * 60_000;
    flood.push(marker(i, start, start + 60_000));
  }
  const capped = prepareMarkers(flood, opts({ maxBands: 5 }));
  assert.equal(capped.bands.length, 5);
  assert.match(markerSummary(capped), /^200 incidents, 5 shown, \d+ not drawn$/);
});
