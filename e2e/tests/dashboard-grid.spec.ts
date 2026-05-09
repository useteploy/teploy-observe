import { test, expect } from "@playwright/test";
import { login } from "./helpers.js";

// T017 — verify the dashboard grid: drag/resize affordances are present
// on each panel and width changes persist across reload.

test.describe("dashboard grid drag/resize", () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  test("resize handles + width buttons render on each panel", async ({ page }) => {
    await page.goto("/dashboards", { waitUntil: "domcontentloaded" });
    await page.waitForSelector(".dashboard-card", { timeout: 5_000 });
    await page.locator(".dashboard-card").first().click();
    await page.waitForSelector(".dashboard-panel", { timeout: 5_000 });

    const panel = page.locator(".dashboard-panel").first();
    await panel.hover();

    await expect(panel.locator(".dashboard-panel-resize")).toHaveCount(1);
    await expect(panel.locator(".dashboard-panel-ctrl")).toHaveCount(4);

    const title = panel.locator(".dashboard-panel-title").first();
    await expect(title).toHaveCSS("cursor", "grab");

    expect(await panel.getAttribute("draggable")).toBe("true");
  });

  test("width button updates panel and persists across reload", async ({ page }) => {
    await page.goto("/dashboards", { waitUntil: "domcontentloaded" });
    await page.waitForSelector(".dashboard-card", { timeout: 5_000 });
    await page.locator(".dashboard-card").first().click();
    await page.waitForSelector(".dashboard-panel", { timeout: 5_000 });

    // Target a specific panel by its title so reorder doesn't move what we
    // assert against (panels with the same position_y can swap).
    const panelCount = await page.locator(".dashboard-panel").count();
    test.skip(panelCount === 0, "no panels in seeded dashboard");
    // .dashboard-panel-title has text-transform: uppercase, so innerText returns
    // the rendered casing — use textContent for the underlying DOM string.
    const titleText = await page.locator(".dashboard-panel-title").first().evaluate((el) => el.textContent || "");
    const panel = page.locator(".dashboard-panel").filter({ has: page.locator(`.dashboard-panel-title:text-is("${titleText}")`) });
    await panel.first().hover();
    await panel.first().locator('.dashboard-panel-ctrl[aria-label="Set width 12/12"]').click();
    await expect(panel.first()).toHaveAttribute("style", /span 12/, { timeout: 3_000 });

    // Detail view is state-based, not URL-based — reload returns to the list,
    // so we click back in to verify persistence.
    await page.reload({ waitUntil: "domcontentloaded" });
    await page.waitForSelector(".dashboard-card", { timeout: 5_000 });
    await page.locator(".dashboard-card").first().click();
    await page.waitForSelector(".dashboard-panel", { timeout: 5_000 });
    const reloaded = page.locator(".dashboard-panel").filter({ has: page.locator(`.dashboard-panel-title:text-is("${titleText}")`) });
    expect(await reloaded.first().getAttribute("style")).toMatch(/span 12/);
    // Reset width back to 6 so subsequent runs start clean.
    await reloaded.first().hover();
    await reloaded.first().locator('.dashboard-panel-ctrl[aria-label="Set width 6/12"]').click();
  });
});
