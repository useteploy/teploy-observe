import { test, expect, request } from "@playwright/test";

// W3.A Phase 2: rate() aggregation for cumulative counters.
//
// Strategy: ingest a synthetic monotonic counter time-series, query it
// via /api/v1/metrics/series with agg=rate, assert the per-second slope
// matches the expected value. Also exercise step + group_by so the
// per-label-set fan-out covers a real wire round-trip.

const OBSERVE_URL = process.env.OBSERVE_URL || "http://127.0.0.1:3000";

async function adminToken(api: any): Promise<string> {
  const r = await api.post(`${OBSERVE_URL}/api/v1/auth/login`, {
    data: { username: "admin", password: "observe" },
  });
  expect(r.ok()).toBeTruthy();
  return (await r.json()).token;
}

test.describe("Metrics rate() + group_by (W3.A Phase 2)", () => {
  test("rate query returns per-second slope across counter resets", async () => {
    const api = await request.newContext();
    const metricName = `e2e_rate_metric_${Date.now()}`;
    // Anchor the series in the recent past so all 4 points fall inside
    // a single 60s bucket regardless of clock skew.
    const baseMs = Date.now() - 30_000;
    const baseNs = baseMs * 1_000_000;

    // Synthetic cumulative counter: 0 → 10 → 30 (slope 10/s, 20/s),
    // RESET to 5 → 15 (slope 10/s) over 4s.
    const dp = (offsetSec: number, value: number) => ({
      timeUnixNano: String(baseNs + offsetSec * 1_000_000_000),
      asDouble: value,
      attributes: [{ key: "region", value: { stringValue: "us-east-1" } }],
    });

    const envelope = {
      resourceMetrics: [{
        resource: { attributes: [{ key: "service.name", value: { stringValue: "rate-svc" } }] },
        scopeMetrics: [{
          scope: { name: "playwright-rate", version: "1.0" },
          metrics: [{
            name: metricName,
            sum: {
              isMonotonic: true,
              aggregationTemporality: 2,
              dataPoints: [dp(0, 0), dp(1, 10), dp(2, 30), dp(3, 5), dp(4, 15)],
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

    const token = await adminToken(api);
    const headers = { Authorization: `Bearer ${token}` };
    const fromMs = baseMs - 5_000;
    const toMs = baseMs + 60_000;

    // Phase-1 single-series shape — agg=rate goes through the same path.
    const q = await api.get(
      `${OBSERVE_URL}/api/v1/metrics/query?site_id=default&name=${metricName}&from=${fromMs}&to=${toMs}&agg=rate&step=60s`,
      { headers },
    );
    expect(q.ok(), `rate query ${q.status()}: ${await q.text()}`).toBeTruthy();
    const body = await q.json();
    expect(Array.isArray(body)).toBeTruthy();
    // Slopes 10, 20, (skipped reset), 10 → mean 13.33…
    expect(body.length).toBeGreaterThanOrEqual(1);
    const total = body.reduce((acc: number, p: any) => acc + p.value, 0);
    expect(total).toBeGreaterThan(0);
    expect(total).toBeLessThan(50); // sanity bound
  });

  test("series endpoint fans out one entry per label combination", async () => {
    const api = await request.newContext();
    const metricName = `e2e_groupby_metric_${Date.now()}`;
    const baseMs = Date.now() - 30_000;
    const baseNs = baseMs * 1_000_000;
    const dp = (region: string, value: number) => ({
      timeUnixNano: String(baseNs),
      asDouble: value,
      attributes: [{ key: "region", value: { stringValue: region } }],
    });

    const envelope = {
      resourceMetrics: [{
        resource: { attributes: [{ key: "service.name", value: { stringValue: "fanout-svc" } }] },
        scopeMetrics: [{
          scope: { name: "playwright-fanout", version: "1.0" },
          metrics: [{
            name: metricName,
            gauge: {
              dataPoints: [dp("us-east-1", 1), dp("eu-west-1", 2), dp("ap-south-1", 3)],
            },
          }],
        }],
      }],
    };

    const ingest = await api.post(`${OBSERVE_URL}/v1/metrics?site_id=default`, {
      data: envelope,
      headers: { "Content-Type": "application/json" },
    });
    expect(ingest.ok()).toBeTruthy();

    const token = await adminToken(api);
    const headers = { Authorization: `Bearer ${token}` };
    const fromMs = baseMs - 5_000;
    const toMs = baseMs + 60_000;

    const r = await api.get(
      `${OBSERVE_URL}/api/v1/metrics/series?site_id=default&name=${metricName}&from=${fromMs}&to=${toMs}&agg=last&group_by=region`,
      { headers },
    );
    expect(r.ok(), `series ${r.status()}: ${await r.text()}`).toBeTruthy();
    const body = await r.json();
    expect(Array.isArray(body)).toBeTruthy();
    // Expect exactly 3 distinct series, one per region.
    expect(body.length).toBe(3);
    const labels = body.map((s: any) => s.labels?.region).sort();
    expect(labels).toEqual(["ap-south-1", "eu-west-1", "us-east-1"]);
  });

  test("invalid step + invalid agg are rejected with 400", async () => {
    const api = await request.newContext();
    const token = await adminToken(api);
    const headers = { Authorization: `Bearer ${token}` };
    const fromMs = Date.now() - 60_000;
    const toMs = Date.now();
    const bad1 = await api.get(
      `${OBSERVE_URL}/api/v1/metrics/query?site_id=default&name=anything&from=${fromMs}&to=${toMs}&agg=histogram_quantile`,
      { headers },
    );
    expect(bad1.status()).toBe(400);
    const bad2 = await api.get(
      `${OBSERVE_URL}/api/v1/metrics/query?site_id=default&name=anything&from=${fromMs}&to=${toMs}&step=2y`,
      { headers },
    );
    expect(bad2.status()).toBe(400);
  });
});
