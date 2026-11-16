import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { Account, Administrator } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

async function changeRole(administrator: Administrator, account: Account, role: 'admin' | 'user') {
  const path = `/api/v2/admin/users/${account.user.id}`;
  const current = await administrator.api.get(path);
  expect(current.status()).toBe(200);
  const changed = await administrator.api.patch(path, {
    headers: { ...administrator.headers, 'If-Match': current.headers().etag! },
    data: { role },
  });
  expect(changed.status()).toBe(200);
  return (await changed.json()) as Account['user'];
}

test('administrators inspect registered sources over HTTPS', async ({
  account,
  administrator,
  page,
  request,
}, testInfo) => {
  test.setTimeout(90_000);
  const name = `browse-ui-${testInfo.project.name}`;
  const id = `${name}-source`;
  const apiPath = `/api/v2/admin/sources/${id}`;
  const mutations: string[] = [];
  const errors: string[] = [];
  page.on('request', (entry) => {
    if (
      new URL(entry.url()).pathname.startsWith('/api/v2/admin/sources') &&
      entry.method() !== 'GET'
    ) {
      mutations.push(`${entry.method()} ${entry.url()}`);
    }
  });
  page.on('pageerror', (error) => errors.push(error.message));
  page.on('console', (message) => {
    if (message.text().includes('Content Security Policy')) errors.push(message.text());
  });

  expect((await request.get('/api/v2/admin/sources')).status()).toBe(401);
  await page.goto('/admin/sources');
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
  await changeRole(administrator, account, 'admin');
  await signIn(page, account);
  await page
    .getByRole('navigation', { name: 'Gallery navigation' })
    .getByRole('link', { name: 'Sources' })
    .click();
  await expect(page.getByRole('heading', { name: 'Media sources', exact: true })).toBeVisible();
  await expect(page.locator('.job-list > li')).toHaveCount(30);
  const initial = await page.request.get('/api/v2/admin/sources?limit=30');
  expect(initial.status()).toBe(200);
  const initialPage = (await initial.json()) as components['schemas']['SourcePage'];
  expect(initialPage.items).toHaveLength(30);
  expect(initialPage.has_more).toBe(true);
  const cursor = initialPage.next_cursor!;
  expect(cursor).toBeTruthy();
  expect(
    (
      await administrator.api.get(`/api/v2/admin/sources?cursor=${encodeURIComponent(cursor)}`)
    ).status(),
  ).toBe(400);

  for (const [width, height, scheme] of [
    [375, 812, 'light'],
    [768, 1024, 'dark'],
    [1280, 800, 'light'],
  ] as const) {
    await page.setViewportSize({ width, height });
    await page.emulateMedia({ colorScheme: scheme, reducedMotion: 'reduce' });
    await page.evaluate(() => window.scrollTo(0, 0));
    expect(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)).toBe(
      false,
    );
    for (const target of [
      page
        .getByRole('navigation', { name: 'Gallery navigation' })
        .getByRole('link', { name: 'Sources' }),
      page.getByRole('button', { name: 'Refresh' }),
      page.getByRole('link', { name: 'Next page' }),
    ]) {
      const box = await target.boundingBox();
      expect(box?.width).toBeGreaterThanOrEqual(44);
      expect(box?.height).toBeGreaterThanOrEqual(44);
    }
    const contrast = await page.evaluate(() => {
      const luminance = (color: string) => {
        const values =
          color
            .match(/\d+(?:\.\d+)?/g)
            ?.slice(0, 3)
            .map(Number) ?? [];
        const linear = values.map((value) => {
          const channel = value / 255;
          return channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4;
        });
        return 0.2126 * linear[0]! + 0.7152 * linear[1]! + 0.0722 * linear[2]!;
      };
      const background = luminance(getComputedStyle(document.body).backgroundColor);
      return [
        '.gallery-heading h1',
        '.gallery-heading p',
        '.job-entry-link strong',
        '.job-status',
      ].map((selector) => {
        const foreground = luminance(getComputedStyle(document.querySelector(selector)!).color);
        return (
          (Math.max(background, foreground) + 0.05) / (Math.min(background, foreground) + 0.05)
        );
      });
    });
    for (const ratio of contrast) expect(ratio).toBeGreaterThanOrEqual(4.5);
    await page.screenshot({ path: testInfo.outputPath(`sources-${width}-${scheme}.png`) });
  }
  await page.getByRole('combobox', { name: 'Language' }).selectOption('zh-TW');
  await expect(page.getByRole('heading', { name: '媒體來源', exact: true })).toBeVisible();
  await page.getByRole('combobox', { name: '語言' }).selectOption('zh-CN');
  await expect(page.getByRole('heading', { name: '媒体来源', exact: true })).toBeVisible();
  await page.getByRole('combobox', { name: '语言' }).selectOption('en');

  const target = page.getByRole('link', { name: `View source ${name}` });
  for (let index = 0; index < 4 && (await target.count()) === 0; index += 1) {
    await page.getByRole('link', { name: 'Next page' }).click();
    await expect(page.getByRole('main')).toBeFocused();
  }
  await expect(target).toBeVisible();
  const returnPath = new URL(page.url()).pathname + new URL(page.url()).search;
  await target.click();
  await expect(page.getByRole('heading', { name, exact: true })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Back to sources' })).toHaveAttribute(
    'href',
    returnPath,
  );
  await expect(page.getByText('Matches this server')).toBeVisible();
  await expect(page.getByText('Healthy', { exact: true })).toBeVisible();
  await expect(page.getByText(/does not prove a completed inventory/)).toBeVisible();
  expect(await (await page.request.get(apiPath)).text()).not.toContain('root_path');
  await expect(page.locator('body')).not.toContainText('gallerygo-e2e-');
  await page.screenshot({ path: testInfo.outputPath('source-details.png') });

  let release!: () => void;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  const isSource = (url: URL) => url.pathname === apiPath;
  await page.route(isSource, async (route) => {
    await held;
    await route.fulfill({
      status: 503,
      contentType: 'application/problem+json',
      body: JSON.stringify({
        code: 'INTERNAL_ERROR',
        status: 503,
        title: 'Unavailable',
        type: 'urn:test',
        request_id: 'test',
        detail: '/private/secret',
      }),
    });
  });
  try {
    const pending = page.waitForRequest((entry) => new URL(entry.url()).pathname === apiPath);
    await page.getByRole('button', { name: 'Refresh' }).click();
    await pending;
    await expect(page.getByText('Loading sources…')).toBeVisible();
    await expect(page.getByRole('heading', { name, exact: true })).toHaveCount(0);
    release();
    await expect(
      page.getByText('Could not load sources. Check the connection and try again.'),
    ).toBeVisible();
    await expect(page.getByText('/private/secret')).toHaveCount(0);
    await expect(page.getByRole('heading', { name, exact: true })).toHaveCount(0);
  } finally {
    release();
    await page.unroute(isSource);
  }
  await page.getByRole('alert').getByRole('button', { name: 'Refresh' }).focus();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { name, exact: true })).toBeVisible();
  await expect(page.getByRole('main')).toBeFocused();
  expect(mutations).toEqual([]);
  expect(errors).toEqual([]);
});

