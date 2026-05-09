import { test, expect } from "@playwright/test";

// Dead-click detection: a click on an element that triggers no DOM mutation,
// no navigation, and no input focus change within 1.5s should emit a
// `dead_click` event to /api/v1/events/batch. PostHog parity (gap #5).

test("tracker emits dead_click for unresponsive button", async ({ page }) => {
  // Capture every batch the tracker POSTs at /api/v1/events/batch (or /events/batch).
  const captured: any[] = [];
  await page.route("**/events/batch", async (route) => {
    try {
      const body = route.request().postData();
      if (body) {
        const parsed = JSON.parse(body);
        if (parsed && Array.isArray(parsed.events)) {
          for (const ev of parsed.events) captured.push(ev);
        }
      }
    } catch {
      // ignore non-JSON beacons
    }
    await route.fulfill({ status: 204, body: "" });
  });

  // Synthetic page that loads the real tracker against this origin.
  // Button has no handler => clicking it should mutate nothing => dead click.
  await page.goto("/");
  await page.setContent(`
    <!doctype html>
    <html>
      <head><title>dead-click fixture</title></head>
      <body>
        <button id="nope">Does nothing</button>
      </body>
    </html>
  `);

  // addScriptTag preserves document.currentScript so the tracker initializes;
  // setContent + inline <script src> can leave currentScript null.
  await page.addScriptTag({
    url: "/t/observe.js",
    // playwright addScriptTag forwards arbitrary attributes via type/content,
    // but data-* attrs need to be set on the resulting element. We work
    // around this by injecting a sibling <script> that reads from a global.
  });
  await page.waitForFunction(() => typeof (window as any).observe !== "undefined", null, { timeout: 5000 });

  await page.click("#nope");

  // Dead-click fires after the 1.5s mutation-observer window. Add slack
  // for the 500ms tracker flush interval.
  await page.waitForTimeout(2500);

  const deadClicks = captured.filter((e) => e && e.event_type === "dead_click");
  expect(deadClicks.length, `expected at least one dead_click in ${JSON.stringify(captured)}`).toBeGreaterThan(0);

  const dc = deadClicks[0];
  expect(dc.properties).toBeTruthy();
  expect(typeof dc.properties.target_selector).toBe("string");
  expect(dc.properties.target_selector).toContain("button");
});
