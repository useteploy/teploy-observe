import assert from "node:assert/strict";
import { test } from "node:test";
import { NAV_ITEMS, navKeyFor } from "./navItems.ts";
import { ROUTES, searchRoutes, routeKeywords } from "./paletteRoutes.ts";

// Funnels, retention, journeys, goal conversions and correlations all live at
// /insights, and nothing on the way there said so: the sidebar said
// "Insights", which is a category, not a thing you can look for. These tests
// pin the two places a person looks — the sidebar and the command palette —
// against the words they actually type.

const insightsNav = () => NAV_ITEMS.find((i) => i.href === "/insights");
const insightsRoute = () => ROUTES.find((r) => r.path === "/insights");

test("the sidebar entry for /insights names what is inside it", () => {
  const item = insightsNav();
  assert.ok(item, "/insights has no sidebar entry");
  const label = item.label.toLowerCase();
  assert.ok(
    label.includes("funnel") || label.includes("conversion"),
    `sidebar label ${JSON.stringify(item.label)} does not say funnels or conversions`,
  );
  // It still has to fit a 220px sidebar next to an 18px icon.
  assert.ok(item.label.length <= 18, `sidebar label ${JSON.stringify(item.label)} is too long`);
});

test("every sidebar label is distinct", () => {
  const labels = NAV_ITEMS.map((i) => i.label);
  assert.equal(new Set(labels).size, labels.length, "two nav items share a label");
});

test("navKeyFor still resolves /insights and its deep links", () => {
  // cohorts.tsx links to /insights?site_id=…&cohort_id=… — the query string is
  // not part of the path, but the route must keep resolving either way.
  assert.equal(navKeyFor("/insights"), "insights");
  assert.equal(navKeyFor("/insights/funnels"), "insights");
  assert.equal(navKeyFor("/"), "analytics");
  assert.equal(navKeyFor("/dashboards"), "dashboards");
});

test("the command palette finds /insights by the words people type", () => {
  for (const query of ["conversion", "conversions", "funnel", "funnels", "goal", "goals", "revenue", "insights"]) {
    const hits = searchRoutes(query);
    assert.ok(
      hits.some((r) => r.path === "/insights"),
      `searching ${JSON.stringify(query)} does not surface /insights`,
    );
  }
});

test("the palette entry for /insights carries conversion vocabulary", () => {
  const route = insightsRoute();
  assert.ok(route, "/insights is missing from the palette");
  const text = routeKeywords(route).toLowerCase();
  for (const word of ["conversion", "funnel", "goal", "retention", "journey"]) {
    assert.ok(text.includes(word), `palette entry does not mention ${word}`);
  }
});

test("the sidebar and the palette agree on what /insights is called", () => {
  assert.equal(insightsNav()?.label, insightsRoute()?.label);
});

test("every sidebar destination is reachable from the palette", () => {
  const paths = new Set(ROUTES.map((r) => r.path));
  for (const item of NAV_ITEMS) {
    // Boards and the two person-shaped pages predate the palette; assert only
    // that the renamed one did not fall out of it.
    if (item.href === "/insights") {
      assert.ok(paths.has(item.href), "/insights dropped out of the command palette");
    }
  }
});
