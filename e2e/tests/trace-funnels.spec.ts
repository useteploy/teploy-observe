import { test, expect, request as pwRequest } from "@playwright/test";
import { login } from "./helpers.js";

// W2.A trace funnels e2e. Note: this test does NOT assert non-zero counts
// against seeded traces because dogfood finding #25 (OTLP ingest doesn't
// persist via neutron-go) means the seeded traces table is empty in dev.
// Instead these tests validate the API + UI contracts (shape, codes,
// round-trip) and rely on the unit tests in internal/tracing/funnels_test.go
// for correctness of the funnel computation itself.

const SITE = "default";

async function loginToken(api: any, baseURL: string | undefined): Promise<string> {
  const r = await api.post(`${baseURL || ""}/api/v1/auth/login`, {
    data: { username: "admin", password: "observe" },
  });
  expect(r.ok(), `login failed: ${r.status()} ${await r.text()}`).toBeTruthy();
  return (await r.json()).token;
}

test.describe("traces — funnels (W2.A)", () => {
  test("Funnels tab renders builder and runs preview", async ({ page }) => {
    await login(page);
    const errors: string[] = [];
    page.on("pageerror", (err) => errors.push(err.message));
    page.on("console", (msg) => {
      if (msg.type() === "error" && !/401|Unauthorized/.test(msg.text())) {
        errors.push(msg.text());
      }
    });

    const resp = await page.goto("/traces", { waitUntil: "networkidle" });
    expect(resp?.status(), "GET /traces").toBe(200);

    const tab = page.getByTestId("traces-tab-funnels");
    await expect(tab).toBeVisible();
    await tab.click();

    const builder = page.getByTestId("trace-funnel-builder");
    await expect(builder).toBeVisible({ timeout: 5_000 });

    // Define a 2-step funnel against operation names that are likely to
    // appear in seeded data when ingest is healthy.
    await page.getByTestId("trace-funnel-op-0").fill("GET /products");
    await page.getByTestId("trace-funnel-op-1").fill("POST /cart");

    await page.getByTestId("trace-funnel-run").click();

    const result = page.getByTestId("trace-funnel-result");
    await expect(result).toBeVisible({ timeout: 10_000 });
    // The result panel must render both step labels regardless of count.
    await expect(result).toContainText("GET /products");
    await expect(result).toContainText("POST /cart");

    expect(errors, `console errors: ${errors.join(" | ")}`).toHaveLength(0);
  });

  test("preview API returns well-shaped FunnelResult", async ({ baseURL }) => {
    const api = await pwRequest.newContext();
    const token = await loginToken(api, baseURL);
    const headers = { Authorization: `Bearer ${token}` };
    const to = Date.now();
    const from = to - 24 * 60 * 60 * 1000;

    const res = await api.post(`${baseURL}/api/v1/tracing/funnel/preview`, {
      headers,
      data: {
        site_id: SITE,
        ops: ["GET /products", "POST /cart", "POST /checkout"],
        from,
        to,
      },
    });
    expect(res.ok(), `POST /api/v1/tracing/funnel/preview status=${res.status()} body=${await res.text()}`).toBeTruthy();
    const body = await res.json();
    expect(Array.isArray(body.steps), "steps is array").toBeTruthy();
    expect(body.steps.length).toBe(3);
    expect(body.steps[0].operation).toBe("GET /products");
    expect(body.steps[1].operation).toBe("POST /cart");
    expect(body.steps[2].operation).toBe("POST /checkout");
    // Schema sanity — every step has the documented numeric fields.
    for (const s of body.steps) {
      expect(typeof s.count).toBe("number");
      expect(typeof s.conversion_pct).toBe("number");
      expect(typeof s.median_gap_ms).toBe("number");
      expect(typeof s.p95_gap_ms).toBe("number");
    }
    expect(typeof body.total_traces).toBe("number");

    await api.dispose();
  });

  test("preview rejects single-op funnels with 400", async ({ baseURL }) => {
    const api = await pwRequest.newContext();
    const token = await loginToken(api, baseURL);
    const headers = { Authorization: `Bearer ${token}` };
    const to = Date.now();
    const from = to - 60 * 60 * 1000;
    const res = await api.post(`${baseURL}/api/v1/tracing/funnel/preview`, {
      headers,
      data: { site_id: SITE, ops: ["only one"], from, to },
    });
    expect(res.status()).toBe(400);
    await api.dispose();
  });

  test("save / list / delete saved funnels round-trip", async ({ baseURL }) => {
    const api = await pwRequest.newContext();
    const token = await loginToken(api, baseURL);
    const headers = { Authorization: `Bearer ${token}` };
    const name = `e2e-funnel-${Date.now()}`;

    const create = await api.post(`${baseURL}/api/v1/tracing/funnel/saved`, {
      headers,
      data: { site_id: SITE, name, ops: ["GET /products", "POST /cart"] },
    });
    expect(create.ok(), `save status=${create.status()} body=${await create.text()}`).toBeTruthy();
    const created = await create.json();
    expect(created.name).toBe(name);
    expect(created.ops).toEqual(["GET /products", "POST /cart"]);

    const list = await api.get(`${baseURL}/api/v1/tracing/funnel/saved?site_id=${SITE}`, { headers });
    expect(list.ok()).toBeTruthy();
    const all = await list.json();
    const found = (all as Array<{ name: string; view_id: string }>).find(f => f.name === name);
    expect(found, `saved funnel ${name} should be in list`).toBeTruthy();

    const del = await api.delete(`${baseURL}/api/v1/tracing/funnel/saved/${found!.view_id}`, { headers });
    expect(del.ok(), `delete status=${del.status()}`).toBeTruthy();

    await api.dispose();
  });
});
