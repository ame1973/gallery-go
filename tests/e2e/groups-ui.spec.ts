import { randomUUID } from 'node:crypto';
import { expect, signIn } from './fixtures';
import { isolatedTest as test } from './isolated-fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('regular accounts cannot browse administrative groups', async ({ account, page, request }) => {
  expect((await request.get('/api/v2/admin/groups')).status()).toBe(401);
  await page.goto('/admin/groups');
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
  await signIn(page, account);
  const requests: string[] = [];
  page.on('request', (entry) => {
    if (new URL(entry.url()).pathname.startsWith('/api/v2/admin/groups'))
      requests.push(entry.url());
  });
  await page.goto('/admin/groups');
  await expect(page.getByText('Administrator access is required to view groups.')).toBeVisible();
  await expect(page.getByRole('link', { name: 'Groups', exact: true })).toHaveCount(0);
  expect(requests).toEqual([]);
});

test('administrators browse identity-bound group pages over HTTPS', async ({
  account,
  administrator,
  page,
}) => {
  test.setTimeout(90_000);
  const name = `Groups directory ${randomUUID()}`;
  const created = await administrator.api.post('/api/v2/admin/groups', {
    headers: { ...administrator.headers, 'Idempotency-Key': randomUUID() },
    data: { name },
  });
  expect(created.status()).toBe(201);
  const group = (await created.json()) as components['schemas']['Group'];
  const groupPath = `/api/v2/admin/groups/${group.id}`;
  try {
    const accountPath = `/api/v2/admin/users/${account.user.id}`;
    const current = await administrator.api.get(accountPath);
    expect(current.status()).toBe(200);
    const promoted = await administrator.api.patch(accountPath, {
      headers: { ...administrator.headers, 'If-Match': current.headers().etag! },
      data: { role: 'admin' },
    });
    expect(promoted.status()).toBe(200);
    await signIn(page, account);
    const me = await page.request.get('/api/v2/auth/me');
    expect(me.status()).toBe(200);
    const actor = (await me.json()) as components['schemas']['User'];
    const requests: { method: string; user: string | undefined; scope: string | undefined }[] = [];
    page.on('request', (entry) => {
      if (new URL(entry.url()).pathname.startsWith('/api/v2/admin/groups')) {
        const headers = entry.headers();
        requests.push({
          method: entry.method(),
          user: headers['x-gallerygo-expected-user'],
          scope: headers['x-gallerygo-expected-scope'],
        });
      }
    });
    await page
      .getByRole('navigation', { name: 'Gallery navigation' })
      .getByRole('link', { name: 'Groups' })
      .click();
    await expect(page.getByRole('heading', { name: 'Groups', exact: true })).toBeVisible();
    const groupLink = page.getByRole('link', { name: `View group ${name}` });
    await expect(groupLink).toBeVisible();
    for (const [width, height] of [
      [375, 812],
      [768, 1024],
      [1280, 800],
    ] as const) {
      await page.setViewportSize({ width, height });
      expect(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)).toBe(
        false,
      );
      const box = await groupLink.boundingBox();
      expect(box?.width).toBeGreaterThanOrEqual(44);
      expect(box?.height).toBeGreaterThanOrEqual(44);
    }
    await page.getByRole('combobox', { name: 'Language' }).selectOption('zh-TW');
    await expect(page.getByRole('heading', { name: '群組', exact: true })).toBeVisible();
    await page.getByRole('combobox', { name: '語言' }).selectOption('zh-CN');
    await expect(page.getByRole('heading', { name: '群组', exact: true })).toBeVisible();
    await page.getByRole('combobox', { name: '语言' }).selectOption('en');
    await groupLink.click();
    await expect(page.getByRole('heading', { name, exact: true })).toBeVisible();
    await expect(page.getByRole('link', { name: 'Back to groups' })).toHaveAttribute(
      'href',
      '/admin/groups',
    );
    await expect(page.getByText(group.id, { exact: true })).toBeVisible();

    const isGroup = (url: URL) => url.pathname === groupPath;
    await page.route(isGroup, async (route) => {
      const response = await route.fetch();
      const headers = { ...response.headers() };
      delete headers['x-gallerygo-expected-scope'];
      await route.fulfill({ response, headers });
    });
    try {
      await page.getByRole('button', { name: 'Refresh' }).click();
      await expect(
        page.getByText(
          'The current account changed or could not be verified. Refresh the session and try again.',
        ),
      ).toBeVisible();
      await expect(page.getByRole('heading', { name, exact: true })).toHaveCount(0);
    } finally {
      await page.unrouteAll({ behavior: 'wait' });
    }
    await page.getByRole('alert').getByRole('button', { name: 'Refresh' }).click();
    await expect(page.getByRole('heading', { name, exact: true })).toBeVisible();
    expect(requests.length).toBeGreaterThanOrEqual(3);
    for (const request of requests) {
      expect(request).toEqual({ method: 'GET', user: actor.id, scope: actor.scope_id });
    }
  } finally {
    const latest = await administrator.api.get(groupPath);
    if (latest.status() !== 404) {
      expect(latest.status()).toBe(200);
      const removed = await administrator.api.delete(groupPath, {
        headers: { ...administrator.headers, 'If-Match': latest.headers().etag! },
      });
      expect(removed.status()).toBe(204);
    }
  }
});
