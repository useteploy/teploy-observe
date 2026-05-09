import { test, expect } from "@playwright/test";
import { login } from "./helpers.js";

test("Cmd-K palette opens and navigates", async ({ page }) => {
  await login(page);
  await page.goto("/", { waitUntil: "networkidle" });
  // Use Control+K — handler accepts both Meta and Ctrl, and headless Chromium
  // sometimes intercepts Meta+K as a browser shortcut before it reaches the page.
  // Retry a couple of times because the keydown handler is attached in a hooks
  // useEffect that races with networkidle.
  const input = page.locator(".cmdk-input");
  await expect.poll(async () => {
    if (await input.isVisible().catch(() => false)) return true;
    await page.keyboard.press("Control+k");
    return false;
  }, { timeout: 5_000, intervals: [200, 400, 800] }).toBe(true);
  await input.fill("errors");
  await page.keyboard.press("Enter");
  await page.waitForURL(/\/errors/, { timeout: 5_000 });
  expect(page.url()).toMatch(/\/errors/);
});
