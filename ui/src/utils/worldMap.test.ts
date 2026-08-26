import assert from "node:assert/strict";
import { test } from "node:test";
import {
  NEUTRAL_FILL,
  RAMP_MIX,
  buildChoropleth,
  fillFor,
  fillForStep,
  lookupCountry,
  normalizeCode,
  stepFor,
} from "./worldMap.ts";
import { WORLD_MARKERS, WORLD_SHAPES, WORLD_VIEWBOX } from "../data/world110m.ts";

test("the geometry covers the codes a dashboard actually sees", () => {
  // A spread across every continent, plus the awkward ones: France and Norway
  // carry ISO_A2 = -99 in Natural Earth and only resolve through ISO_A2_EH.
  const expected: Array<[string, string]> = [
    ["US", "United States of America"],
    ["CA", "Canada"],
    ["BR", "Brazil"],
    ["GB", "United Kingdom"],
    ["FR", "France"],
    ["NO", "Norway"],
    ["DE", "Germany"],
    ["RU", "Russia"],
    ["IN", "India"],
    ["CN", "China"],
    ["JP", "Japan"],
    ["AU", "Australia"],
    ["NZ", "New Zealand"],
    ["ZA", "South Africa"],
  ];

  for (const [code, name] of expected) {
    const found = lookupCountry(code);
    assert.ok(found, `${code} should resolve to a country`);
    assert.equal(found.code, code);
    assert.equal(found.name, name);
    assert.equal(found.kind, "shape");
  }
});

test("countries too small for a 110m polygon resolve to a point marker", () => {
  for (const [code, name] of [
    ["SG", "Singapore"],
    ["MT", "Malta"],
    ["BH", "Bahrain"],
    ["MV", "Maldives"],
  ]) {
    const found = lookupCountry(code);
    assert.ok(found, `${code} should resolve`);
    assert.equal(found.name, name);
    assert.equal(found.kind, "marker");
  }
});

test("codes are normalised before matching", () => {
  assert.equal(normalizeCode(" de "), "DE");
  assert.equal(normalizeCode(null), "");
  assert.equal(lookupCountry("de")?.code, "DE");
  assert.equal(lookupCountry(" fr ")?.name, "France");
});

test("every code in the geometry is unique and ISO alpha-2 shaped", () => {
  const seen = new Set<string>();
  for (const [code] of [...WORLD_SHAPES, ...WORLD_MARKERS]) {
    assert.match(code, /^[A-Z]{2}$/, `${code} is not alpha-2`);
    assert.ok(!seen.has(code), `${code} appears twice`);
    seen.add(code);
  }
  assert.ok(seen.size > 190, `expected near-global coverage, got ${seen.size}`);
});

test("the projection window is the equirectangular viewBox the paths were written for", () => {
  const [minX, minY, w, h] = WORLD_VIEWBOX.split(" ").map(Number);
  assert.equal(minX, -180);
  assert.equal(w, 360);
  assert.equal(minY, -84); // north edge, above the tip of Greenland
  assert.equal(minY + h, 59); // south edge, below Cape Horn
  for (const [, , x, y] of WORLD_MARKERS) {
    assert.ok(x >= minX && x <= minX + w, `marker x ${x} outside the viewBox`);
    assert.ok(y >= minY && y <= minY + h, `marker y ${y} outside the viewBox`);
  }
});

test("no data is neutral land and the maximum is the top of the ramp", () => {
  assert.equal(stepFor(0, 900), 0);
  assert.equal(fillFor(0, 900), NEUTRAL_FILL);
  assert.equal(fillForStep(0), NEUTRAL_FILL);

  assert.equal(stepFor(900, 900), RAMP_MIX.length);
  assert.equal(fillFor(900, 900), `color-mix(in srgb, var(--obs-accent) 100%, ${NEUTRAL_FILL})`);

  // A single visitor is on the ramp, not neutral, and not the top of it.
  assert.equal(stepFor(1, 900), 1);
  assert.notEqual(fillFor(1, 900), NEUTRAL_FILL);
  assert.equal(fillFor(1, 900), `color-mix(in srgb, var(--obs-accent) ${RAMP_MIX[0]}%, ${NEUTRAL_FILL})`);
});

test("the scale is monotonic and never leaves the ramp", () => {
  let previous = 0;
  for (const visitors of [0, 1, 2, 5, 12, 40, 120, 400, 900]) {
    const step = stepFor(visitors, 900);
    assert.ok(step >= previous, `step went backwards at ${visitors}`);
    assert.ok(step >= 0 && step <= RAMP_MIX.length, `step ${step} out of range`);
    previous = step;
  }
});

