import { test, expect } from "@playwright/test";
import { login } from "./helpers.js";

/**
 * Honest minimal smoke (AUD-056 remainder): the app boots against a real
 * Nucleus, an admin can log in, the dashboard renders, and one seeded
 * session replay opens in the player. This is not the full e2e suite - it
 * is the narrow "the served product actually works" check that runs in CI
 * on every push. Skipped (not failed) when no instance answers at baseURL,
 * so `npm test` locally without a server is a clean skip, not a red run.
 */
let serverUp = false;

test.beforeAll(async ({ request }) => {
  try {
    const res = await request.get("/healthz", { timeout: 5000 });
    serverUp = res.ok();
  } catch {
    serverUp = false;
  }
  if (!serverUp) {
    test.skip(!serverUp, "no Observe instance at baseURL - see e2e/README.md to run the smoke");
  }
});

test("login lands on a rendered dashboard", async ({ page }) => {
  await login(page);
  await expect(page.locator("h1", { hasText: "Dashboard" })).toBeVisible({ timeout: 15000 });
});

test("one seeded replay opens and plays", async ({ page }) => {
  await login(page);
  // Explicit site: without it the sessions route falls back to "first
  // non-default site", which a concurrently-running spec (e.g. the
  // dashboard-replay journey) can create mid-run and steer this test onto
  // its rrweb session instead of the demo-seeded keyframe one.
  await page.goto("/sessions?site_id=default");
  const firstCard = page.locator(".sessions-card").first();
  await expect(firstCard).toBeVisible({ timeout: 15000 });
  await firstCard.click();
  await expect(page.locator(".sessions-detail-header")).toBeVisible();
  const play = page.locator(".sessions-play-btn");
  await expect(play).toBeEnabled();
  await play.click();
  await expect(page.locator(".replay-overlay")).toBeVisible();
  await expect(page.locator(".replay-iframe")).toBeAttached();
});
