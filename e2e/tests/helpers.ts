import { Page } from "@playwright/test";

/** Log in with the instance's admin credentials and land on the dashboard.
 * Defaults match the CI smoke boot (OBSERVE_ADMIN_PASSWORD=observe-e2e-pass;
 * "observe" alone is 7 chars and under the server's 8-char password floor).
 * Override via OBSERVE_ADMIN_USER / OBSERVE_ADMIN_PASSWORD. */
export async function login(page: Page, username?: string, password?: string) {
  const user = username ?? process.env.OBSERVE_ADMIN_USER ?? "admin";
  const pass = password ?? process.env.OBSERVE_ADMIN_PASSWORD ?? "observe-e2e-pass";
  await page.goto("/login");
  await page.fill('input[name="username"], input[type="text"]', user);
  await page.fill('input[type="password"]', pass);
  await Promise.all([
    page.waitForURL((url) => url.pathname !== "/login", { timeout: 10_000 }),
    page.click('button[type="submit"], button:has-text("Sign")'),
  ]);
}
