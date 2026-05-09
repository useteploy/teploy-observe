import { test, expect } from "@playwright/test";
import { login } from "./helpers.js";

// Hard reload tests catch hydration / SSR-mismatch / first-render bugs that
// don't appear when navigating client-side. A fresh navigation after login
// is the user's most common way of re-entering a page.

const ROUTES = [
  "/",
  "/errors",
  "/sessions",
  "/logs",
  "/traces",
  "/dashboards",
  "/insights",
  "/explorer",
];

test.describe("hard reload survives without console errors", () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  for (const route of ROUTES) {
    test(`hard reload ${route}`, async ({ page }) => {
      const errors: string[] = [];
      page.on("pageerror", (err) => errors.push(err.message));
      page.on("console", (msg) => {
        if (msg.type() === "error" && !/401|Unauthorized|Failed to load resource/.test(msg.text())) {
          errors.push(msg.text());
        }
      });
      await page.goto(route, { waitUntil: "networkidle" });
      await page.reload({ waitUntil: "networkidle" });
      await page.waitForTimeout(250);
      expect(errors, `errors after reload of ${route}: ${errors.join(" | ")}`).toHaveLength(0);
      // Sanity: the body still has visible content (not a white screen).
      const text = (await page.locator("body").innerText()).trim();
      expect(text.length, `body empty after reload of ${route}`).toBeGreaterThan(0);
    });
  }
});
