import { test, expect, request } from "@playwright/test";
import { login } from "./helpers.js";

// Sentry parity (W4.8) — replay <-> issue cross-jump.
//
// Part A asserts the existing /api/v1/errors ingest endpoint accepts the
// replay_id field, that the errors UI surfaces a "View replay" button when
// an event has a replay_id, and that clicking through deep-links into the
// sessions page with ?replay_id=<id>.
//
// Part B asserts the same ingest endpoint accepts a synthetic RageClick
// payload (the auto-issue path the replay tracker fires for unresponsive
// click bursts). Driving the actual browser-side rage-click detector is
// fragile; the tracker contract is the API call, so we assert that here.

const SEED_REPLAY_ID = "seed-replay-1";
const REPLAY_ERROR_TITLE = "ReplayLinkSeed: deep link probe";

test.describe("replay <-> issue linking", () => {
  test.beforeEach(async ({ page, baseURL }) => {
    // Seed a single error tied to a known replay_id via the public ingest
    // endpoint (no auth required for /api/v1/errors).
    const api = await request.newContext({ baseURL });
    const res = await api.post("/api/v1/errors", {
      data: {
        site_id: "default",
        replay_id: SEED_REPLAY_ID,
        error_type: "ReplayLinkSeed",
        error_value: "deep link probe",
        level: "error",
        url: "https://example.com/replay-link-test",
        stack_trace: [
          { filename: "/app/seed.js", function: "seed", lineno: 1, colno: 1, in_app: true },
        ],
      },
    });
    expect(res.ok(), `seed POST status=${res.status()}`).toBeTruthy();
    await api.dispose();
    // The errors ingest path is buffered (see internal/errors/buffer.go);
    // wait one flush cycle (2s) plus slack for the issues query to see it.
    await page.waitForTimeout(3500);

    await login(page);
  });

  test("issue detail surfaces View replay button and deep-links to sessions", async ({ page }) => {
    await page.goto("/errors", { waitUntil: "networkidle" });

    // Find the seeded issue row and click into it.
    const row = page.locator(".errors-issue-row", { hasText: REPLAY_ERROR_TITLE }).first();
    await expect(row, "seeded ReplayLinkSeed issue should appear in /errors").toBeVisible({
      timeout: 10_000,
    });
    await row.click();

    // The View replay button is rendered when the selected event has a
    // replay_id — assert it's present and points at /sessions?replay_id=...
    const viewReplay = page.getByTestId("view-replay");
    await expect(viewReplay, "View replay button should render for an event with replay_id").toBeVisible({
      timeout: 10_000,
    });
    const href = await viewReplay.getAttribute("href");
    expect(href).toBeTruthy();
    expect(href!).toContain(`replay_id=${SEED_REPLAY_ID}`);

    // Follow the link and confirm the sessions page hydrates with the deep
    // replay id in the URL.
    await viewReplay.click();
    await page.waitForLoadState("networkidle");
    expect(page.url()).toContain(`replay_id=${SEED_REPLAY_ID}`);
  });

  test("ingest endpoint accepts a RageClick auto-issue payload", async ({ baseURL }) => {
    // Mirrors what observe-replay.js POSTs from its rage-click handler.
    const api = await request.newContext({ baseURL });
    const res = await api.post("/api/v1/errors", {
      data: {
        site_id: "default",
        replay_id: SEED_REPLAY_ID,
        error_type: "RageClick",
        error_value: "User clicked 5 times on button#submit without progress",
        mechanism: "rage_click",
        handled: false,
        level: "warning",
        url: "https://example.com/checkout",
        selector: "button#submit",
        stack_trace: [
          { filename: "/checkout", function: "button#submit", in_app: true },
        ],
      },
    });
    expect(res.ok(), `RageClick POST status=${res.status()}`).toBeTruthy();
    const body = await res.json();
    expect(body.ok).toBe(true);
    await api.dispose();
  });
});
