import { test, expect, request } from "@playwright/test";
import { login } from "./helpers.js";

// C2 (Wave 4): cohorts behavioural grouping. Verify create/list via API,
// then UI render + "Use as filter" deep link to /insights with cohort_id.

const OBSERVE_URL = process.env.OBSERVE_URL || "http://127.0.0.1:3000";
const SITE = "default";

async function adminToken(api: any): Promise<string> {
  const r = await api.post(`${OBSERVE_URL}/api/v1/auth/login`, {
    data: { username: "admin", password: "observe" },
  });
  expect(r.ok()).toBeTruthy();
  return (await r.json()).token;
}

test.describe("Cohorts (C2 Wave 4)", () => {
  test("create + list via API returns the new cohort", async () => {
    const api = await request.newContext();
    const token = await adminToken(api);
    const headers = { Authorization: `Bearer ${token}`, "Content-Type": "application/json" };

    const name = `E2E Chrome ${Date.now()}`;
    const create = await api.post(`${OBSERVE_URL}/api/v1/cohorts`, {
      headers,
      data: {
        site_id: SITE,
        name,
        description: "users on Chrome",
        rule: {
          op: "and",
          rules: [{ type: "property", key: "browser", operator: "=", value: "Chrome" }],
        },
      },
    });
    expect(create.ok(), `create ${create.status()}: ${await create.text()}`).toBeTruthy();
    const created = await create.json();
    expect(created.cohort_id).toBeTruthy();
    expect(created.name).toBe(name);

    // member_count is an int — may be 0 on a fresh stack, so we don't
    // assert > 0. The contract is just that the field is present.
    expect(typeof created.member_count).toBe("number");

    const list = await api.get(`${OBSERVE_URL}/api/v1/cohorts?site_id=${SITE}`, {
      headers: { Authorization: `Bearer ${token}` },
    });
    expect(list.ok()).toBeTruthy();
    const cohorts: Array<{ cohort_id: string; name: string }> = await list.json();
    expect(cohorts.find((c) => c.cohort_id === created.cohort_id)).toBeTruthy();

    // Cleanup so repeated runs don't pile up cohorts.
    const del = await api.delete(`${OBSERVE_URL}/api/v1/cohorts/${created.cohort_id}?site_id=${SITE}`, {
      headers: { Authorization: `Bearer ${token}` },
    });
    expect(del.ok()).toBeTruthy();
  });

  test("preview endpoint returns a count without persisting", async () => {
    const api = await request.newContext();
    const token = await adminToken(api);
    const headers = { Authorization: `Bearer ${token}`, "Content-Type": "application/json" };

    const r = await api.post(`${OBSERVE_URL}/api/v1/cohorts/preview`, {
      headers,
      data: {
        site_id: SITE,
        rule: {
          op: "and",
          rules: [{ type: "property", key: "browser", operator: "=", value: "Chrome" }],
        },
      },
    });
    expect(r.ok(), `preview ${r.status()}: ${await r.text()}`).toBeTruthy();
    const body = await r.json();
    expect(typeof body.count).toBe("number");
    expect(Array.isArray(body.sample)).toBeTruthy();
  });

  test("/cohorts route renders cards and 'Use as filter' deep links to /insights", async ({ page, request: req }) => {
    // Seed a cohort via API so the UI has something to render.
    const token = await adminToken(req);
    const headers = { Authorization: `Bearer ${token}`, "Content-Type": "application/json" };
    const cohortName = `E2E UI ${Date.now()}`;
    const create = await req.post(`${OBSERVE_URL}/api/v1/cohorts`, {
      headers,
      data: {
        site_id: SITE,
        name: cohortName,
        description: "",
        rule: { op: "and", rules: [{ type: "property", key: "browser", operator: "=", value: "Chrome" }] },
      },
    });
    expect(create.ok()).toBeTruthy();
    const created = await create.json();
    const cohortID = created.cohort_id;

    try {
      await login(page);
      await page.goto("/cohorts");
      await page.waitForLoadState("networkidle");

      await expect(page.locator("h1", { hasText: "Cohorts" })).toBeVisible({ timeout: 10_000 });

      // Cohort card should render with the name we just created.
      const card = page.locator('[data-testid="cohort-card"]', { hasText: cohortName });
      await expect(card).toBeVisible({ timeout: 5_000 });

      // Click into the detail panel.
      await card.click();
      await expect(page.getByText(/Refresh/i)).toBeVisible({ timeout: 5_000 });

      // "Use as filter" must be a link with the cohort_id in its href.
      const filterLink = page.locator('[data-testid="cohort-use-as-filter"]');
      await expect(filterLink).toBeVisible();
      const href = await filterLink.getAttribute("href");
      expect(href).toContain(`cohort_id=${cohortID}`);
      expect(href?.startsWith("/insights")).toBeTruthy();
    } finally {
      // Cleanup
      await req.delete(`${OBSERVE_URL}/api/v1/cohorts/${cohortID}?site_id=${SITE}`, {
        headers: { Authorization: `Bearer ${token}` },
      });
    }
  });
});
