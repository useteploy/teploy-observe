import { test, expect } from "@playwright/test";
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
});
