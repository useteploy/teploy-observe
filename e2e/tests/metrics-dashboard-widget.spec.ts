import { test, expect, request } from "@playwright/test";

// W3.A Phase 2: dashboard widget for metric_series.
//
// Round-trip flow: create a dashboard, ingest a counter metric, add a
// metric_series panel, execute the panel, assert the response is the
// fan-out series shape.

const OBSERVE_URL = process.env.OBSERVE_URL || "http://127.0.0.1:3000";

async function adminToken(api: any): Promise<string> {
  const r = await api.post(`${OBSERVE_URL}/api/v1/auth/login`, {
    data: { username: "admin", password: "observe" },
  });
  expect(r.ok()).toBeTruthy();
  return (await r.json()).token;
}

test.describe("Metric series dashboard widget (W3.A Phase 2)", () => {
  test("create dashboard + add metric_series panel + execute", async () => {
    const api = await request.newContext();
    const token = await adminToken(api);
    const headers = { Authorization: `Bearer ${token}`, "Content-Type": "application/json" };

    // 1. Ingest a synthetic counter so the panel has data to chart.
    const metricName = `e2e_widget_metric_${Date.now()}`;
    const baseMs = Date.now() - 30_000;
    const baseNs = baseMs * 1_000_000;
    const dp = (offsetSec: number, value: number) => ({
      timeUnixNano: String(baseNs + offsetSec * 1_000_000_000),
      asDouble: value,
      attributes: [{ key: "region", value: { stringValue: "us-east-1" } }],
    });
    const envelope = {
      resourceMetrics: [{
        resource: { attributes: [{ key: "service.name", value: { stringValue: "widget-svc" } }] },
        scopeMetrics: [{
          scope: { name: "playwright-widget", version: "1.0" },
          metrics: [{
            name: metricName,
            sum: {
              isMonotonic: true,
              aggregationTemporality: 2,
              dataPoints: [dp(0, 0), dp(1, 5), dp(2, 15)],
            },
          }],
        }],
      }],
    };
    const ingest = await api.post(`${OBSERVE_URL}/v1/metrics?site_id=default`, {
      data: envelope, headers: { "Content-Type": "application/json" },
    });
    expect(ingest.ok()).toBeTruthy();

    // 2. Create a dashboard.
    const dashRes = await api.post(`${OBSERVE_URL}/api/v1/dashboards`, {
      data: { site_id: "default", name: `e2e widget ${Date.now()}`, description: "" },
      headers,
    });
    expect(dashRes.ok(), `create ${dashRes.status()}`).toBeTruthy();
    const dash = await dashRes.json();
    const dashboardId = dash.dashboard_id;
    expect(dashboardId).toBeTruthy();

    // 3. Add a metric_series panel.
    const panelCfg = {
      metric: metricName,
      labels: { region: "us-east-1" },
      agg: "rate",
      step: "60s",
    };
    const panelRes = await api.post(`${OBSERVE_URL}/api/v1/dashboards/${dashboardId}/panels`, {
      data: {
        panel_type: "metric_series",
        title: "Request rate",
        query_type: "metric_series",
        query_config: JSON.stringify(panelCfg),
        width: "6",
        height: "4",
      },
      headers,
    });
    expect(panelRes.ok(), `add panel ${panelRes.status()}: ${await panelRes.text()}`).toBeTruthy();
    const panel = await panelRes.json();
    const panelId = panel.panel_id;
    expect(panelId).toBeTruthy();

    // 4. Execute the panel — should return Series[].
    const fromMs = String(baseMs - 5_000);
    const toMs = String(baseMs + 60_000);
    const execRes = await api.post(`${OBSERVE_URL}/api/v1/dashboards/${dashboardId}/panels/${panelId}/execute`, {
      data: { site_id: "default", from: fromMs, to: toMs },
      headers,
    });
    expect(execRes.ok(), `execute ${execRes.status()}: ${await execRes.text()}`).toBeTruthy();
    const series = await execRes.json();
    expect(Array.isArray(series), `expected array got ${typeof series}: ${JSON.stringify(series)}`).toBeTruthy();
    expect(series.length).toBeGreaterThanOrEqual(1);
    // Series object has labels + points
    expect(series[0]).toHaveProperty("labels");
    expect(series[0]).toHaveProperty("points");
    expect(Array.isArray(series[0].points)).toBeTruthy();
    expect(series[0].points.length).toBeGreaterThanOrEqual(1);
  });

  test("add panel rejects metric_series without metric name", async () => {
    const api = await request.newContext();
    const token = await adminToken(api);
    const headers = { Authorization: `Bearer ${token}`, "Content-Type": "application/json" };

    const dashRes = await api.post(`${OBSERVE_URL}/api/v1/dashboards`, {
      data: { site_id: "default", name: `e2e bad widget ${Date.now()}` },
      headers,
    });
    expect(dashRes.ok()).toBeTruthy();
    const dashboardId = (await dashRes.json()).dashboard_id;

    const bad = await api.post(`${OBSERVE_URL}/api/v1/dashboards/${dashboardId}/panels`, {
      data: {
        panel_type: "metric_series",
        title: "no metric",
        query_type: "metric_series",
        query_config: JSON.stringify({ agg: "rate" }), // missing metric
      },
      headers,
    });
    expect(bad.ok()).toBeFalsy();
  });
});
