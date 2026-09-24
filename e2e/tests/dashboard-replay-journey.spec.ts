import { test, expect, request } from "@playwright/test";
import { login } from "./helpers.js";

/**
 * O06 tail — the dashboard-to-replay journey as one continuous flow: log in,
 * walk to a session that has replay data, open the player, and assert the
 * rrweb DOM actually renders — with no console errors along the way.
 *
 * The replay is seeded through the public ingest API in setup (v2 of the
 * tracker's wrapped-rrweb shape: a Meta event plus a full snapshot carrying
 * a marker node), so the journey exercises the real ingest -> list -> fetch
 * -> sanitize -> rebuild path rather than depending on demo seed data.
 * Skipped cleanly when no instance answers at baseURL, like the other specs.
 */
let serverUp = false;
// Journey-created site, deleted in afterAll so later specs (smoke/core
// click the first session card of whatever site the UI falls back to) are
// not steered onto this journey's rrweb data.
let journeySiteId: string | null = null;
let journeyToken: string | null = null;

test.beforeAll(async ({ request }) => {
  try {
    const res = await request.get("/healthz", { timeout: 5000 });
    serverUp = res.ok();
  } catch {
    serverUp = false;
  }
  if (!serverUp) {
    test.skip(!serverUp, "no Observe instance at baseURL - see e2e/README.md to run the journey");
  }
});

test.afterAll(async ({ request }) => {
  if (!serverUp || !journeySiteId || !journeyToken) return;
  try {
    await request.delete(`/api/v1/sites/${journeySiteId}`, {
      headers: { Authorization: `Bearer ${journeyToken}` },
    });
  } catch {
    // Best-effort cleanup; a leftover site only re-orders other specs' data.
  }
});

const MARKER_TEXT = `O13 journey marker ${Date.now()}`;

test("login -> sessions -> rrweb replay renders", async ({ page, baseURL }) => {
  test.setTimeout(60_000);

  // ── Journey step 1: log in through the real form ──
  // One login per file: the server rate-limits logins per IP (10/min) and
  // parallel spec files trip it together, so the browser session's own token
  // doubles as the API credential for the seeding below.
  const consoleErrors: string[] = [];
  page.on("console", (msg) => {
    if (msg.type() !== "error") return;
    // The replay iframe is sandboxed WITHOUT allow-scripts by design
    // (recorded scripts must never execute in the operator's browser), and
    // Chromium logs each blocked execution as a console error. Those
    // messages are the security boundary working, so they are filtered;
    // anything else fails.
    if (/Blocked script execution in .* because the document's frame is sandboxed/.test(msg.text())) return;
    consoleErrors.push(msg.text());
  });
  page.on("pageerror", (err) => consoleErrors.push(String(err)));

  await login(page);
  const token = await page.evaluate(() => localStorage.getItem("obs_token"));
  expect(token, "form login stores a session token").toBeTruthy();
  journeyToken = token;

  // ── Setup (same session): seed one rrweb replay via the ingest API ──
  const api = await request.newContext({
    baseURL,
    extraHTTPHeaders: { Authorization: `Bearer ${token}` },
  });
  const auth = {}; // header already on the context

  // A dedicated site keeps the journey hermetic: seeding into "default"
  // would make this run's session the most recent card on /sessions, which
  // the smoke/core specs click expecting a demo-seeded keyframe replay.
  const JOURNEY_SITE = "o13-journey";
  const sitesRes = await api.get("/api/v1/sites");
  expect(sitesRes.ok()).toBeTruthy();
  const sites = await sitesRes.json();
  let siteId = (Array.isArray(sites) ? sites : []).find((s: any) => s.name === JOURNEY_SITE)?.site_id;
  if (!siteId) {
    const created = await api.post("/api/v1/sites", {
      // The API rejects a site without a domain; a fixed fake one identifies
      // the journey's data in every list.
      data: { name: JOURNEY_SITE, domain: "o13-journey.e2e.local" },
    });
    expect(created.ok(), `create site status=${created.status()}`).toBeTruthy();
    siteId = (await created.json()).site_id;
  }
  journeySiteId = siteId;

  // Ingest requires a site-scoped telemetry key; mint one for the seed.
  const keyRes = await api.post(`/api/v1/sites/${siteId}/keys`, { data: {} });
  expect(keyRes.ok(), `create key status=${keyRes.status()}`).toBeTruthy();
  const { key } = await keyRes.json();
  expect(key).toBeTruthy();

  // Wrapped rrweb events (what observe-replay-delta.js posts): a Meta event
  // plus a full snapshot whose body carries the marker. Serialized-node
  // numbering follows rrweb: Document=0, Element=2, Text=3 — ids are
  // required by the Replayer's node map; class is an allowlisted attribute;
  // plain text nodes survive the play-time sanitizer.
  const now = Date.now();
  let nextId = 1;
  const id = () => nextId++;
  const seedUrl = `https://e2e.local/o13-journey-${now}`;
  const ingestRes = await api.post("/api/v1/replays", {
    headers: { "X-API-Key": key, "Content-Type": "application/json" },
    data: {
      site_id: siteId,
      session_id: `o13-journey-session-${now}`,
      url: seedUrl,
      browser: "Chrome",
      os: "macOS",
      device: "desktop",
      events: [
        {
          type: "rrweb",
          timestamp: now,
          data: { type: 4, data: { href: seedUrl, width: 1280, height: 800 } },
        },
        {
          type: "rrweb",
          timestamp: now + 100,
          data: {
            type: 2,
            data: {
              node: {
                type: 0,
                id: id(),
                childNodes: [
                  {
                    type: 2,
                    id: id(),
                    tagName: "html",
                    attributes: {},
                    childNodes: [
                      { type: 2, id: id(), tagName: "head", attributes: {}, childNodes: [] },
                      {
                        type: 2,
                        id: id(),
                        tagName: "body",
                        attributes: {},
                        childNodes: [
                          {
                            type: 2,
                            id: id(),
                            tagName: "div",
                            attributes: { class: "o13-marker" },
                            childNodes: [
                              { type: 3, id: id(), textContent: MARKER_TEXT, isRoot: false },
                            ],
                          },
                        ],
                      },
                    ],
                  },
                ],
              },
            },
          },
        },
      ],
    },
  });
  expect(ingestRes.ok(), `replay ingest status=${ingestRes.status()}`).toBeTruthy();
  await api.dispose();

  // ── Journey: walk the UI to the seeded replay ──
  await page.goto(`/sessions?site_id=${encodeURIComponent(siteId)}`);
  const card = page.locator(".sessions-card", { hasText: "o13-journey" }).first();
  await expect(card, "seeded session should appear in /sessions").toBeVisible({ timeout: 15_000 });
  await card.click();
  await expect(page.locator(".sessions-detail-header")).toBeVisible();

  const play = page.locator(".sessions-play-btn");
  await expect(play).toBeEnabled();
  await play.click();
  await expect(page.locator(".replay-overlay")).toBeVisible();

  // rrweb mode: the player mounts the Replayer into .replay-rrweb-root and
  // it creates its own iframe; the rebuilt DOM must contain the marker.
  const rrwebRoot = page.locator(".replay-rrweb-root");
  await expect(rrwebRoot).toBeVisible({ timeout: 15_000 });
  const replayFrame = page.frameLocator(".replay-rrweb-root iframe");
  await expect(
    replayFrame.locator(`text=${MARKER_TEXT}`),
    "rebuilt rrweb DOM should contain the seeded marker",
  ).toBeVisible({ timeout: 15_000 });

  expect(consoleErrors, "no console errors during the journey").toEqual([]);
});
