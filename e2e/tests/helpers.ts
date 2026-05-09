import { Page } from "@playwright/test";

/** Log in with default demo credentials and land on the dashboard. */
export async function login(page: Page) {
  await page.goto("/login");
  await page.fill('input[name="username"], input[type="text"]', "admin");
  await page.fill('input[type="password"]', "observe");
  await Promise.all([
    page.waitForURL((url) => url.pathname !== "/login", { timeout: 10_000 }),
    page.click('button[type="submit"], button:has-text("Sign")'),
  ]);
}
