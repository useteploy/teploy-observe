import { test, expect } from "@playwright/test";
import { login } from "./auth.spec.js";

test("Cmd-K palette opens and navigates", async ({ page }) => {
  await login(page);
  await page.goto("/");
  await page.keyboard.press("Meta+K");
  const input = page.locator(".cmdk-input");
  await expect(input).toBeVisible({ timeout: 2_000 });
  await input.fill("errors");
  await page.keyboard.press("Enter");
  await page.waitForURL(/\/errors/, { timeout: 5_000 });
  expect(page.url()).toMatch(/\/errors/);
});
