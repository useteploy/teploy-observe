import { test, expect, request } from "@playwright/test";

// T002: every list endpoint must return `[]` (never `null`) on empty results,
// and a parseable JSON array when populated. See docs/api-shape-convention.md.
//
// We hit each endpoint twice when applicable:
//   - with a guaranteed-empty site_id (`__t002_empty__`) to force the
//     nucleus.Query nil-slice path,
//   - with the real `default` site_id to confirm the populated shape.
//
// A `null` body would crash UI code that does `data.map(...)` — that is the
// regression we are guarding against.

const OBSERVE_URL = process.env.OBSERVE_URL || "http://localhost:3000";
const EMPTY_SITE = "__t002_empty__";
const REAL_SITE = "default";

async function login(api: any): Promise<string> {
  const r = await api.post(`${OBSERVE_URL}/api/v1/auth/login`, {
    data: { username: "admin", password: "observe" },
  });
  expect(r.ok(), `login failed: ${r.status()} ${await r.text()}`).toBeTruthy();
  return (await r.json()).token;
}

const NOW = Date.now();
const FROM = new Date(NOW - 24 * 60 * 60_000).toISOString();
const TO = new Date(NOW + 60_000).toISOString();

// 12 representative list endpoints spanning every major surface.
const LIST_ENDPOINTS: { name: string; url: (site: string) => string }[] = [
  { name: "issues",           url: (s) => `/api/v1/issues?site_id=${s}` },
  { name: "issues.daily",     url: (s) => `/api/v1/issues/daily?site_id=${s}&days=7` },
  { name: "logs.search",      url: (s) => `/api/v1/logs/search?site_id=${s}&from=${FROM}&to=${TO}` },
  { name: "logs.histogram",   url: (s) => `/api/v1/logs/histogram?site_id=${s}&from=${FROM}&to=${TO}&bucket_ms=3600000` },
  { name: "traces.services",  url: (s) => `/api/v1/traces/services?site_id=${s}&from=${FROM}&to=${TO}` },
  { name: "traces.search",    url: (s) => `/api/v1/traces/search?site_id=${s}&from=${FROM}&to=${TO}&limit=50` },
  { name: "traces.deps",      url: (s) => `/api/v1/traces/dependencies?site_id=${s}&from=${FROM}&to=${TO}` },
  { name: "monitors",         url: (s) => `/api/v1/monitors?site_id=${s}` },
  { name: "crons",            url: (s) => `/api/v1/crons?site_id=${s}` },
  { name: "dashboards",       url: (s) => `/api/v1/dashboards?site_id=${s}` },
  { name: "flags",            url: (s) => `/api/v1/flags?site_id=${s}` },
  { name: "platform.alerts",  url: (s) => `/api/v1/platform/alerts/rules?site_id=${s}` },
];

test.describe("API shape convention: list endpoints return [] not null", () => {
  test("every list endpoint returns [] for an empty site (no null bodies)", async () => {
    const api = await request.newContext();
    const token = await login(api);
    const headers = { Authorization: `Bearer ${token}` };

    const failures: string[] = [];

    for (const { name, url } of LIST_ENDPOINTS) {
      const target = `${OBSERVE_URL}${url(EMPTY_SITE)}`;
      const r = await api.get(target, { headers });

      if (!r.ok()) {
        failures.push(`${name}: HTTP ${r.status()} (${await r.text()})`);
        continue;
      }

      const raw = await r.text();
      if (raw.trim() === "null") {
        failures.push(`${name}: body is literal "null" — must be "[]"`);
        continue;
      }

      let body: unknown;
      try {
        body = JSON.parse(raw);
      } catch (e) {
        failures.push(`${name}: body is not JSON: ${raw.slice(0, 120)}`);
        continue;
      }

      if (!Array.isArray(body)) {
        failures.push(`${name}: body is not an array (typeof=${typeof body}): ${raw.slice(0, 120)}`);
        continue;
      }

      if (body.length !== 0) {
        // Empty site shouldn't have data; not fatal but worth noting.
        // Don't fail — Observe self-monitoring may have planted rows.
      }
    }

    expect(failures, `shape violations:\n  ${failures.join("\n  ")}`).toHaveLength(0);
  });

  test("populated list endpoints return parseable arrays for real site", async () => {
    const api = await request.newContext();
    const token = await login(api);
    const headers = { Authorization: `Bearer ${token}` };

    const failures: string[] = [];

    for (const { name, url } of LIST_ENDPOINTS) {
      const target = `${OBSERVE_URL}${url(REAL_SITE)}`;
      const r = await api.get(target, { headers });

      if (!r.ok()) {
        failures.push(`${name}: HTTP ${r.status()}`);
        continue;
      }

      const raw = await r.text();
      if (raw.trim() === "null") {
        failures.push(`${name}: real-site body is literal "null"`);
        continue;
      }

      let body: unknown;
      try {
        body = JSON.parse(raw);
      } catch {
        failures.push(`${name}: real-site body is not JSON`);
        continue;
      }

      if (!Array.isArray(body)) {
        failures.push(`${name}: real-site body is not an array (typeof=${typeof body})`);
        continue;
      }

      // If populated, every row must be an object (not a primitive).
      for (let i = 0; i < Math.min(body.length, 3); i++) {
        if (typeof body[i] !== "object" || body[i] === null) {
          failures.push(`${name}: row[${i}] is not an object: ${JSON.stringify(body[i])}`);
          break;
        }
      }
    }

    expect(failures, `shape violations:\n  ${failures.join("\n  ")}`).toHaveLength(0);
  });

  test("sites endpoint (Empty input handler) also returns []", async () => {
    // /api/v1/sites takes neutron.Empty input — distinct code path worth
    // covering since it can't fail on missing site_id.
    const api = await request.newContext();
    const token = await login(api);
    const r = await api.get(`${OBSERVE_URL}/api/v1/sites`, {
      headers: { Authorization: `Bearer ${token}` },
    });
    expect(r.ok()).toBeTruthy();
    const raw = await r.text();
    expect(raw.trim()).not.toBe("null");
    const body = JSON.parse(raw);
    expect(Array.isArray(body)).toBeTruthy();
  });
});
