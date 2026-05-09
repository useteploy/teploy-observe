import { test, expect, request as pwRequest } from "@playwright/test";
import { login } from "./helpers.js";

test.describe("traces — service map", () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  test("Map tab renders a service map (graph or empty state)", async ({ page }) => {
    const errors: string[] = [];
    page.on("pageerror", (err) => errors.push(err.message));
    page.on("console", (msg) => {
      if (msg.type() === "error" && !/401|Unauthorized/.test(msg.text())) {
        errors.push(msg.text());
      }
    });

    const resp = await page.goto("/traces", { waitUntil: "networkidle" });
    expect(resp?.status(), "GET /traces").toBe(200);

    // The four tabs should be visible.
    const mapTab = page.getByTestId("traces-tab-map");
    await expect(mapTab).toBeVisible();
    await expect(page.getByTestId("traces-tab-services")).toBeVisible();
    await expect(page.getByTestId("traces-tab-deps")).toBeVisible();
    await expect(page.getByTestId("traces-tab-search")).toBeVisible();

    await mapTab.click();

    // Wait for either the SVG container or the empty state.
    const svgLocator = page.locator("svg.service-map-svg");
    const emptyLocator = page.locator(".obs-empty-state");
    await expect(svgLocator.or(emptyLocator)).toBeVisible({ timeout: 10_000 });

    // If we got the graph, sanity-check that the legend rendered too.
    if (await svgLocator.isVisible()) {
      await expect(page.locator(".service-map-legend")).toBeVisible();
    }

    expect(errors, `console errors: ${errors.join(" | ")}`).toHaveLength(0);
  });

  // Regression for dogfood finding #11: rollup tables (service_stats,
  // service_dependencies) used to never get written. After the
  // ingest-time rollup fix the seed-loaded stack must surface at
  // least one service in /api/v1/traces/services and a dependency
  // edge from the seeded cross-service traces.
  test("seeded stack populates service_stats + service_dependencies", async ({
    page,
    baseURL,
  }) => {
    const ctx = await pwRequest.newContext({ baseURL, storageState: await page.context().storageState() });
    const to = new Date();
    const from = new Date(to.getTime() - 24 * 60 * 60 * 1000);
    const range = `site_id=default&from=${from.toISOString()}&to=${to.toISOString()}`;

    const services = await ctx.get(`/api/v1/traces/services?${range}`);
    expect(services.ok(), "GET /api/v1/traces/services").toBeTruthy();
    const servicesBody = await services.json();
    expect(Array.isArray(servicesBody)).toBeTruthy();
    expect(
      servicesBody.length,
      `seed should populate >=1 service, got ${JSON.stringify(servicesBody)}`,
    ).toBeGreaterThanOrEqual(1);

    const deps = await ctx.get(`/api/v1/traces/dependencies?${range}`);
    expect(deps.ok(), "GET /api/v1/traces/dependencies").toBeTruthy();
    const depsBody = await deps.json();
    expect(Array.isArray(depsBody)).toBeTruthy();
    // The seed has at least one cross-service trace (web -> api,
    // api -> worker), so at least one edge should exist.
    expect(
      depsBody.length,
      `seed should populate >=1 dependency edge, got ${JSON.stringify(depsBody)}`,
    ).toBeGreaterThanOrEqual(1);

    await ctx.dispose();
  });
});
