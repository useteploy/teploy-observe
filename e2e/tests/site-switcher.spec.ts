import { test, expect, request } from "@playwright/test";
import { login } from "./helpers.js";

// Site/project switcher: header dropdown that swaps the active siteId for
// every route consuming `useFilters().state.siteId`. Persists via URL
// (`?site_id=`) AND localStorage (`observe.site_id`) so reloads round-trip
// the selection. No backend change — reuses /api/v1/sites.

const OBSERVE_URL = process.env.OBSERVE_URL || "http://127.0.0.1:3000";

async function adminToken(api: any): Promise<string> {
  const r = await api.post(`${OBSERVE_URL}/api/v1/auth/login`, {
    data: { username: "admin", password: "observe" },
  });
  expect(r.ok()).toBeTruthy();
  return (await r.json()).token;
}

async function ensureMultipleSites(): Promise<{ site_id: string; name: string }[]> {
  const api = await request.newContext();
  const token = await adminToken(api);
  const headers = { Authorization: `Bearer ${token}` };

  let sitesRes = await api.get(`${OBSERVE_URL}/api/v1/sites`, { headers });
  expect(sitesRes.ok()).toBeTruthy();
  let sites: { site_id: string; name: string }[] = await sitesRes.json();

  // Backfill so we have at least 2 sites for the dropdown test.
  const needed = Math.max(0, 2 - sites.length);
  for (let i = 0; i < needed; i++) {
    await api.post(`${OBSERVE_URL}/api/v1/sites`, {
      headers: { ...headers, "Content-Type": "application/json" },
      data: { name: `Switcher Test ${Date.now()}-${i}`, domain: `switcher-${Date.now()}-${i}.example.com` },
    });
  }
  if (needed > 0) {
    sitesRes = await api.get(`${OBSERVE_URL}/api/v1/sites`, { headers });
    sites = await sitesRes.json();
  }
  return sites;
}

test.describe("Site switcher", () => {
  test("renders trigger with the current site label", async ({ page }) => {
    await ensureMultipleSites();
    await login(page);

    const trigger = page.getByTestId("site-switcher-trigger");
    await expect(trigger).toBeVisible({ timeout: 10_000 });
    // Label is non-empty (could be the site name, domain, or raw id).
    await expect(trigger).not.toHaveText("");
  });

  test("opens dropdown and lists all sites with at least one option", async ({ page }) => {
    const sites = await ensureMultipleSites();
    await login(page);

    await page.getByTestId("site-switcher-trigger").click();
    const menu = page.getByTestId("site-switcher-menu");
    await expect(menu).toBeVisible();

    const options = page.getByTestId("site-switcher-option");
    await expect(options.first()).toBeVisible({ timeout: 5_000 });
    const count = await options.count();
    expect(count).toBeGreaterThanOrEqual(sites.length);

    // Create-new-site link is present.
    await expect(page.getByTestId("site-switcher-create")).toBeVisible();
  });

  test("selecting a site updates URL and localStorage", async ({ page }) => {
    const sites = await ensureMultipleSites();
    await login(page);

    // Pick the FIRST site that isn't currently selected. We read the current
    // value from localStorage rather than the URL because RouteFilterProvider
    // canonicalizes asynchronously.
    const current = await page.evaluate(() => localStorage.getItem("observe.site_id") || "");
    const target = sites.find(s => s.site_id !== current) || sites[1] || sites[0];
    expect(target).toBeTruthy();

    await page.getByTestId("site-switcher-trigger").click();
    await page.getByTestId("site-switcher-menu").waitFor();
    await page.locator(`[data-testid=site-switcher-option][data-site-id="${target.site_id}"]`).click();

    // URL canonicalizes via history.replaceState — give it a beat.
    await page.waitForFunction(
      (id) => new URL(window.location.href).searchParams.get("site_id") === id,
      target.site_id,
      { timeout: 5_000 },
    );

    const stored = await page.evaluate(() => localStorage.getItem("observe.site_id"));
    expect(stored).toBe(target.site_id);
  });

  test("selection survives a page reload via localStorage", async ({ page }) => {
    const sites = await ensureMultipleSites();
    expect(sites.length).toBeGreaterThanOrEqual(2);
    await login(page);

    const target = sites[1];
    await page.getByTestId("site-switcher-trigger").click();
    await page.getByTestId("site-switcher-menu").waitFor();
    await page.locator(`[data-testid=site-switcher-option][data-site-id="${target.site_id}"]`).click();
    await page.waitForFunction(
      (id) => localStorage.getItem("observe.site_id") === id,
      target.site_id,
    );

    // Hard-reload from a URL with NO site_id param so we test the
    // localStorage fallback path. RouteFilterProvider must restore it.
    await page.goto("/");
    await page.waitForLoadState("networkidle");

    await page.waitForFunction(
      (id) => new URL(window.location.href).searchParams.get("site_id") === id,
      target.site_id,
      { timeout: 5_000 },
    );

    const stored = await page.evaluate(() => localStorage.getItem("observe.site_id"));
    expect(stored).toBe(target.site_id);
  });
});
