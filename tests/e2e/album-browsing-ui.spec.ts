import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('authenticated album browsing uses private previews, scoped pages, and current permissions', async ({
  account,
  administrator,
  page,
  request,
}, testInfo) => {
  const prefix = `browse-ui-${testInfo.project.name}`;
  const path = `/api/v2/admin/libraries/${prefix}-library/access`;
  const memberPath = `/api/v2/admin/users/${account.user.id}/groups`;
  const { api, headers } = administrator;
  const requests: string[] = [];
  const scriptErrors: string[] = [];
  page.on('request', (entry) => requests.push(entry.url()));
  page.on('pageerror', (error) => scriptErrors.push(error.message));
  page.on('console', (message) => {
    if (message.text().includes('Content Security Policy')) scriptErrors.push(message.text());
  });

  await page.goto('/albums');
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
  await signIn(page, account);
  await page
    .getByRole('navigation', { name: 'Gallery navigation' })
    .getByRole('link', { name: 'Albums' })
    .click();
  await expect(page.getByText('No albums are available on this page.')).toBeVisible();

  const created = await api.post('/api/v2/admin/groups', {
    headers: { ...headers, 'Idempotency-Key': randomUUID() },
    data: { name: `Browse UI-${randomUUID()}` },
  });
  expect(created.status()).toBe(201);
  const group = (await created.json()) as components['schemas']['Group'];
  try {
    for (const target of [path, memberPath]) {
      const current = await api.get(target);
      expect(current.status()).toBe(200);
      const changed = await api.put(target, {
        headers: { ...headers, 'If-Match': current.headers().etag! },
        data: { group_ids: [group.id] },
      });
      expect(changed.status()).toBe(200);
    }
    await page.reload();
    await expect(page.getByRole('main')).toBeFocused();
    await page.keyboard.press('Tab');
    await expect(
      page
        .getByRole('navigation', { name: 'Gallery navigation' })
        .getByRole('link', { name: 'Albums' }),
    ).toBeFocused();
    const card = page.getByRole('link', { name: /Real PNG fixture/ });
    await expect(card).toBeVisible();
    await expect(card.locator('img')).toBeVisible();
    await expect
      .poll(() => card.locator('img').evaluate((image: HTMLImageElement) => image.naturalWidth))
      .toBe(1);
    await expect(page.getByText('Hidden private album')).toHaveCount(0);
    const url = `/api/v2/media/${prefix}-media/variants/${prefix}-media-variant/content`;
    expect((await request.get(url)).status()).toBe(401);

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
      const sortBox = await page.getByRole('combobox', { name: 'Sort albums' }).boundingBox();
      expect(sortBox?.height).toBeGreaterThanOrEqual(44);
      await page.screenshot({
        path: testInfo.outputPath(`album-grid-${width}-${scheme}.png`),
        fullPage: true,
      });
    }
    await page.getByRole('combobox', { name: 'Language' }).selectOption('zh-TW');
    await expect(page.getByRole('heading', { name: '相簿', exact: true })).toBeVisible();
    await page.getByRole('combobox', { name: '語言' }).selectOption('en');

    await page.getByRole('link', { name: 'Next page' }).click();
    await expect(page.getByRole('main')).toBeFocused();
    await expect(page.getByText('Paged album 31')).toBeVisible();
    await expect(page.getByText('Hidden private album')).toHaveCount(0);
    await page.getByRole('link', { name: 'First page' }).click();
    await card.click();
    await expect(page.getByRole('main')).toBeFocused();
    await expect(page.getByRole('heading', { name: 'Real PNG fixture' })).toBeVisible();
    await expect(page.getByText('fixture.png', { exact: true })).toBeVisible();
    await expect(page.getByText('other.png', { exact: true })).toBeVisible();
    await expect(
      page.getByText('Offline downloads and the full media viewer are still under development.'),
    ).toBeVisible();
    await expect.poll(() => page.locator('.gallery-media img').count()).toBeGreaterThan(0);
    await page.screenshot({ path: testInfo.outputPath('album-detail.png'), fullPage: true });

    const current = await api.get(path);
    expect(current.status()).toBe(200);
    const revoked = await api.put(path, {
      headers: { ...headers, 'If-Match': current.headers().etag! },
      data: { group_ids: [] },
    });
    expect(revoked.status()).toBe(200);
    await page.reload();
    await expect(
      page.getByText('This album is unavailable or you no longer have access.'),
    ).toBeVisible();
    await expect(page.getByText('Real PNG fixture')).toHaveCount(0);
    expect((await page.request.get(url)).status()).toBe(404);
    expect(requests.some((entry) => new URL(entry).pathname.endsWith('/original'))).toBe(false);
    expect(scriptErrors).toEqual([]);
  } finally {
    const current = await api.get(`/api/v2/admin/groups/${group.id}`);
    if (current.status() === 200) {
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
