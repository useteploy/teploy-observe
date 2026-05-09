import { test, expect } from "@playwright/test";
import { login } from "./helpers.js";

// When the backend errors, the UI must degrade — never white-screen.
// We force /api/v1/* to 500 and assert the route still mounts a chrome
// (sidebar / header) so the user can navigate elsewhere.

test.describe("UI degrades gracefully when API fails", () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  const ROUTES = ["/errors", "/logs", "/traces", "/dashboards"];

  for (const route of ROUTES) {
    test(`${route} mounts chrome on 500`, async ({ page, context }) => {
      // Intercept all data API calls AFTER login completes.
      await context.route("**/api/v1/**", (route) => {
        // Allow auth/me and sites lookup so the layout can resolve user/site state;
        // fail everything else so list/data fetches die.
        const url = route.request().url();
        if (/\/api\/v1\/(me|sites|auth)\b/.test(url)) {
          return route.fallback();
        }
        return route.fulfill({
          status: 500,
          contentType: "application/json",
          body: JSON.stringify({ error: "synthetic failure for e2e" }),
        });
      });

      const errors: string[] = [];
      page.on("pageerror", (err) => errors.push(err.message));

      await page.goto(route, { waitUntil: "networkidle" });
      await page.waitForTimeout(500);

      // Page-level JS errors are unacceptable even when fetches fail.
      expect(errors, `pageerror on ${route} during 500: ${errors.join(" | ")}`).toHaveLength(0);

      // Chrome must still be present — sidebar nav links visible.
      const navCount = await page.locator('a[href="/errors"], a[href="/logs"], nav a').count();
      expect(navCount, `nav chrome missing after 500 on ${route}`).toBeGreaterThan(0);
    });
  }
});
