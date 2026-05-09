import { test, expect, request } from "@playwright/test";

// T044 dogfood: every HTTP request to Observe should produce a trace span
// under site_id=_meta. This test makes a request, then verifies the span
// shows up via the trace search API. Loop-prevention is also asserted —
// the trace ingest endpoint itself must NOT be traced.
//
// Self-monitoring is gated on OBSERVE_DOGFOOD=true at server boot, so this
// suite skips itself when the server is running without it.

const OBSERVE_URL = process.env.OBSERVE_URL || "http://localhost:3000";

test.skip(
  () => process.env.OBSERVE_DOGFOOD !== "true",
  "set OBSERVE_DOGFOOD=true (server-side and as test env) to verify self-monitoring",
);

async function login(api: any): Promise<string> {
  const r = await api.post(`${OBSERVE_URL}/api/v1/auth/login`, {
    data: { username: "admin", password: "observe" },
  });
  expect(r.ok()).toBeTruthy();
  return (await r.json()).token;
}

test("self-traces every API request under site_id=_meta", async () => {
  const api = await request.newContext();
  const token = await login(api);

  // Generate a recognizable trace by hitting an admin route.
  const r = await api.get(`${OBSERVE_URL}/api/v1/sites`, {
    headers: { Authorization: `Bearer ${token}` },
  });
  expect(r.ok()).toBeTruthy();

  // SDK batches spans; wait for the flush window.
  await new Promise((res) => setTimeout(res, 3500));

  const from = new Date(Date.now() - 5 * 60_000).toISOString();
  const to = new Date(Date.now() + 60_000).toISOString();
  const trace = await api.get(
    `${OBSERVE_URL}/api/v1/traces/search?site_id=_meta&from=${from}&to=${to}&limit=50`,
    { headers: { Authorization: `Bearer ${token}` } },
  );
  expect(trace.ok()).toBeTruthy();
  const rows = await trace.json();
  expect(Array.isArray(rows)).toBeTruthy();

  // Some span recorded for /api/v1/sites should exist.
  const sitesSpan = rows.find((r: any) => /\/api\/v1\/sites/.test(r.root_operation));
  expect(sitesSpan, `expected a span for GET /api/v1/sites, got: ${rows.map((r: any) => r.root_operation).join(", ")}`).toBeTruthy();

  // Loop prevention: the trace ingest path itself must never appear.
  const tracesSpan = rows.find((r: any) => /\/v1\/traces$/.test(r.root_operation));
  expect(tracesSpan, "trace ingest path must be excluded from self-tracing to prevent feedback loops").toBeFalsy();
});
