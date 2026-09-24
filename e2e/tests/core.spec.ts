import { test, expect, type Page } from "@playwright/test";
import { login } from "./helpers.js";

/**
 * Core product paths (AUD-056 remainder, fuller step): the flows a release
 * actually breaks — login lands on a rendered dashboard, a replay opens and
 * the transport controls work (play/pause toggle, scrub seeks), and the
 * audit view renders the immutable trail. This is still NOT the full
 * 27-spec local suite (see e2e/README.md); it is the honest middle: the
 * core surface runs in CI on every push, skipped cleanly when no instance
 * answers at baseURL.
 *
 * One login per worker: the server rate-limits login attempts per IP
 * (10/min — brute-force guard), and five spec files logging in within
 * seconds of each other trip it. The token is minted once here and
 * injected into each test's localStorage; the form-login flow itself stays
 * covered by smoke.spec.ts and auth.spec.ts.
 */
let serverUp = false;
let authToken = "";

test.beforeAll(async ({ request }) => {
  try {
    const res = await request.get("/healthz", { timeout: 5000 });
    serverUp = res.ok();
  } catch {
    serverUp = false;
  }
  if (!serverUp) {
    test.skip(!serverUp, "no Observe instance at baseURL - see e2e/README.md to run the core suite");
    return;
  }
  const user = process.env.OBSERVE_ADMIN_USER ?? "admin";
  const pass = process.env.OBSERVE_ADMIN_PASSWORD ?? "observe-e2e-pass";
  const res = await request.post("/api/v1/auth/login", { data: { username: user, password: pass } });
  if (res.ok()) {
    const body = await res.json();
    authToken = body.token ?? "";
  }
});

/** Open a path already authenticated with the suite's shared token. */
async function open(page: Page, path: string): Promise<void> {
  if (authToken) {
    await page.addInitScript((token) => localStorage.setItem("obs_token", token), authToken);
  }
  await page.goto(path);
}

test("login lands on a rendered dashboard with stats cards", async ({ page }) => {
  await login(page);
  await expect(page.locator("h1", { hasText: "Dashboard" })).toBeVisible({ timeout: 15000 });
  // The dashboard's stat cards render against the live API, not just the
  // route shell.
  await expect(page.locator(".stats-card, [class*='stat']").first()).toBeVisible({ timeout: 15000 });
});

test("replay opens, pauses, and seeks", async ({ page }) => {
  // Explicit site: without it the sessions route falls back to "first
  // non-default site", which a concurrently-running spec can create mid-run
  // and steer this test onto its rrweb session instead of the demo-seeded
  // keyframe one.
  await open(page, "/sessions?site_id=default");
  const firstCard = page.locator(".sessions-card").first();
  await expect(firstCard).toBeVisible({ timeout: 15000 });
  await firstCard.click();
  await expect(page.locator(".sessions-detail-header")).toBeVisible();
  const play = page.locator(".sessions-play-btn");
  await expect(play).toBeEnabled();
  await play.click();
  await expect(page.locator(".replay-overlay")).toBeVisible();

  // Playing: the transport button offers Pause.
  const toggle = page.locator(".replay-play");
  await expect(toggle).toHaveText(/Pause/, { timeout: 10000 });

  // Pause: the label flips back.
  await toggle.click();
  await expect(toggle).toHaveText(/Play/);

  // Seek: scrubbing the range input moves the playhead readout.
  const scrub = page.locator(".replay-scrub");
  await expect(scrub).toBeVisible();
  const timeBefore = await page.locator(".replay-time").innerText();
  const max = parseInt(await scrub.getAttribute("max") ?? "1000", 10);
  // The input steps by 100 — seek to a step-aligned midpoint.
  const target = Math.max(100, Math.floor(max / 2 / 100) * 100);
  await scrub.fill(String(target));
  const timeAfter = await page.locator(".replay-time").innerText();
  expect(timeAfter).not.toBe(timeBefore);
});

test("audit view renders the trail", async ({ page }) => {
  await open(page, "/audit");
  await expect(page.locator("h1", { hasText: "Audit log" })).toBeVisible({ timeout: 15000 });
  // The login this suite performed is itself audited (auth.login), so a
  // working instance always has at least one matching row — but parallel
  // spec files also write audit actions (site.create, ...), which can sort
  // ahead of it, so the match is order-independent.
  const row = page.locator("table tbody tr").filter({ hasText: /auth\.login|user\.|audit\./ }).first();
  await expect(row).toBeVisible({ timeout: 15000 });
});
