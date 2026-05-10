import { test, expect, request } from "@playwright/test";
import { login } from "./helpers.js";

// A2: the issue search must return 200 (and not 500) on a fresh install
// where the FTS index could be empty, AND it must surface the seeded
// errors planted by `internal/seed/seed.go::seedErrors`. Pre-refactor the
// seed inserted directly into error_events without calling IndexError;
// search returned 500 on every fresh install. Funneling the seed through
// `errors.Service.IngestErrorEvent` fixed the root cause — this test pins
// the contract.

const OBSERVE_URL = process.env.OBSERVE_URL || "http://127.0.0.1:3000";
const SITE = "default";

async function loginToken(api: any): Promise<string> {
  const r = await api.post(`${OBSERVE_URL}/api/v1/auth/login`, {
    data: { username: "admin", password: "observe" },
  });
  expect(r.ok(), `login failed: ${r.status()} ${await r.text()}`).toBeTruthy();
  return (await r.json()).token;
}

test.describe("error search (FTS) on seeded data", () => {
  test("GET /api/v1/issues/search returns 200 with results for a seeded query", async () => {
    const api = await request.newContext();
    const token = await loginToken(api);
    const headers = { Authorization: `Bearer ${token}` };

    // The seedErrors step plants three demo errors, including a NetworkError
    // whose value is "Connection timed out after 30s". A search for
    // "Connection" should hit at least one issue.
    const r = await api.get(
      `${OBSERVE_URL}/api/v1/issues/search?site_id=${SITE}&q=Connection`,
      { headers },
    );

    expect(
      r.status(),
      `search returned ${r.status()} — body: ${await r.text()}`,
    ).toBe(200);

    const body = await r.json();
    expect(Array.isArray(body), "response must be a JSON array").toBeTruthy();
    expect(body.length, "expected at least one Connection issue from seed").toBeGreaterThanOrEqual(1);

    // Sanity: every returned row has the issue shape the UI expects.
    for (const row of body) {
      expect(typeof row.issue_id).toBe("string");
      expect(typeof row.title).toBe("string");
      expect(row.title).toContain("Connection");
    }
  });

  test("GET /api/v1/issues/search returns 200 with [] for a no-match query (no FTS 500)", async () => {
    const api = await request.newContext();
    const token = await loginToken(api);
    const headers = { Authorization: `Bearer ${token}` };

    // A token that's astronomically unlikely to be in any indexed text.
    const r = await api.get(
      `${OBSERVE_URL}/api/v1/issues/search?site_id=${SITE}&q=zzzz_no_match_${Date.now()}`,
      { headers },
    );

    expect(r.status()).toBe(200);
    const raw = await r.text();
    expect(raw.trim(), "no-match must be \"[]\", not \"null\"").not.toBe("null");
    expect(Array.isArray(JSON.parse(raw))).toBeTruthy();
  });
});
