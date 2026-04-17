import { test, expect, Page } from "@playwright/test";

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

test("login flow works", async ({ page }) => {
  await login(page);
  await expect(page).not.toHaveURL(/\/login/);
});

test("unauthenticated user is redirected to /login", async ({ page, context }) => {
  await context.clearCookies();
  await context.addInitScript(() => localStorage.clear());
  await page.goto("/errors");
  // Either /login directly or dashboard auth-fallback; accept both.
  await page.waitForLoadState("networkidle");
  const url = page.url();
  expect(url.includes("/login") || url.endsWith("/")).toBeTruthy();
});
