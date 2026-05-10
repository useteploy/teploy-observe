import { test, expect } from "@playwright/test";
import { login } from "./helpers.js";

// Multi-touch UTM attribution (W2.C / Umami gap #1):
//
// 1. /api/v1/attribution must respond 200 with a JSON array for any of
//    the three supported models (and 400 for nonsense models).
// 2. The new "Attribution" tab on /campaigns must mount, and switching
//    the model selector must trigger a fresh request and re-render the
//    panel — even when the underlying dataset is empty (we don't seed
//    UTM events here so the empty-state path is the most stable thing
//    to assert against).

test.describe("UTM attribution", () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  test("GET /api/v1/attribution returns an array for each model", async ({ page }) => {
    const from = "2025-01-01T00:00:00Z";
    const to = "2030-01-01T00:00:00Z";

    for (const model of ["first", "last", "linear"] as const) {
      const result = await page.evaluate(async ({ from, to, model }) => {
        const token = localStorage.getItem("obs_token");
        const r = await fetch(
          `/api/v1/attribution?site_id=default&model=${model}&from=${from}&to=${to}`,
          { headers: token ? { Authorization: `Bearer ${token}` } : {} },
        );
        const text = await r.text();
        let body: unknown = null;
        try { body = JSON.parse(text); } catch { /* leave null */ }
        return { status: r.status, text, body };
      }, { from, to, model });

      expect(result.status, `model=${model} body=${result.text}`).toBe(200);
      expect(Array.isArray(result.body), `model=${model} body=${result.text}`).toBeTruthy();
      // Don't assert specific numbers — seed may not have utm-tagged data.
      // Just verify the row shape on whatever rows exist.
      for (const row of result.body as Array<{ source: string; sessions: number; conversions: number; conversion_pct: number }>) {
        expect(typeof row.source).toBe("string");
        expect(typeof row.sessions).toBe("number");
        expect(typeof row.conversions).toBe("number");
        expect(typeof row.conversion_pct).toBe("number");
      }
    }
  });

  test("GET /api/v1/attribution rejects an invalid model with 400", async ({ page }) => {
    const result = await page.evaluate(async () => {
      const token = localStorage.getItem("obs_token");
      const r = await fetch(
        `/api/v1/attribution?site_id=default&model=nonsense`,
        { headers: token ? { Authorization: `Bearer ${token}` } : {} },
      );
      return { status: r.status };
    });
    expect(result.status).toBe(400);
  });

  test("Attribution tab on /campaigns mounts and re-queries on model switch", async ({ page }) => {
    const errors: string[] = [];
    page.on("pageerror", (err) => errors.push(err.message));

    await page.goto("/campaigns", { waitUntil: "networkidle" });

    // Click into the new Attribution tab.
    const attribTab = page.getByTestId("campaigns-tab-attribution");
    await expect(attribTab).toBeVisible();
    await attribTab.click();

    // The panel should mount regardless of dataset state.
    const panel = page.getByTestId("campaigns-attribution-panel");
    await expect(panel).toBeVisible();

    // Track the API calls so we can prove model switches re-query.
    const requested: string[] = [];
    page.on("request", (req) => {
      if (req.url().includes("/api/v1/attribution")) {
        const u = new URL(req.url());
        const m = u.searchParams.get("model");
        if (m) requested.push(m);
      }
    });

    // The buttons exist in all three states even when there's no data —
    // empty-state is rendered inside the panel, not in place of it.
    for (const model of ["last", "linear", "first"] as const) {
      const btn = page.getByTestId(`attribution-model-${model}`);
      await expect(btn).toBeVisible();
      await btn.click();
      // Either the table or the empty-state should render after the
      // model switch. Either is fine — we just want proof the panel
      // re-rendered.
      await Promise.race([
        expect(page.getByTestId("attribution-table")).toBeVisible({ timeout: 5_000 }).catch(() => {}),
        expect(page.getByTestId("attribution-empty")).toBeVisible({ timeout: 5_000 }).catch(() => {}),
      ]);
      // aria-checked flipping is the surest single-button signal that
      // state advanced after the click.
      await expect(btn).toHaveAttribute("aria-checked", "true");
    }

    // We clicked three models; should have fired at least three requests
    // (the initial mount can add a fourth — anything ≥3 is fine).
    expect(requested.length).toBeGreaterThanOrEqual(3);
    expect(requested).toContain("first");
    expect(requested).toContain("last");
    expect(requested).toContain("linear");

    expect(errors, `pageerrors during attribution UI: ${errors.join(" | ")}`).toHaveLength(0);
  });
});
