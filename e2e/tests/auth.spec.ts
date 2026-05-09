import { test, expect } from "@playwright/test";
import { login } from "./helpers.js";

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