test("a long tail stays on the ramp without flattening the leaders", () => {
  // A realistic shape: one home country, a distant second, a long tail.
  const max = 4820;
  assert.equal(stepFor(4820, max), 5);
  assert.equal(stepFor(1210, max), 4); // must not read the same as the leader
  assert.equal(stepFor(300, max), 2);
  assert.equal(stepFor(2, max), 1); // and the tail must not read as no data
});

test("degenerate scales do not produce a fill", () => {
  assert.equal(stepFor(5, 0), 0);
  assert.equal(stepFor(Number.NaN, 100), 0);
  assert.equal(stepFor(5, Number.NaN), 0);
  assert.equal(stepFor(-3, 100), 0);
  // A single country with a single visitor is still the maximum.
  assert.equal(stepFor(1, 1), RAMP_MIX.length);
});

test("a country with no row renders as land, not as a zero highlight", () => {
  const map = buildChoropleth([{ country: "US", visitors: 500 }]);
  const pt = map.shapes.find((s) => s.code === "PT");
  assert.ok(pt);
  assert.equal(pt.visitors, 0);
  assert.equal(pt.step, 0);
  assert.equal(fillForStep(pt.step), NEUTRAL_FILL);

  const us = map.shapes.find((s) => s.code === "US");
  assert.equal(us?.step, RAMP_MIX.length);
});

test("unmatched codes are counted and surfaced, never dropped", () => {
  const map = buildChoropleth([
    { country: "US", visitors: 500 },
    { country: "HK", visitors: 90 },  // no admin-0 feature at 110m
    { country: "ZZ", visitors: 7 },   // not a country at all
    { country: "MO", visitors: 40 },
  ]);

  assert.deepEqual(map.unmatched, [
    { code: "HK", visitors: 90 },
    { code: "MO", visitors: 40 },
    { code: "ZZ", visitors: 7 },
  ]);
  assert.equal(map.unmatchedVisitors, 137);
  // An unmatched code must not set the scale — otherwise the whole map dims
  // for a value nothing can draw.
  assert.equal(map.max, 500);
  assert.equal(map.shapes.length, WORLD_SHAPES.length);
});

test("garbage in the breakdown cannot break the panel", () => {
  const map = buildChoropleth([
    { country: "US", visitors: 10 },
    { country: "", visitors: 5 },
    { country: "  ", visitors: 5 },
    { country: "DE", visitors: Number.NaN },
    { country: "FR", visitors: -4 },
    null as unknown as { country: string; visitors: number },
    undefined as unknown as { country: string; visitors: number },
  ]);

  assert.equal(map.max, 10);
  assert.equal(map.unmatched.length, 0);
  assert.equal(map.shapes.find((s) => s.code === "DE")?.visitors, 0);
  assert.equal(map.shapes.find((s) => s.code === "FR")?.visitors, 0);
  assert.deepEqual(buildChoropleth(null).unmatched, []);
  assert.equal(buildChoropleth(undefined).max, 0);
  assert.equal(buildChoropleth([]).shapes.length, WORLD_SHAPES.length);
});

test("repeated rows for one country are summed", () => {
  const map = buildChoropleth([
    { country: "gb", visitors: 30 },
    { country: "GB", visitors: 12 },
  ]);
  assert.equal(map.shapes.find((s) => s.code === "GB")?.visitors, 42);
  assert.equal(map.max, 42);
});

test("point-only countries take a fill from the same scale", () => {
  const map = buildChoropleth([
    { country: "US", visitors: 400 },
    { country: "SG", visitors: 400 },
  ]);
  const sg = map.markers.find((m) => m.code === "SG");
  assert.ok(sg);
  assert.equal(sg.visitors, 400);
  assert.equal(sg.step, RAMP_MIX.length);
  assert.equal(map.markers.find((m) => m.code === "MT")?.step, 0);
});

test("shapes are drawn least-busy first so a small bright country stays visible", () => {
  const map = buildChoropleth([
    { country: "US", visitors: 500 },
    { country: "LU", visitors: 20 },
  ]);
  const lu = map.shapes.findIndex((s) => s.code === "LU");
  const us = map.shapes.findIndex((s) => s.code === "US");
  assert.ok(lu < us, "the busiest country should be painted last");
});