test('revoked administrators lose source details and old cursors', async ({
  account,
  administrator,
  page,
  request,
}, testInfo) => {
  test.setTimeout(90_000);
  const id = `browse-ui-${testInfo.project.name}-source`;
  const apiPath = `/api/v2/admin/sources/${id}`;
  expect((await request.get(apiPath)).status()).toBe(401);
  await signIn(page, account);
  await page.goto(`/admin/sources/${id}/details`);
  await expect(page.getByText('Administrator access is required to view sources.')).toBeVisible();
  await expect(page.getByRole('link', { name: 'Sources', exact: true })).toHaveCount(0);
  expect((await page.request.get(apiPath)).status()).toBe(403);

  const promoted = await changeRole(administrator, account, 'admin');
  await signIn(page, account);
  const list = await page.request.get('/api/v2/admin/sources?limit=30');
  expect(list.status()).toBe(200);
  const cursor = ((await list.json()) as components['schemas']['SourcePage']).next_cursor!;
  expect(cursor).toBeTruthy();
  const createdGroup = await administrator.api.post('/api/v2/admin/groups', {
    headers: { ...administrator.headers, 'Idempotency-Key': randomUUID() },
    data: { name: `source-browser-${randomUUID()}` },
  });
  expect(createdGroup.status()).toBe(201);
  const group = (await createdGroup.json()) as components['schemas']['Group'];
  try {
    const beforeResponse = await page.request.get('/api/v2/auth/me');
    expect(beforeResponse.status()).toBe(200);
    const before = (await beforeResponse.json()) as Account['user'];
    const cookieBefore = (await page.context().cookies()).find(
      (cookie) => cookie.name === '__Host-gallerygo_session',
    )?.value;
    expect(cookieBefore).toBeTruthy();
    const membershipPath = `/api/v2/admin/users/${account.user.id}/groups`;
    const membership = await administrator.api.get(membershipPath);
    expect(membership.status()).toBe(200);
    const updated = await administrator.api.put(membershipPath, {
      headers: { ...administrator.headers, 'If-Match': membership.headers().etag! },
      data: { group_ids: [group.id] },
    });
    expect(updated.status()).toBe(200);
    const afterResponse = await page.request.get('/api/v2/auth/me');
    expect(afterResponse.status()).toBe(200);
    const after = (await afterResponse.json()) as Account['user'];
    expect(after.id).toBe(before.id);
    expect(after.scope_id).not.toBe(before.scope_id);
    const cookieAfter = (await page.context().cookies()).find(
      (cookie) => cookie.name === '__Host-gallerygo_session',
    )?.value;
    expect(cookieAfter === cookieBefore).toBe(true);
    const expired = await page.request.get(
      `/api/v2/admin/sources?cursor=${encodeURIComponent(cursor)}`,
    );
    expect(expired.status()).toBe(400);
    expect(await expired.json()).toMatchObject({ code: 'INVALID_CURSOR' });
    await page.goto(`/admin/sources?cursor=${encodeURIComponent(cursor)}`);
    await expect(page.getByText('This page link is invalid or expired.')).toBeVisible();
    await expect(page.locator('.job-list > li')).toHaveCount(0);
    await page.getByRole('link', { name: 'First page' }).click();
    await expect(page.locator('.job-list > li')).toHaveCount(30);
  } finally {
    const current = await administrator.api.get(`/api/v2/admin/groups/${group.id}`);
    expect(current.status()).toBe(200);
    const removed = await administrator.api.delete(`/api/v2/admin/groups/${group.id}`, {
      headers: { ...administrator.headers, 'If-Match': current.headers().etag! },
    });
    expect(removed.status()).toBe(204);
  }
  await page.goto(`/admin/sources/${id}/details`);
  await expect(
    page.getByRole('heading', { name: `browse-ui-${testInfo.project.name}` }),
  ).toBeVisible();

  let release!: () => void;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  const isSource = (url: URL) => url.pathname === apiPath;
  await page.route(isSource, async (route) => {
    await held;
    await route.continue();
  });
  try {
    const pending = page.waitForRequest((entry) => new URL(entry.url()).pathname === apiPath);
    const denied = page.waitForResponse(
      (response) => new URL(response.url()).pathname === apiPath && response.status() === 401,
    );
    await page.getByRole('button', { name: 'Refresh' }).click();
    await pending;
    await expect(page.getByText('Loading sources…')).toBeVisible();
    await expect(page.getByText(id)).toHaveCount(0);
    const demoted = await changeRole(administrator, account, 'user');
    expect(demoted.scope_id).not.toBe(promoted.scope_id);
    release();
    expect((await denied).status()).toBe(401);
    await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
    await expect(page.getByText(id)).toHaveCount(0);
  } finally {
    release();
    await page.unroute(isSource);
  }
  expect((await page.request.get(apiPath)).status()).toBe(401);
});
