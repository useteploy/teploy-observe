import { test, expect } from "@playwright/test";
import { login } from "./helpers.js";

// Click heatmap overlay (PostHog gap #3 / W4.2):
// 1. /api/v1/heatmaps must respond with a JSON array (200, possibly empty).
// 2. The Heatmap toggle inside ReplayPlayer must render an overlay canvas
//    when activated.

test.describe("click heatmaps", () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  test("GET /api/v1/heatmaps returns a JSON array (no 500)", async ({ page }) => {
    // Observe stores the JWT in localStorage and reads it via the helpers
    // wrapper (ui/src/api/helpers.ts). Run the fetch in-page so it picks
    // up the same `Authorization: Bearer <token>` header the UI uses.
    const url = "https://demo.local/";
    const from = "2025-01-01T00:00:00Z";
    const to = "2030-01-01T00:00:00Z";
    const result = await page.evaluate(async ({ url, from, to }) => {
      const token = localStorage.getItem("obs_token");
      const r = await fetch(
        `/api/v1/heatmaps?site_id=default&url=${encodeURIComponent(url)}&from=${from}&to=${to}`,
        { headers: token ? { Authorization: `Bearer ${token}` } : {} },
      );
      const text = await r.text();
      let body: unknown = null;
      try { body = JSON.parse(text); } catch { /* leave null */ }
      return { status: r.status, text, body };
    }, { url, from, to });

    expect(result.status, `status body: ${result.text}`).toBe(200);
    expect(Array.isArray(result.body)).toBeTruthy();
    // Each click row must look like { x, y, count } when present.
    for (const c of result.body as Array<{ x: number; y: number; count: number }>) {
      expect(typeof c.x).toBe("number");
      expect(typeof c.y).toBe("number");
      expect(typeof c.count).toBe("number");
    }
  });

  test("Heatmap toggle inside ReplayPlayer renders an overlay", async ({ page }) => {
    const errors: string[] = [];
    page.on("pageerror", (err) => errors.push(err.message));

    await page.goto("/sessions", { waitUntil: "networkidle" });
    const cards = page.locator(".sessions-card");
    const cardCount = await cards.count();
    test.skip(cardCount === 0, "no seeded sessions — heatmap UI test cannot drill in");

    await cards.first().click();

    // SessionDetail renders a "Play replay" button; click it to mount
    // ReplayPlayer (the modal that owns the heatmap toggle). The button
    // is disabled while events are still loading, so wait for it to
    // become enabled before clicking.
    const playBtn = page.locator(".sessions-play-btn");
    await expect(playBtn).toBeVisible();
    await expect(playBtn).toBeEnabled({ timeout: 10_000 });
    await playBtn.click();

    const toggle = page.getByTestId("heatmap-toggle");
    await expect(toggle).toBeVisible();

    // Canvas should NOT exist before the toggle fires.
    await expect(page.getByTestId("heatmap-overlay-canvas")).toHaveCount(0);

    await toggle.click();

    // After the toggle the overlay canvas should exist and be sized.
    const canvas = page.getByTestId("heatmap-overlay-canvas");
    await expect(canvas).toHaveCount(1);
    await expect(canvas).toBeVisible();

    expect(errors, `pageerrors during heatmap render: ${errors.join(" | ")}`).toHaveLength(0);
  });
});
