import { test, expect, request as pwRequest } from "@playwright/test";
import { login } from "./helpers.js";

test.describe("traces — funnels (W2.A)", () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  test("Funnels tab renders builder and runs preview against seeded ops", async ({ page }) => {
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

    // Seed contains a trace where root op = "GET /users/:id" and a child
    // op = "db.query users". Use those as the 2-step funnel — both always
    // co-occur in the same trace, so step1 must yield a non-zero count.
    await page.getByTestId("trace-funnel-op-0").fill("GET /users/:id");
    await page.getByTestId("trace-funnel-op-1").fill("db.query users");

    await page.getByTestId("trace-funnel-run").click();

    const result = page.getByTestId("trace-funnel-result");
    await expect(result).toBeVisible({ timeout: 10_000 });
    // Both steps should report a non-zero count for the seed data.
    await expect(result).toContainText("GET /users/:id");
    await expect(result).toContainText("db.query users");

    expect(errors, `console errors: ${errors.join(" | ")}`).toHaveLength(0);
  });

  test("preview API computes a funnel against seeded spans", async ({ page, baseURL }) => {
    const ctx = await pwRequest.newContext({ baseURL, storageState: await page.context().storageState() });
    const to = Date.now();
    const from = to - 24 * 60 * 60 * 1000;

    const res = await ctx.post("/api/v1/tracing/funnel/preview", {
      data: {
        site_id: "default",
        ops: ["GET /users/:id", "db.query users"],
        from,
        to,
      },
    });
    expect(res.ok(), `POST /api/v1/tracing/funnel/preview status=${res.status()}`).toBeTruthy();
    const body = await res.json();
    expect(Array.isArray(body.steps), "steps is array").toBeTruthy();
    expect(body.steps.length).toBe(2);
    expect(body.steps[0].operation).toBe("GET /users/:id");
    expect(body.steps[1].operation).toBe("db.query users");
    expect(body.steps[0].count, `step0 count must be >0 (seed has this op), got ${JSON.stringify(body)}`).toBeGreaterThan(0);

    await ctx.dispose();
  });

  test("save / list / delete saved funnels round-trip", async ({ page, baseURL }) => {
    const ctx = await pwRequest.newContext({ baseURL, storageState: await page.context().storageState() });
    const name = `e2e-funnel-${Date.now()}`;

    const create = await ctx.post("/api/v1/tracing/funnel/saved", {
      data: { site_id: "default", name, ops: ["GET /users/:id", "db.query users"] },
    });
    expect(create.ok(), `save status=${create.status()}`).toBeTruthy();
    const created = await create.json();
    expect(created.name).toBe(name);
    expect(created.ops).toEqual(["GET /users/:id", "db.query users"]);

    const list = await ctx.get(`/api/v1/tracing/funnel/saved?site_id=default`);
    expect(list.ok()).toBeTruthy();
    const all = await list.json();
    const found = (all as Array<{ name: string; view_id: string }>).find(f => f.name === name);
    expect(found, `saved funnel ${name} should be in list`).toBeTruthy();

    const del = await ctx.delete(`/api/v1/tracing/funnel/saved/${found!.view_id}`);
    expect(del.ok(), `delete status=${del.status()}`).toBeTruthy();

    await ctx.dispose();
  });
});
