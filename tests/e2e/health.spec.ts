import { expect, test } from '@playwright/test';

test('production assets render and the real API is ready', async ({ page, request }, testInfo) => {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  page.on('console', (message) => {
    if (message.type() === 'error') errors.push(message.text());
  });
  await page.goto('/status');
  await expect(page.getByRole('heading', { name: 'Service connection' })).toBeVisible();
  await expect(page.getByText('The service is ready.')).toBeVisible();
  await expect(page.getByText('Version 2.0.0-dev.0')).toBeVisible();
  await page.getByRole('button', { name: 'Check again' }).click();
  await expect(page.getByText('The service is ready.')).toBeVisible();
  const response = await request.get('/api/v2/health/ready');
  expect(response.status()).toBe(200);
  expect(await response.json()).toEqual({ status: 'ok' });
  expect(response.headers()['cache-control']).toBe('private, no-store');
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth);
  expect(overflow).toBe(false);
  await page.screenshot({ path: testInfo.outputPath('service-connection.png'), fullPage: true });
  expect(errors).toEqual([]);
});

test('language selection persists through a real page reload', async ({ page }) => {
  await page.goto('/status');
  await page.getByRole('combobox', { name: 'Language' }).selectOption('zh-TW');
  await expect(page.getByRole('heading', { name: '服務連線' })).toBeVisible();
  await page.reload();
  await expect(page.getByText('服務已就緒。')).toBeVisible();
  await expect(page.locator('html')).toHaveAttribute('lang', 'zh-TW');
  await page.getByRole('combobox', { name: '語言' }).selectOption('zh-CN');
  await expect(page.getByRole('heading', { name: '服务连接' })).toBeVisible();
});

test('private-looking paths never fall through to static files', async ({ request }) => {
  for (const path of [
    '/api/v2/albums',
    '/api/v1/albums',
    '/.env',
    '/.gallerygo/original.jpg',
    '/data/gallerygo.db',
  ]) {
    const response = await request.get(path);
    expect(response.status()).toBe(404);
    expect(response.headers()['content-type']).toBe('application/problem+json');
    expect(await response.json()).toMatchObject({ status: 404, code: 'NOT_FOUND' });
  }
});

test('a network failure is visible and can be retried', async ({ page, context }) => {
  await page.goto('/status');
  await expect(page.getByText('The service is ready.')).toBeVisible();
  await context.setOffline(true);
  await page.getByRole('button', { name: 'Check again' }).click();
  await expect(page.getByRole('alert')).toContainText('The service could not be reached');
  await context.setOffline(false);
  await page.getByRole('button', { name: 'Check again' }).click();
  await expect(page.getByText('The service is ready.')).toBeVisible();
});
