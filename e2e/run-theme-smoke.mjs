/* One-off UI smoke for the theme-move change. Not part of the suite: it runs
 * against a plain static server with a faked token, so every API call fails —
 * fine, because everything under test is client-side. Run:
 *   node run-theme-smoke.mjs
 */
import { chromium } from "@playwright/test";

const BASE = "http://127.0.0.1:8931";
const results = [];
const check = (name, ok, extra = "") => {
  results.push({ name, ok, extra });
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${extra ? ` — ${extra}` : ""}`);
};

const browser = await chromium.launch();

// ─── Desktop ───
{
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  await ctx.addInitScript(() => localStorage.setItem("obs_token", "smoke-fake"));
  const page = await ctx.newPage();
  await page.goto(BASE + "/", { waitUntil: "domcontentloaded" });
  await page.waitForTimeout(1200);

  const sidebar = page.locator("nav.obs-sidebar");
  check("desktop: sidebar renders", await sidebar.count() === 1);
  // SiteSwitcher/ProductSwitcher legitimately render buttons in the sidebar —
  // the check is that no theme toggle remains.
  const sidebarBtnTexts = await sidebar.locator("button").allTextContents();
  check("desktop: no theme button left in sidebar",
    !sidebarBtnTexts.some(t => /light mode|dark mode/i.test(t)) &&
    (await page.getByText("Light Mode").count()) === 0,
    `sidebar buttons: ${JSON.stringify(sidebarBtnTexts)}`);
  check("desktop: mobile menu trigger hidden", !(await page.locator(".cmdk-mobile-trigger").isVisible()));

  // Navigate to /settings client-side (static server can't serve the path).
  await page.evaluate(() => {
    history.pushState(null, "", "/settings");
    window.dispatchEvent(new PopStateEvent("popstate"));
  });
  await page.waitForTimeout(600);
  const appearanceTab = page.getByRole("tab", { name: "Appearance" });
  check("desktop: Settings shows Appearance tab", await appearanceTab.count() === 1, await page.title());
  await appearanceTab.click();
  await page.waitForTimeout(300);

  const lightBtn = page.getByRole("button", { name: /^Light/ });
  const darkBtn = page.getByRole("button", { name: /^Dark/ });
  check("desktop: Appearance section shows both options",
    await lightBtn.isVisible() && await darkBtn.isVisible());

  await lightBtn.click();
  await page.waitForTimeout(300);
  check("desktop: Light applies data-theme", await page.evaluate(() =>
    document.documentElement.getAttribute("data-theme")) === "light");
  check("desktop: Light persists to localStorage", await page.evaluate(() =>
    localStorage.getItem("obs_theme")) === "light");
  const bgLight = await page.evaluate(() =>
    getComputedStyle(document.querySelector(".obs-main-content").parentElement).backgroundColor);

  // Reload: pre-paint script must hold the theme.
  await page.reload({ waitUntil: "domcontentloaded" });
  const attrEarly = await page.evaluate(() => document.documentElement.getAttribute("data-theme"));
  await page.waitForTimeout(800);
  check("desktop: reload keeps light theme", attrEarly === "light" &&
    (await page.evaluate(() => document.documentElement.getAttribute("data-theme"))) === "light");
  check("desktop: light background actually differs",
    /^rgb\((2[0-9]{2}|1[5-9][0-9])/i.test(bgLight), `layout bg in light = ${bgLight}`);

  // Back to dark via the section (reload landed us on /settings again in URL only;
  // re-navigate client-side).
  await page.evaluate(() => {
    history.pushState(null, "", "/settings");
    window.dispatchEvent(new PopStateEvent("popstate"));
  });
  await page.waitForTimeout(500);
  await page.getByRole("tab", { name: "Appearance" }).click();
  await page.waitForTimeout(200);
  await darkBtn.click();
  await page.waitForTimeout(200);
  check("desktop: Dark removes attribute + persists",
    (await page.evaluate(() => document.documentElement.getAttribute("data-theme"))) === null &&
    (await page.evaluate(() => localStorage.getItem("obs_theme"))) === "dark");
  await ctx.close();
}

// ─── Mobile (390px — below the 480px sidebar collapse) ───
{
  const ctx = await browser.newContext({
    viewport: { width: 390, height: 844 },
    isMobile: true, hasTouch: true,
    userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1",
  });
  await ctx.addInitScript(() => localStorage.setItem("obs_token", "smoke-fake"));
  const page = await ctx.newPage();
  await page.goto(BASE + "/", { waitUntil: "domcontentloaded" });
  await page.waitForTimeout(1000);

  // Main's mobile shell keeps the sidebar at width ~280px and slides it
  // off-canvas with transform+visibility when closed, so the assertion is
  // that the closed drawer sits fully outside the viewport.
  const closedRight = await page.evaluate(() =>
    Math.round(document.querySelector("nav.obs-sidebar").getBoundingClientRect().right));
  check("mobile: sidebar is collapsed away", closedRight <= 0, `closed drawer right edge = ${closedRight}px`);

  // Main's mobile shell: top bar + off-canvas drawer.
  const toggle = page.locator(".obs-mobile-nav-toggle");
  check("mobile: drawer toggle visible", await toggle.isVisible());
  await toggle.tap();
  await page.waitForTimeout(400);
  check("mobile: drawer opens", await page.locator("nav.obs-sidebar.obs-sidebar--open").isVisible());
  await page.locator('nav.obs-sidebar a[href="/settings"]').tap();
  await page.waitForTimeout(800);
  check("mobile: landed on settings", page.url().includes("/settings"));

  await page.getByRole("tab", { name: "Appearance" }).tap();
  await page.waitForTimeout(300);
  await page.getByRole("button", { name: /^Light/ }).tap();
  await page.waitForTimeout(300);
  check("mobile: theme switch works",
    (await page.evaluate(() => document.documentElement.getAttribute("data-theme"))) === "light" &&
    (await page.evaluate(() => localStorage.getItem("obs_theme"))) === "light");
  await ctx.close();
}

await browser.close();
const fails = results.filter(r => !r.ok);
console.log(`\n${results.length - fails.length}/${results.length} passed`);
process.exit(fails.length ? 1 : 0);
