import { test, expect } from "@playwright/test";

test("Swagger UI is served at /api/docs", async ({ page }) => {
  const resp = await page.goto("/api/docs");
  expect(resp?.status()).toBe(200);
  const body = await page.content();
  expect(body).toContain("swagger");
});

test("OpenAPI spec serves 80+ paths", async ({ request }) => {
  const resp = await request.get("/openapi.json");
  expect(resp.status()).toBe(200);
  const spec = await resp.json();
  expect(spec.openapi).toBe("3.1.0");
  expect(Object.keys(spec.paths).length).toBeGreaterThan(80);
});
