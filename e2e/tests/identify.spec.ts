import { test, expect, request } from "@playwright/test";
import { login } from "./helpers.js";

// A3: SDK identify() flows distinct_id end to end. POST an event with
// distinct_id; assert the row landed in events with the value HASHED
// (not the raw user id we sent). This pins both:
//   - the ingest handler accepts distinct_id and stores it,
//   - the privacy contract — raw IDs never persist by default; the
//     server hashes with the per-site session_salt before INSERT.
//
// Per-site opt-out via raw_distinct_id is documented but not exercised
// here — that's a Wave 4 concern when the persons UI lands.

const OBSERVE_URL = process.env.OBSERVE_URL || "http://127.0.0.1:3000";
const SITE = "default";
const RAW_USER_ID = "user-12345-identify-spec";

async function adminToken(api: any): Promise<string> {
  const r = await api.post(`${OBSERVE_URL}/api/v1/auth/login`, {
    data: { username: "admin", password: "observe" },
  });
  expect(r.ok()).toBeTruthy();
  return (await r.json()).token;
}

test.describe("SDK identify() distinct_id ingestion", () => {
  test("event ingest accepts distinct_id and stores the hashed value", async () => {
    const api = await request.newContext();

    // 1. POST an event with a raw distinct_id. The events ingest endpoint
    //    is publicly callable (browser SDKs hit it without auth), but it
    //    requires a registered site_id. We use the default site.
    const eventType = "identify_spec_pageview_" + Date.now();
    const post = await api.post(`${OBSERVE_URL}/api/v1/events`, {
      data: {
        site_id: SITE,
        event_type: eventType,
        url: "https://test.local/identify-spec",
        distinct_id: RAW_USER_ID,
      },
    });
    expect(
      post.ok(),
      `events POST ${post.status()} body: ${await post.text()}`,
    ).toBeTruthy();

    // 2. Wait one buffer flush (default 2s) plus a slack window so the
    //    row lands in the events table before we query.
    await new Promise((r) => setTimeout(r, 3500));

    // 3. Query the SQL explorer (admin-auth) to read the row back and
    //    confirm distinct_id is the HASHED value.
    const token = await adminToken(api);
    const headers = { Authorization: `Bearer ${token}` };

    const exp = await api.post(`${OBSERVE_URL}/api/v1/query`, {
      headers,
      data: {
        sql: `SELECT distinct_id FROM events WHERE event_type = '${eventType}' LIMIT 1`,
      },
    });
    expect(
      exp.ok(),
      `explorer query failed: ${exp.status()} ${await exp.text()}`,
    ).toBeTruthy();
    const body = await exp.json();
    const rows: any[] = body.rows ?? [];
    expect(rows.length, "expected at least one event row").toBeGreaterThanOrEqual(1);

    // Explorer rows come back as either {col: val} maps or [val] arrays
    // depending on the encoder; tolerate both.
    const first = rows[0];
    const stored: string =
      typeof first === "object" && !Array.isArray(first)
        ? (first.distinct_id ?? "")
        : Array.isArray(first)
          ? String(first[0] ?? "")
          : "";

    expect(stored, "distinct_id must be persisted").not.toBe("");
    expect(stored, "distinct_id must be hashed, not the raw user id").not.toBe(
      RAW_USER_ID,
    );
    // The privacy contract pins length to 16 hex chars (matches session_id).
    expect(stored.length, `stored=${stored}`).toBe(16);
    expect(/^[0-9a-f]+$/.test(stored), `stored=${stored}`).toBeTruthy();
  });
});
