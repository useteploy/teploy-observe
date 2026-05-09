import { test, expect } from "@playwright/test";
import { login } from "./helpers.js";

// Drives detail views by clicking into the first row of each list page,
// covering the routes that crash most often (data-bound, non-trivial state).

test.describe("detail routes render after drill-in", () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  // /traces lands on the services view, not a flat trace list — click a
  // service card to drill into operations. /sessions is a flat card list.
  const cases = [
    { list: "/traces", rowSelector: ".traces-service-card" },
    { list: "/sessions", rowSelector: ".sessions-card" },
  ];

  for (const c of cases) {
    test(`drill into first row of ${c.list}`, async ({ page }) => {
      const errors: string[] = [];
      page.on("pageerror", (err) => errors.push(err.message));
      page.on("console", (msg) => {
        if (msg.type() === "error" && !/401|Unauthorized|Failed to load resource/.test(msg.text())) {
          errors.push(msg.text());
        }
      });

      await page.goto(c.list, { waitUntil: "networkidle" });
      const first = page.locator(c.rowSelector).first();
      const count = await page.locator(c.rowSelector).count();
      test.skip(count === 0, `no rows present at ${c.list} — seed not run?`);

      const urlBefore = page.url();
      await first.click();
      // Detail view may be: a URL change, a modal/aside, or in-place state swap
      // (the route's component re-renders to a detail layout with a back button).
      await page.waitForTimeout(500);
      const urlAfter = page.url();
      const drilled =
        urlAfter !== urlBefore ||
        (await page.locator('[role="dialog"], aside, .errors-back-btn, .traces-back-btn, [class*="-back-btn"]').count()) > 0;
      expect(drilled, `clicking row should reveal detail at ${c.list}`).toBeTruthy();

      expect(errors, `console errors on ${c.list} detail: ${errors.join(" | ")}`).toHaveLength(0);
    });
  }
});
