import { test, expect } from "@playwright/test";
import { login } from "./helpers.js";

test.describe("every route renders without a console error", () => {
  const ROUTES = [
    "/",
    "/errors",
    "/sessions",
    "/logs",
    "/traces",
    "/alerts",
    "/flags",
    "/experiments",
    "/surveys",
    "/llm",
    "/monitoring",
    "/dashboards",
    "/insights",
    "/events",
    "/campaigns",
    "/settings",
    "/explorer",
    "/integrations",
    "/reports",
    "/docs",
    "/releases",
    "/onboard",
  ];

  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  for (const route of ROUTES) {
    test(`GET ${route} has no console errors`, async ({ page }) => {
      const errors: string[] = [];
      page.on("pageerror", (err) => errors.push(err.message));
      page.on("console", (msg) => {
        // ignore 401 / network chatter surfaced via console
        if (msg.type() === "error" && !/401|Unauthorized/.test(msg.text())) {
          errors.push(msg.text());
        }
      });
      const resp = await page.goto(route, { waitUntil: "networkidle" });
      expect(resp?.status(), `HTTP status for ${route}`).toBe(200);
      // Let any deferred UI render so lazy errors surface.
      await page.waitForTimeout(250);
      expect(errors, `console errors on ${route}: ${errors.join(" | ")}`).toHaveLength(0);
    });
  }
});
