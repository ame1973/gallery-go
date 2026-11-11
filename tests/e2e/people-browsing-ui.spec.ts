import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('people browsing follows current grants and preserves scoped page navigation', async ({
  account,
  administrator,
  page,
  request,
}, testInfo) => {
  const prefix = `people-ui-${testInfo.project.name}`;
  const personPath = `/people/${prefix}-person`;
  const libraryPolicy = `/api/v2/admin/libraries/${prefix}-library/access`;
  const albumPolicy = `/api/v2/admin/albums/${prefix}-album/access`;
  const selectedPolicy = `/api/v2/admin/albums/${prefix}-album-extra-29/access`;
  const membership = `/api/v2/admin/users/${account.user.id}/groups`;
  const preview = `/api/v2/media/${prefix}-media/variants/${prefix}-media-variant/content`;
  const { api, headers } = administrator;
  const requests: string[] = [];
  const errors: string[] = [];
  page.on('request', (entry) => requests.push(entry.url()));
  page.on('pageerror', (error) => errors.push(error.message));
  page.on('console', (message) => {
    if (message.text().includes('Content Security Policy')) errors.push(message.text());
  });

  expect((await request.get('/api/v2/people')).status()).toBe(401);
  expect((await request.get(`/api/v2${personPath}`)).status()).toBe(401);
  await page.goto('/people');
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
  await signIn(page, account);
  await page.goto('/people');
  await expect(page.getByText('No people are available on this page.')).toBeVisible();
  expect((await page.request.get(`/api/v2${personPath}`)).status()).toBe(404);

  const created = await api.post('/api/v2/admin/groups', {
    headers: { ...headers, 'Idempotency-Key': randomUUID() },
    data: { name: `People UI-${randomUUID()}` },
  });
  expect(created.status()).toBe(201);
  const group = (await created.json()) as components['schemas']['Group'];
  try {
    for (const target of [libraryPolicy, membership]) {
      const current = await api.get(target);
      expect(current.status()).toBe(200);
      expect(
        (
          await api.put(target, {
            headers: { ...headers, 'If-Match': current.headers().etag! },
            data: { group_ids: [group.id] },
          })
        ).status(),
      ).toBe(200);
    }
    await page.reload();
    await expect(page.getByRole('main')).toBeFocused();
    await page.keyboard.press('Tab');
    await expect(
      page
        .getByRole('navigation', { name: 'Gallery navigation' })
        .getByRole('link', { name: 'Albums' }),
    ).toBeFocused();
    await page.keyboard.press('Tab');
    await expect(
      page
        .getByRole('navigation', { name: 'Gallery navigation' })
        .getByRole('link', { name: 'People' }),
    ).toBeFocused();
    await expect(page.locator('.gallery-grid > li > a')).toHaveCount(30);
    await expect(page.getByRole('link', { name: /Same display name/ })).toHaveCount(2);
    await expect(page.getByText('Hidden private person')).toHaveCount(0);
    const primary = page.getByRole('link', { name: /Visible creator/ });
    await expect(primary).toBeVisible();
    await expect(primary.getByText('33 albums')).toBeVisible();
    await expect(primary.locator('img')).toBeVisible();
    await expect
      .poll(() => primary.locator('img').evaluate((image: HTMLImageElement) => image.naturalWidth))
      .toBe(1);
    expect((await request.get(preview)).status()).toBe(401);
    expect((await page.request.get(preview)).status()).toBe(200);

    for (const [width, height, scheme] of [
      [375, 812, 'light'],
      [768, 1024, 'dark'],
      [1280, 800, 'light'],
    ] as const) {
      await page.setViewportSize({ width, height });
      await page.emulateMedia({ colorScheme: scheme, reducedMotion: 'reduce' });
      expect(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)).toBe(
        false,
      );
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
        return ['.gallery-heading h1', '.gallery-heading p'].map((selector) => {
          const foreground = luminance(getComputedStyle(document.querySelector(selector)!).color);
          return (
            (Math.max(background, foreground) + 0.05) / (Math.min(background, foreground) + 0.05)
          );
        });
      });
      for (const ratio of contrast) expect(ratio).toBeGreaterThanOrEqual(4.5);
      const peopleNav = await page
        .getByRole('navigation', { name: 'Gallery navigation' })
        .getByRole('link', { name: 'People' })
        .boundingBox();
      expect(peopleNav?.height).toBeGreaterThanOrEqual(44);
      await page.screenshot({
        path: testInfo.outputPath(`people-grid-${width}-${scheme}.png`),
        fullPage: true,
      });
    }
    await page.getByRole('combobox', { name: 'Language' }).selectOption('zh-TW');
    await expect(page.getByRole('heading', { name: '人物', exact: true })).toBeVisible();
    await page.getByRole('combobox', { name: '語言' }).selectOption('zh-CN');
    await expect(page.getByRole('heading', { name: '人物', exact: true })).toBeVisible();
    await page.getByRole('combobox', { name: '语言' }).selectOption('en');

    await page.getByRole('link', { name: 'Next page' }).click();
    await expect(page.getByRole('main')).toBeFocused();
    await expect(page.locator('.gallery-grid > li > a')).toHaveCount(3);
    const peopleNext = page.url();
    await page.getByRole('link', { name: /Person 29/ }).click();
    await expect(page.getByRole('heading', { name: 'Person 29' })).toBeVisible();
    await page.getByRole('link', { name: /Solo album 29/ }).click();
    await expect(page.getByRole('heading', { name: 'Solo album 29' })).toBeVisible();
    await page.getByRole('link', { name: 'Back to person' }).click();
    await expect(page.getByRole('heading', { name: 'Person 29' })).toBeVisible();
    await page.getByRole('link', { name: 'Back to people' }).click();
    await expect(page).toHaveURL(peopleNext);
    await expect(page.locator('.gallery-grid > li > a')).toHaveCount(3);
    await page.getByRole('link', { name: 'First page' }).click();
    await primary.click();
    await expect(page.getByRole('heading', { name: 'Visible creator' })).toBeVisible();
    await expect(page.getByText('33 albums')).toBeVisible();
    await expect(page.locator('.gallery-grid > li > a')).toHaveCount(30);

    // Simulate a visibility refetch and hold one real browser request in flight.
    const personAPI = `**/api/v2/people/${prefix}-person`;
    let release!: () => void;
    const pending = new Promise<void>((resolve) => {
      release = resolve;
    });
    await page.route(personAPI, async (route) => {
      await pending;
      await route.fulfill({
        status: 503,
        contentType: 'application/problem+json',
        body: JSON.stringify({
          code: 'INTERNAL_ERROR',
          status: 503,
          title: 'Unavailable',
          type: 'urn:test',
          request_id: 'test',
          detail: 'private path',
        }),
      });
    });
    try {
      const requestStarted = page.waitForRequest(
        (entry) => new URL(entry.url()).pathname === `/api/v2/people/${prefix}-person`,
      );
      await page.evaluate(() => window.dispatchEvent(new Event('visibilitychange')));
      await requestStarted;
      await expect(page.getByText('Loading this person and their albums…')).toBeVisible();
      await expect(page.getByRole('heading', { name: 'Visible creator' })).toHaveCount(0);
      await expect(page.getByRole('link', { name: /Real PNG fixture/ })).toHaveCount(0);
      release();
      await expect(
        page.getByText('Could not load this page. Check your connection and try again.'),
      ).toBeVisible();
      await expect(page.getByText('Visible creator')).toHaveCount(0);
      await expect(page.getByText('private path')).toHaveCount(0);
    } finally {
      release();
      await page.unroute(personAPI);
    }
    await page.getByRole('button', { name: 'Try again' }).click();
    await expect(page.getByRole('heading', { name: 'Visible creator' })).toBeVisible();
    await page.getByRole('link', { name: 'Next page' }).click();
    await expect(page.getByRole('main')).toBeFocused();
    await expect(page.getByRole('link', { name: /Creator album 29/ })).toBeVisible();
    const personNext = page.url();
    await page.getByRole('link', { name: /Creator album 29/ }).click();
    await expect(page.getByRole('heading', { name: 'Creator album 29' })).toBeVisible();
    const selectedAlbum = await api.get(selectedPolicy);
    expect(selectedAlbum.status()).toBe(200);
    expect(
      (
        await api.put(selectedPolicy, {
          headers: { ...headers, 'If-Match': selectedAlbum.headers().etag! },
          data: { restricted: true, group_ids: [] },
        })
      ).status(),
    ).toBe(200);
    await page.reload();
    await expect(
      page.getByText('This album is unavailable or you no longer have access.'),
    ).toBeVisible();
    await page.getByRole('alert').getByRole('link', { name: 'Back to person' }).click();
    await expect(page).toHaveURL(personNext);
    const invalid =
      'This page link is invalid or expired, or access has changed. Return to the first page.';
    await expect(page.getByText(invalid)).toBeVisible();
    await expect(page.getByText('Visible creator')).toHaveCount(0);
    await page.getByRole('link', { name: 'First page' }).click();
    await expect(page.getByRole('heading', { name: 'Visible creator' })).toBeVisible();
    await expect(page.getByText('32 albums')).toBeVisible();

    const currentAlbum = await api.get(albumPolicy);
    expect(currentAlbum.status()).toBe(200);
    expect(
      (
        await api.put(albumPolicy, {
          headers: { ...headers, 'If-Match': currentAlbum.headers().etag! },
          data: { restricted: true, group_ids: [] },
        })
      ).status(),
    ).toBe(200);
    await page.reload();
    await expect(page.getByRole('heading', { name: 'Visible creator' })).toBeVisible();
    await expect(page.getByText('31 albums')).toBeVisible();
    await expect(page.getByRole('link', { name: /Real PNG fixture/ })).toHaveCount(0);
    expect((await page.request.get(preview)).status()).toBe(404);
    await page.goto(peopleNext);
    await expect(page.getByText(invalid)).toBeVisible();
    await page.getByRole('link', { name: 'First page' }).click();
    await expect(page.getByRole('link', { name: /Visible creator/ })).toBeVisible();

    const currentLibrary = await api.get(libraryPolicy);
    expect(currentLibrary.status()).toBe(200);
    expect(
      (
        await api.put(libraryPolicy, {
          headers: { ...headers, 'If-Match': currentLibrary.headers().etag! },
          data: { group_ids: [] },
        })
      ).status(),
    ).toBe(200);
    await page.goto(personPath);
    await expect(
      page.getByText('This person is unavailable or you no longer have access.'),
    ).toBeVisible();
    await expect(page.getByText('Visible creator')).toHaveCount(0);
    await page
      .getByRole('navigation', { name: 'Gallery navigation' })
      .getByRole('link', { name: 'People' })
      .click();
    await expect(page.getByText('No people are available on this page.')).toBeVisible();
    expect(requests.some((entry) => new URL(entry).pathname.endsWith('/original'))).toBe(false);
    expect(errors).toEqual([]);
  } finally {
    try {
      // Group deletion preserves restriction latches, so restore both fixture policies explicitly.
      for (const target of [albumPolicy, selectedPolicy]) {
        const current = await api.get(target);
        expect(current.status()).toBe(200);
        const restored = await api.put(target, {
          headers: { ...headers, 'If-Match': current.headers().etag! },
          data: { restricted: false, group_ids: [] },
        });
        expect(restored.status()).toBe(200);
        expect(await restored.json()).toMatchObject({ restricted: false, group_ids: [] });
      }
    } finally {
      const current = await api.get(`/api/v2/admin/groups/${group.id}`);
      expect(current.status()).toBe(200);
      expect(
        (
          await api.delete(`/api/v2/admin/groups/${group.id}`, {
            headers: { ...headers, 'If-Match': current.headers().etag! },
          })
        ).status(),
      ).toBe(204);
    }
  }
});
