import { test, expect } from '@playwright/test';

const sites = [
  { site_id: 'default', name: 'Default Site', domain: 'localhost' },
  { site_id: 'fylun', name: 'Fylun', domain: 'fylun.ai' },
  { site_id: 'teploy', name: 'Teploy', domain: 'teploy.com' },
];

test.beforeEach(async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('obs_token', 'site-selection-test'));
  await page.route('**/api/**', route => route.fulfill({ json: [] }));
  await page.route('**/api/v1/sites', route => route.fulfill({ json: sites }));
});

test('fresh browser selects a configured site before persisting the fallback', async ({ page }) => {
  await page.goto('/');
  await expect(page).toHaveURL(/site_id=fylun/);
  expect(await page.evaluate(() => localStorage.getItem('observe.site_id'))).toBe('fylun');
});

test('URL selection wins over remembered site, including explicit default', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('observe.site_id', 'teploy'));
  await page.goto('/?site_id=default');
  await expect(page.locator('.obs-site-switcher')).toContainText('Default Site');
  expect(await page.evaluate(() => localStorage.getItem('observe.site_id'))).toBe('default');
});

test('remembered site wins over initial discovery', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('observe.site_id', 'teploy'));
  await page.goto('/');
  await expect(page).toHaveURL(/site_id=teploy/);
});

test('bootstrap-only installation keeps default available', async ({ page }) => {
  await page.route('**/api/v1/sites', route => route.fulfill({ json: [sites[0]] }));
  await page.goto('/');
  await expect(page).toHaveURL(/site_id=default/);
});
