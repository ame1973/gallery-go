import { expect, test } from '@playwright/test';
import type { Page } from '@playwright/test';

const username = 'e2e-owner';
const password = process.env.GALLERYGO_TEST_PASSWORD;
if (!password) throw new Error('Missing generated test password');
const secret: string = password;

async function login(page: Page, value = secret) {
  await page.goto('/login');
  await page.getByLabel('Username', { exact: true }).fill(username);
  await page.getByLabel('Password', { exact: true }).fill(value);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Account security' })).toBeVisible();
}

async function updatePassword(page: Page, current: string, next: string) {
  await page.getByLabel('Current password', { exact: true }).fill(current);
  await page.getByLabel('New password', { exact: true }).fill(next);
  await page.getByLabel('Confirm new password', { exact: true }).fill(next);
  await page.getByRole('button', { name: 'Change password', exact: true }).click();
  await expect(
    page.getByText('Password changed. Other sessions have been signed out.'),
  ).toBeVisible();
}

test('HTTPS login, session revocation, password rotation, and logout work together', async ({
  page,
  context,
  browser,
}, testInfo) => {
  const violations: string[] = [];
  page.on('pageerror', (error) => violations.push(error.message));
  page.on('console', (message) => {
    if (message.text().includes('Content Security Policy')) violations.push(message.text());
  });
  await login(page);
  const cookies = await context.cookies();
  const session = cookies.find((cookie) => cookie.name === '__Host-gallerygo_session');
  expect(session).toMatchObject({ secure: true, httpOnly: true, sameSite: 'Lax', path: '/' });
  expect(await page.evaluate(() => document.cookie)).not.toContain(session!.value);
  const second = await browser.newContext({
    baseURL: 'https://127.0.0.1:4318',
    ignoreHTTPSErrors: true,
  });
  try {
    const otherPage = await second.newPage();
    await login(otherPage);
    await page.reload();
    const revoke = page.getByRole('button', { name: 'Revoke session', exact: true });
    await revoke.click();
    const dialog = page.getByRole('alertdialog');
    await expect(dialog).toBeVisible();
    await expect(dialog.getByRole('button', { name: 'Cancel' })).toBeFocused();
    await dialog.getByRole('button', { name: 'Cancel' }).click();
    await expect(revoke).toBeFocused();
    await revoke.click();
    await dialog.getByRole('button', { name: 'Revoke session', exact: true }).click();
    await expect(page.getByText('Session revoked.')).toBeVisible();
    await otherPage.reload();
    await expect(otherPage.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
    await login(otherPage);
    const replacement = secret + '-changed';
    let changed = false;
    try {
      await updatePassword(page, secret, replacement);
      changed = true;
      const after = (await context.cookies()).find(
        (cookie) => cookie.name === '__Host-gallerygo_session',
      );
      expect(after!.value).not.toBe(session!.value);
      await otherPage.reload();
      await expect(otherPage.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
      await page.screenshot({ path: testInfo.outputPath('account-security.png'), fullPage: true });
    } finally {
      if (changed) await updatePassword(page, replacement, secret);
    }
    await page.getByRole('button', { name: 'Sign out', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
    expect(
      (await context.cookies()).some((cookie) => cookie.name === '__Host-gallerygo_session'),
    ).toBe(false);
    const keys = await page.evaluate(() => Object.keys(localStorage));
    expect(keys).toEqual(['gallerygo:locale']);
    expect(violations).toEqual([]);
  } finally {
    await second.close();
  }
});

test('anonymous access, validation, and translated login errors are explicit', async ({ page }) => {
  await page.goto('/settings/security');
  await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.getByRole('alert')).toHaveCount(2);
  await page.getByRole('combobox', { name: 'Language' }).selectOption('zh-TW');
  await page.getByLabel('使用者名稱').fill(username);
  await page.getByLabel('密碼', { exact: true }).fill('an incorrect password');
  await page.getByRole('button', { name: '登入', exact: true }).click();
  await expect(page.getByText('使用者名稱或密碼不正確。')).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)).toBe(false);
});

test('a real HTTPS session cannot mutate without same-origin CSRF', async ({ page, context }) => {
  await login(page);
  const missing = await context.request.post('/api/v2/auth/logout', {
    headers: { Origin: 'https://127.0.0.1:4318' },
  });
  expect(missing.status()).toBe(403);
  expect(await missing.json()).toMatchObject({ code: 'CSRF_REJECTED' });
  const csrf = await context.request.get('/api/v2/auth/csrf');
  const token = (await csrf.json()) as { token: string };
  const crossSite = await context.request.post('/api/v2/auth/logout', {
    headers: { Origin: 'https://untrusted.example', 'X-CSRF-Token': token.token },
  });
  expect(crossSite.status()).toBe(403);
  expect(await crossSite.json()).toMatchObject({ code: 'ORIGIN_REJECTED' });
  expect((await context.request.get('/api/v2/auth/me')).status()).toBe(200);
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
});
