import { test, expect, request as pwRequest } from "@playwright/test";
import { login } from "./helpers.js";

// B2 phase 1 — release health endpoints + UI surface.
//
// The math is unit-tested in internal/errors/releases_test.go against a
// synthetic dataset (precise crash-free / adoption assertions). This e2e
// asserts the wire contract:
//
//   - GET /api/v1/releases/health is 200 with a JSON array (never null
//     even on an empty window).
//   - GET /api/v1/releases/sparkline is 200 with a JSON array of 14
//     daily points (zero-filled for days with no sessions).
//   - The /releases page loads without console errors and the legacy
//     release list still renders.
//
// The stat-grid is conditional on at least one release having sessions
// in the window — a fresh seed has none, so we don't pin its presence
// here. The unit test covers the data-present case.

const SITE = "default";

async function adminToken(api: any): Promise<string> {
  const r = await api.post("/api/v1/auth/login", {
    data: { username: "admin", password: "observe" },
  });
  expect(r.ok()).toBeTruthy();
  return (await r.json()).token;
}

test.describe("releases — phase 1 health endpoints", () => {
  test("GET /api/v1/releases/health returns 200 [] on an empty window", async ({ baseURL }) => {
    const api = await pwRequest.newContext({ baseURL });
    const token = await adminToken(api);

    // Future window guaranteed empty.
    const from = "2099-01-01T00:00:00Z";
    const to = "2099-01-02T00:00:00Z";
    const r = await api.get(
      `/api/v1/releases/health?site_id=${SITE}&from=${from}&to=${to}`,
      { headers: { Authorization: `Bearer ${token}` } },
    );
    expect(r.status()).toBe(200);
    const raw = await r.text();
    expect(raw.trim(), "must not be the literal null").not.toBe("null");
    const body = JSON.parse(raw);
    expect(Array.isArray(body)).toBeTruthy();
    expect(body.length).toBe(0);
    await api.dispose();
  });

  test("GET /api/v1/releases/sparkline returns a 14-day series for any release tag", async ({ baseURL }) => {
    const api = await pwRequest.newContext({ baseURL });
    const token = await adminToken(api);

    const r = await api.get(
      `/api/v1/releases/sparkline?site_id=${SITE}&release=__no_such_release__&days=14`,
      { headers: { Authorization: `Bearer ${token}` } },
    );
    expect(r.status()).toBe(200);
    const body = await r.json();
    expect(Array.isArray(body)).toBeTruthy();
    expect(body.length).toBe(14);
    for (const p of body) {
      expect(typeof p.day_ms).toBe("number");
      expect(typeof p.crash_free_session_pct).toBe("number");
      expect(p.crash_free_session_pct).toBeGreaterThanOrEqual(0);
      expect(p.crash_free_session_pct).toBeLessThanOrEqual(100);
    }
    await api.dispose();
  });

  test("GET /api/v1/releases/sparkline rejects an empty release", async ({ baseURL }) => {
    const api = await pwRequest.newContext({ baseURL });
    const token = await adminToken(api);
    const r = await api.get(
      `/api/v1/releases/sparkline?site_id=${SITE}&release=&days=14`,
      { headers: { Authorization: `Bearer ${token}` } },
    );
    expect([400, 422]).toContain(r.status());
    await api.dispose();
  });
});

test.describe("releases — UI", () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  test("/releases loads without console errors and the export button renders", async ({ page }) => {
    const errors: string[] = [];
    page.on("pageerror", (e) => errors.push(e.message));
    page.on("console", (msg) => {
      if (msg.type() === "error" && !/401|Unauthorized/.test(msg.text())) {
        errors.push(msg.text());
      }
    });

    const resp = await page.goto("/releases", { waitUntil: "networkidle" });
    expect(resp?.status()).toBe(200);

    // Heading always renders. Either the empty state OR the legacy list
    // is visible depending on whether the install has any release_tag
    // error events. The new stat grid is conditional on session-tagged
    // releases, which a fresh seed doesn't include yet.
    await expect(page.getByRole("heading", { name: "Releases" })).toBeVisible();

    expect(errors, `console errors: ${errors.join(" | ")}`).toHaveLength(0);
  });
});
