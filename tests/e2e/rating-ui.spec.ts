import { randomBytes, randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('album rating UI keeps personal revisions, recovery and shared averages current over HTTPS', async ({
  account,
  administrator,
  page,
  context,
  request,
}, testInfo) => {
  const prefix = `rating-ui-${testInfo.project.name}`;
  const albumID = `${prefix}-album`;
  const ratingPath = `/api/v2/albums/${albumID}/rating`;
  const albumPath = `/api/v2/albums/${albumID}`;
  const policyPath = `/api/v2/admin/libraries/${prefix}-library/access`;
  const membershipPath = `/api/v2/admin/users/${account.user.id}/groups`;
  const { api, headers } = administrator;
  const pageErrors: string[] = [];
  const requested: string[] = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  page.on('request', (entry) => requested.push(entry.url()));
  page.on('console', (message) => {
    if (message.text().includes('Content Security Policy')) pageErrors.push(message.text());
  });
  expect((await request.get(ratingPath)).status()).toBe(401);
  await signIn(page, account);
  const groupResponse = await api.post('/api/v2/admin/groups', {
    headers: { ...headers, 'Idempotency-Key': randomUUID() },
    data: { name: `Rating UI-${randomUUID()}` },
  });
  expect(groupResponse.status()).toBe(201);
  const group = (await groupResponse.json()) as components['schemas']['Group'];
  let secondUserID: string | undefined;
  try {
    for (const target of [policyPath, membershipPath]) {
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
    await page.goto(`/albums/${albumID}`);
    await expect(page.getByRole('heading', { name: 'Real PNG fixture' })).toBeVisible();
    await expect(page.getByRole('radio', { name: 'No rating' })).toBeChecked();
    await expect(page.getByText('No ratings yet.')).toBeVisible();
    await page.getByRole('radio', { name: 'No rating' }).focus();
    await page.keyboard.press('ArrowRight');
    await expect(page.getByRole('radio', { name: '1 star', exact: true })).toBeChecked();
    await page.keyboard.press('ArrowLeft');
    await expect(page.getByRole('radio', { name: 'No rating' })).toBeChecked();
    await expect(page.getByRole('button', { name: 'Save rating' })).toBeDisabled();

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
      for (const label of await page.locator('.rating-choice').all()) {
        const size = await label.boundingBox();
        expect(size?.height).toBeGreaterThanOrEqual(44);
        expect(size?.width).toBeGreaterThanOrEqual(44);
      }
      const contrast = await page
        .locator('.rating-choice')
        .first()
        .evaluate((element) => {
          const luminance = (color: string) => {
            const channels =
              color
                .match(/\d+(?:\.\d+)?/g)
                ?.slice(0, 3)
                .map(Number) ?? [];
            const linear = channels.map((value) => {
              const channel = value / 255;
              return channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4;
            });
            return 0.2126 * linear[0]! + 0.7152 * linear[1]! + 0.0722 * linear[2]!;
          };
          const style = getComputedStyle(element);
          const text = luminance(style.color);
          const background = luminance(style.backgroundColor);
          return (Math.max(text, background) + 0.05) / (Math.min(text, background) + 0.05);
        });
      expect(contrast).toBeGreaterThanOrEqual(4.5);
      await page.screenshot({
        path: testInfo.outputPath(`rating-${width}-${scheme}.png`),
        fullPage: true,
      });
    }
    await page.getByRole('combobox', { name: 'Language' }).selectOption('zh-TW');
    await expect(page.getByRole('group', { name: '你的評分' })).toBeVisible();
    await expect(page.getByRole('radio', { name: '5 顆星' })).toBeVisible();
    await page.getByRole('combobox', { name: '語言' }).selectOption('zh-CN');
    await expect(page.getByRole('group', { name: '您的评分' })).toBeVisible();
    await page.getByRole('combobox', { name: '语言' }).selectOption('en');

    await page.getByRole('radio', { name: '5 stars' }).check();
    await page.getByRole('button', { name: 'Save rating' }).click();
    await expect(page.getByText('Average rating: 5 of 5')).toBeVisible();
    await expect(page.getByRole('radio', { name: '5 stars' })).toBeChecked();
    expect(await (await context.request.get(albumPath)).json()).toMatchObject({ rating_avg: 5 });
    const adminPersonal = await api.get(ratingPath);
    expect(adminPersonal.status()).toBe(200);
    expect(
      (
        await api.put(ratingPath, {
          headers: {
            ...headers,
            'If-Match': adminPersonal.headers().etag!,
            'Idempotency-Key': randomUUID(),
          },
          data: { rating: 1 },
        })
      ).status(),
    ).toBe(200);
    await page.reload();
    await expect(page.getByText('Average rating: 3 of 5')).toBeVisible();
    await expect(page.getByRole('radio', { name: '5 stars' })).toBeChecked();

    const csrfResponse = await context.request.get('/api/v2/auth/csrf');
    const { token } = (await csrfResponse.json()) as { token: string };
    const ownHeaders = { Origin: headers.Origin, 'X-CSRF-Token': token };
    let current = await context.request.get(ratingPath);
    expect(
      (
        await context.request.put(ratingPath, {
          headers: {
            ...ownHeaders,
            'If-Match': current.headers().etag!,
            'Idempotency-Key': randomUUID(),
          },
          data: { rating: 2 },
        })
      ).status(),
    ).toBe(200);
    await page.getByRole('radio', { name: '4 stars' }).check();
    await page.getByRole('button', { name: 'Save rating' }).click();
    await expect(page.getByText(/Your rating changed elsewhere/)).toBeVisible();
    expect(await (await context.request.get(ratingPath)).json()).toMatchObject({ rating: 2 });
    await page.getByRole('button', { name: 'Reload current rating' }).click();
    await expect(page.getByRole('radio', { name: '2 stars' })).toBeChecked();
    await expect(page.getByText('Average rating: 1.5 of 5')).toBeVisible();

    const commands: {
      method: string;
      etag: string | undefined;
      key: string | undefined;
      body: string | null;
    }[] = [];
    let drop = true;
    await page.route(`**${ratingPath}`, async (route) => {
      const entry = route.request();
      if (entry.method() !== 'PUT') return route.continue();
      const requestHeaders = entry.headers();
      commands.push({
        method: entry.method(),
        etag: requestHeaders['if-match'],
        key: requestHeaders['idempotency-key'],
        body: entry.postData(),
      });
      if (drop) {
        drop = false;
        const response = await route.fetch();
        expect(response.status()).toBe(200);
        return route.abort('failed');
      }
      return route.continue();
    });
    await page.getByRole('radio', { name: '4 stars' }).check();
    await page.getByRole('button', { name: 'Save rating' }).click();
    await expect(page.getByText(/Could not confirm whether your rating was saved/)).toBeVisible();

    await page.route(`**${albumPath}`, (route) =>
      route.fulfill({
        status: 503,
        contentType: 'application/problem+json',
        body: JSON.stringify({ code: 'UNAVAILABLE', status: 503, title: 'Unavailable' }),
      }),
    );
    const failedRevalidation = page.waitForResponse(
      (entry) => new URL(entry.url()).pathname === albumPath && entry.status() === 503,
    );
    await page.evaluate(() => window.dispatchEvent(new Event('visibilitychange')));
    await failedRevalidation;
    await expect(
      page.getByText('Could not load this page. Check your connection and try again.'),
    ).toBeVisible();
    await expect(page.getByText('Real PNG fixture')).toHaveCount(0);
    await expect(page.getByRole('button', { name: 'Retry request' })).toHaveCount(0);
    await page.unroute(`**${albumPath}`);
    await page.getByRole('button', { name: 'Try again' }).click();
    await expect(page.getByRole('button', { name: 'Retry request' })).toBeVisible();

    let resumeAlbum: () => void = () => {};
    const albumBlocked = new Promise<void>((resolve) => {
      resumeAlbum = resolve;
    });
    await page.route(`**${albumPath}`, async (route) => {
      await albumBlocked;
      await route.continue();
    });
    const revalidation = page.waitForRequest(
      (entry) => new URL(entry.url()).pathname === albumPath && entry.method() === 'GET',
    );
    await page.evaluate(() => window.dispatchEvent(new Event('visibilitychange')));
    await revalidation;
    await expect(page.getByText('Loading albums…')).toBeVisible();
    await expect(page.getByRole('button', { name: 'Retry request' })).toHaveCount(0);
    resumeAlbum();
    await expect(page.getByRole('button', { name: 'Retry request' })).toBeVisible();
    await page.unroute(`**${albumPath}`);

    current = await context.request.get(ratingPath);
    expect(await current.json()).toMatchObject({ rating: 4 });
    expect(
      (
        await context.request.put(ratingPath, {
          headers: {
            ...ownHeaders,
            'If-Match': current.headers().etag!,
            'Idempotency-Key': randomUUID(),
          },
          data: { rating: 3 },
        })
      ).status(),
    ).toBe(200);
    await page.getByRole('button', { name: 'Retry request' }).click();
    await expect(page.getByRole('radio', { name: '3 stars' })).toBeChecked();
    await expect(page.getByText('Average rating: 2 of 5')).toBeVisible();
    expect(commands).toHaveLength(2);
    expect(commands[1]).toEqual(commands[0]);
    expect(commands[0]?.key).toMatch(/^[0-9a-f-]{36}$/);
    await page.unroute(`**${ratingPath}`);

    const clears: string[] = [];
    page.on('request', (entry) => {
      if (new URL(entry.url()).pathname === ratingPath && entry.method() === 'DELETE') {
        clears.push(entry.postData() ?? '');
      }
    });
    await page.getByRole('radio', { name: 'No rating' }).check();
    await page.getByRole('button', { name: 'Save rating' }).click();
    await expect(page.getByRole('radio', { name: 'No rating' })).toBeChecked();
    await expect(page.getByText('Average rating: 1 of 5')).toBeVisible();
    expect(clears).toEqual(['']);
    expect(await (await context.request.get(ratingPath)).json()).toMatchObject({ rating: null });
    await page.getByRole('radio', { name: '4 stars' }).check();
    await page.getByRole('button', { name: 'Save rating' }).click();
    await expect(page.getByText('Average rating: 2.5 of 5')).toBeVisible();

    const secondPassword = randomBytes(24).toString('base64url');
    const secondResponse = await api.post('/api/v2/admin/users', {
      headers: { ...headers, 'Idempotency-Key': randomUUID() },
      data: { username: `rating-${randomUUID()}`, password: secondPassword },
    });
    expect(secondResponse.status()).toBe(201);
    const secondUser = (await secondResponse.json()) as components['schemas']['User'];
    secondUserID = secondUser.id;
    const secondMembership = `/api/v2/admin/users/${secondUser.id}/groups`;
    current = await api.get(secondMembership);
    expect(
      (
        await api.put(secondMembership, {
          headers: { ...headers, 'If-Match': current.headers().etag! },
          data: { group_ids: [group.id] },
        })
      ).status(),
    ).toBe(200);
    await page.getByRole('link', { name: 'Account security' }).click();
    await page.getByRole('button', { name: 'Sign out' }).click();
    await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
    await signIn(page, { user: secondUser, password: secondPassword });
    await page.goto(`/albums/${albumID}`);
    await expect(page.getByRole('radio', { name: 'No rating' })).toBeChecked();
    await expect(page.getByText('Average rating: 2.5 of 5')).toBeVisible();
    await page.getByRole('radio', { name: '5 stars' }).check();
    await page.getByRole('button', { name: 'Save rating' }).click();
    await expect(page.getByText('Average rating: 3.33 of 5')).toBeVisible();
    await page.getByRole('link', { name: 'Account security' }).click();
    await page.getByRole('button', { name: 'Sign out' }).click();
    await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
    await signIn(page, account);
    await page.goto(`/albums/${albumID}`);
    await expect(page.getByRole('radio', { name: '4 stars' })).toBeChecked();
    await expect(page.getByRole('radio', { name: '5 stars' })).not.toBeChecked();
    await expect(page.getByText('Average rating: 3.33 of 5')).toBeVisible();

    const otherTab = await context.newPage();
    try {
      await otherTab.goto('/settings/security');
      await otherTab.getByRole('button', { name: 'Sign out' }).click();
      await signIn(otherTab, { user: secondUser, password: secondPassword });
      await page.bringToFront();
      await page.evaluate(() => window.dispatchEvent(new Event('visibilitychange')));
      await expect(page.getByRole('radio', { name: '5 stars' })).toBeChecked();
      await expect(page.getByRole('radio', { name: '4 stars' })).not.toBeChecked();

      await otherTab.bringToFront();
      await otherTab.getByRole('button', { name: 'Sign out' }).click();
      await signIn(otherTab, account);
      await page.bringToFront();
      await page.evaluate(() => window.dispatchEvent(new Event('visibilitychange')));
      await expect(page.getByRole('radio', { name: '4 stars' })).toBeChecked();
      await expect(page.getByRole('radio', { name: '5 stars' })).not.toBeChecked();
    } finally {
      await otherTab.close();
    }

    const secondPath = `/api/v2/admin/users/${secondUser.id}`;
    current = await api.get(secondPath);
    expect(
      (
        await api.delete(secondPath, {
          headers: { ...headers, 'If-Match': current.headers().etag! },
        })
      ).status(),
    ).toBe(204);
    secondUserID = undefined;
    await page.reload();
    await expect(page.getByText('Average rating: 2.5 of 5')).toBeVisible();

    current = await api.get(policyPath);
    expect(
      (
        await api.put(policyPath, {
          headers: { ...headers, 'If-Match': current.headers().etag! },
          data: { group_ids: [] },
        })
      ).status(),
    ).toBe(200);
    await page.getByRole('radio', { name: '3 stars' }).check();
    await page.getByRole('button', { name: 'Save rating' }).click();
    await expect(
      page.getByText('This album is unavailable or you no longer have access.'),
    ).toBeVisible();
    await expect(page.getByText('Real PNG fixture')).toHaveCount(0);
    await expect(page.getByRole('radio', { name: '3 stars' })).toHaveCount(0);
    await page.reload();
    await expect(
      page.getByText('This album is unavailable or you no longer have access.'),
    ).toBeVisible();
    expect((await context.request.get(ratingPath)).status()).toBe(404);
    expect(requested.some((entry) => new URL(entry).pathname.endsWith('/original'))).toBe(false);
    expect(pageErrors).toEqual([]);
  } finally {
    if (secondUserID) {
      const target = `/api/v2/admin/users/${secondUserID}`;
      const second = await api.get(target);
      if (second.status() === 200) {
        expect(
          (
            await api.delete(target, {
              headers: { ...headers, 'If-Match': second.headers().etag! },
            })
          ).status(),
        ).toBe(204);
      }
    }
    const own = await api.get(ratingPath);
    if (own.status() === 200) {
      expect(
        (
          await api.delete(ratingPath, {
            headers: {
              ...headers,
              'If-Match': own.headers().etag!,
              'Idempotency-Key': randomUUID(),
            },
          })
        ).status(),
      ).toBe(200);
    }
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
