import { test, expect, request } from "@playwright/test";
import { login } from "./helpers.js";

// W2.B: cross-site boards. The agency / MSP target. Verify the route
// renders, the builder modal lets us pick the default site, and the
// resulting grid shows a row with the expected columns. We don't
// assert non-zero pageviews because the seed data isn't always live
// and the e2e nucleus can be in a state where events SELECTs return
// zero (dogfood finding #20). The contract under test is: route
// works, builder works, summary endpoint serves.

const OBSERVE_URL = process.env.OBSERVE_URL || "http://127.0.0.1:3000";

async function adminToken(api: any): Promise<string> {
  const r = await api.post(`${OBSERVE_URL}/api/v1/auth/login`, {
    data: { username: "admin", password: "observe" },
  });
  expect(r.ok()).toBeTruthy();
  return (await r.json()).token;
}

test.describe("Boards (W2.B)", () => {
  test("summary endpoint returns one row per site_id with the expected fields", async () => {
    const api = await request.newContext();
    const token = await adminToken(api);
    const headers = { Authorization: `Bearer ${token}` };

    // Seed a known set of sites (default plus any pre-existing).
    const sitesRes = await api.get(`${OBSERVE_URL}/api/v1/sites`, { headers });
    expect(sitesRes.ok()).toBeTruthy();
    const sites: Array<{ site_id: string; name: string }> = await sitesRes.json();
    expect(sites.length).toBeGreaterThanOrEqual(1);

    const ids = sites.slice(0, Math.min(3, sites.length)).map((s) => s.site_id);
    const from = new Date(Date.now() - 30 * 86400_000).toISOString();
    const to = new Date().toISOString();

    const r = await api.get(
      `${OBSERVE_URL}/api/v1/boards/summary?site_ids=${ids.join(",")}` +
        `&from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}`,
      { headers },
    );
    expect(r.ok(), `summary ${r.status()}: ${await r.text()}`).toBeTruthy();
    const rows = await r.json();
    expect(Array.isArray(rows)).toBeTruthy();
    expect(rows.length).toBe(ids.length);

    // Every row must carry the 7 numeric / string fields the grid renders.
    for (const row of rows) {
      expect(typeof row.site_id).toBe("string");
      expect(typeof row.pageviews).toBe("number");
      expect(typeof row.visitors).toBe("number");
      expect(typeof row.errors).toBe("number");
      expect(typeof row.uptime_pct).toBe("number");
      expect(typeof row.replay_count).toBe("number");
      expect(typeof row.last_activity_ms).toBe("number");
    }
  });

  test("/boards route renders, can create a board, and the grid shows a row", async ({ page }) => {
    await login(page);
    await page.goto("/boards");
    await page.waitForLoadState("networkidle");

    // Title + sidebar entry render. (Sidebar entry test indirectly proves
    // the _layout NAV_ITEMS update wired correctly.)
    await expect(page.locator("h1", { hasText: "Boards" })).toBeVisible({ timeout: 10_000 });

    // Open the builder modal. The button label is the same on the empty
    // state and the page header — clicking either should work.
    const newBtn = page.getByRole("button", { name: /New board|first board/i }).first();
    await newBtn.click();

    // Modal should open with a Name input + at least one site row.
    await expect(page.getByPlaceholder("All Customers")).toBeVisible();
    const checkboxes = page.locator(".boards-site-row input[type=checkbox]");
    await expect(checkboxes.first()).toBeVisible({ timeout: 5_000 });

    // Fill in the form: pick the first site, give it a name, click Create.
    // Preact controlled inputs ignore raw .fill(), so click + type.
    const boardName = `E2E Board ${Date.now()}`;
    const nameInput = page.getByPlaceholder("All Customers");
    await nameInput.click();
    await nameInput.pressSequentially(boardName, { delay: 10 });
    await checkboxes.first().check();
    // Sanity: confirm the controlled input took the value before submit.
    await expect(nameInput).toHaveValue(boardName);
    await page.getByRole("button", { name: /^Create$/ }).click();

    // After save, the board should be open with a grid table that has
    // the expected column headers.
    await expect(page.locator("h2", { hasText: boardName })).toBeVisible({ timeout: 10_000 });
    await expect(page.locator("th", { hasText: "Pageviews" })).toBeVisible();
    await expect(page.locator("th", { hasText: "Errors" })).toBeVisible();
    await expect(page.locator("th", { hasText: "Uptime" })).toBeVisible();

    // At least one row must render (we picked one site).
    await expect(page.locator(".boards-row").first()).toBeVisible({ timeout: 5_000 });
  });
});
