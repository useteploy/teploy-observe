import { test, expect, request } from "@playwright/test";
import { login } from "./helpers.js";

// W3.A Phase 1: OTLP metrics ingest + query + minimal UI.
//
// Strategy: POST a synthetic OTLP envelope to /v1/metrics directly (no
// SDK in the loop, mirrors what an OTel collector would send), GET it
// back via /api/v1/metrics/query, and verify the UI surfaces the
// metric in the list panel and renders a chart.

const OBSERVE_URL = process.env.OBSERVE_URL || "http://127.0.0.1:3000";

async function adminToken(api: any): Promise<string> {
  const r = await api.post(`${OBSERVE_URL}/api/v1/auth/login`, {
    data: { username: "admin", password: "observe" },
  });
  expect(r.ok()).toBeTruthy();
  return (await r.json()).token;
}

test.describe("Metrics (W3.A Phase 1)", () => {
  const metricName = `e2e_test_metric_${Date.now()}`;
  const nowNs = (Date.now() * 1_000_000).toString();

  test("POST /v1/metrics ingests + GET /api/v1/metrics/query returns the points", async () => {
    const api = await request.newContext();

    // Send a synthetic OTLP gauge envelope. No auth on /v1/metrics
    // (matches /v1/traces convention).
    const envelope = {
      resourceMetrics: [{
        resource: {
          attributes: [{ key: "service.name", value: { stringValue: "e2e-svc" } }],
        },
        scopeMetrics: [{
          scope: { name: "playwright", version: "1.0" },
          metrics: [{
            name: metricName,
            gauge: {
              dataPoints: [
                { timeUnixNano: nowNs, asDouble: 42.5, attributes: [{ key: "region", value: { stringValue: "us-east-1" } }] },
                { timeUnixNano: nowNs, asDouble: 17.0, attributes: [{ key: "region", value: { stringValue: "eu-west-1" } }] },
              ],
            },
          }],
        }],
      }],
    };

    const ingest = await api.post(`${OBSERVE_URL}/v1/metrics?site_id=default`, {
      data: envelope,
      headers: { "Content-Type": "application/json" },
    });
    expect(ingest.ok(), `ingest ${ingest.status()}: ${await ingest.text()}`).toBeTruthy();
    const ingestBody = await ingest.json();
    expect(ingestBody.ok).toBe(true);
    expect(ingestBody.points).toBe(2);

    // Query back. Uses JWT auth since it's under /api/v1/.
    const token = await adminToken(api);
    const headers = { Authorization: `Bearer ${token}` };

    const fromMs = Date.now() - 60_000;
    const toMs = Date.now() + 60_000;

    const list = await api.get(`${OBSERVE_URL}/api/v1/metrics/list?site_id=default`, { headers });
    expect(list.ok(), `list ${list.status()}: ${await list.text()}`).toBeTruthy();
    const listBody = await list.json();
    expect(Array.isArray(listBody)).toBeTruthy();
    const found = listBody.find((m: any) => m.name === metricName);
    expect(found, `metric ${metricName} not in list ${JSON.stringify(listBody)}`).toBeTruthy();
    expect(found.kind).toBe("gauge");

    const q = await api.get(
      `${OBSERVE_URL}/api/v1/metrics/query?site_id=default&name=${metricName}&from=${fromMs}&to=${toMs}&agg=avg`,
      { headers },
    );
    expect(q.ok(), `query ${q.status()}: ${await q.text()}`).toBeTruthy();
    const qBody = await q.json();
    expect(Array.isArray(qBody)).toBeTruthy();
    expect(qBody.length, `expected >=1 bucket, got ${JSON.stringify(qBody)}`).toBeGreaterThanOrEqual(1);
    // avg of {42.5, 17.0} = 29.75
    const total = qBody.reduce((acc: number, p: any) => acc + p.value, 0);
    expect(total).toBeGreaterThan(0);

    // Label filter narrows the result to just the us-east-1 point.
    const qFilt = await api.get(
      `${OBSERVE_URL}/api/v1/metrics/query?site_id=default&name=${metricName}&from=${fromMs}&to=${toMs}&agg=last&label.region=us-east-1`,
      { headers },
    );
    expect(qFilt.ok()).toBeTruthy();
    const qfBody = await qFilt.json();
    expect(qfBody.length).toBeGreaterThanOrEqual(1);
    expect(qfBody[0].value).toBeCloseTo(42.5, 1);

    await api.dispose();
  });

  test("UI /metrics route lists metric and renders chart", async ({ page }) => {
    // First make sure a metric exists for the seeded site.
    const api = await request.newContext();
    const uiMetric = `e2e_ui_metric_${Date.now()}`;
    const envelope = {
      resourceMetrics: [{
        resource: { attributes: [{ key: "service.name", value: { stringValue: "ui-svc" } }] },
        scopeMetrics: [{
          scope: { name: "playwright", version: "1.0" },
          metrics: [{
            name: uiMetric,
            sum: {
              isMonotonic: true,
              aggregationTemporality: 2,
              dataPoints: [{
                timeUnixNano: (Date.now() * 1_000_000).toString(),
                asDouble: 100,
                attributes: [],
              }],
            },
          }],
        }],
      }],
    };
    const r = await api.post(`${OBSERVE_URL}/v1/metrics?site_id=default`, {
      data: envelope, headers: { "Content-Type": "application/json" },
    });
    expect(r.ok()).toBeTruthy();
    await api.dispose();

    await login(page);
    await page.goto("/metrics");
    await page.waitForLoadState("networkidle");

    await expect(page.locator("h1", { hasText: "Metrics" })).toBeVisible({ timeout: 10_000 });

    // The metric we just ingested should appear in the list.
    const item = page.getByTestId(`metric-item-${uiMetric}`);
    await expect(item).toBeVisible({ timeout: 10_000 });
    await item.click();

    // Selecting it should populate the chart panel header + render the SVG.
    await expect(page.getByTestId("metric-selected-name")).toContainText(uiMetric);
    await expect(page.getByTestId("metric-chart").locator("svg")).toBeVisible({ timeout: 10_000 });
  });

  test("sidebar Metrics link is visible", async ({ page }) => {
    await login(page);
    await expect(page.locator(".obs-sidebar-link", { hasText: "Metrics" })).toBeVisible();
  });

  test("UI agg dropdown switches between reducers and chart re-renders", async ({ page }) => {
    // Phase 2: agg is a dropdown that exposes last/avg/sum/min/max/rate/p50/p95/p99.
    // Confirm that switching the value triggers a re-fetch and the chart stays
    // visible — we assert via the data-testid attached to the SVG container.
    const api = await request.newContext();
    const uiMetric = `e2e_agg_metric_${Date.now()}`;
    const baseNs = (Date.now() - 30_000) * 1_000_000;
    const envelope = {
      resourceMetrics: [{
        resource: { attributes: [{ key: "service.name", value: { stringValue: "agg-svc" } }] },
        scopeMetrics: [{
          scope: { name: "playwright-agg", version: "1.0" },
          metrics: [{
            name: uiMetric,
            sum: {
              isMonotonic: true,
              aggregationTemporality: 2,
              dataPoints: [
                { timeUnixNano: String(baseNs), asDouble: 0, attributes: [] },
                { timeUnixNano: String(baseNs + 1_000_000_000), asDouble: 5, attributes: [] },
                { timeUnixNano: String(baseNs + 2_000_000_000), asDouble: 15, attributes: [] },
              ],
            },
          }],
        }],
      }],
    };
    const r = await api.post(`${OBSERVE_URL}/v1/metrics?site_id=default`, {
      data: envelope, headers: { "Content-Type": "application/json" },
    });
    expect(r.ok()).toBeTruthy();
    await api.dispose();

    await login(page);
    await page.goto("/metrics");
    await page.waitForLoadState("networkidle");

    const item = page.getByTestId(`metric-item-${uiMetric}`);
    await expect(item).toBeVisible({ timeout: 10_000 });
    await item.click();

    const aggSelect = page.getByTestId("metric-agg-select");
    await expect(aggSelect).toBeVisible();

    // Switch agg through several reducers — the chart-container testid
    // must remain present (i.e. no crash) for each.
    for (const a of ["avg", "sum", "rate", "p95"]) {
      await aggSelect.selectOption(a);
      await page.waitForTimeout(200);
      await expect(page.getByTestId("metric-chart")).toBeVisible();
    }
  });
});
