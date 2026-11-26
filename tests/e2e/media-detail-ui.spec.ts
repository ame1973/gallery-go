import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('media detail uses current visibility, protected previews, and honest metadata', async ({
  account,
  administrator,
  page,
  request,
}, testInfo) => {
  const prefix = `media-detail-ui-${testInfo.project.name}`;
  const mediaID = `${prefix}-media`;
  const albumID = `${prefix}-album`;
  const detailPath = `/inspect/${mediaID}/details`;
  const mediaAPI = `/api/v2/media/${mediaID}`;
  const preview = `${mediaAPI}/variants/${mediaID}-variant/content`;
  const libraryPolicy = `/api/v2/admin/libraries/${prefix}-library/access`;
  const membership = `/api/v2/admin/users/${account.user.id}/groups`;
  const { api, headers } = administrator;
  const requests: string[] = [];
  const errors: string[] = [];
  page.on('request', (entry) => requests.push(entry.url()));
  page.on('pageerror', (error) => errors.push(error.message));
  page.on('console', (message) => {
    if (message.text().includes('Content Security Policy')) errors.push(message.text());
  });

  expect((await request.get(mediaAPI)).status()).toBe(401);
  expect((await request.get(preview)).status()).toBe(401);
  await page.goto(detailPath);
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
  await signIn(page, account);
  await page.goto(detailPath);
  await expect(
    page.getByText('This media item is unavailable or you no longer have access.'),
  ).toBeVisible();
  expect((await page.request.get(mediaAPI)).status()).toBe(404);
  await expect(page.getByText('fixture.png')).toHaveCount(0);

  const groupID = `${prefix}-group`;
  try {
    const policy = await api.get(libraryPolicy);
    expect(policy.status()).toBe(200);
    expect(await policy.json()).toMatchObject({ group_ids: [groupID] });
    const initialMembership = await api.get(membership);
    expect(initialMembership.status()).toBe(200);
    expect(await initialMembership.json()).toMatchObject({ group_ids: [] });
    expect(
      (
        await api.put(membership, {
          headers: { ...headers, 'If-Match': initialMembership.headers().etag! },
          data: { group_ids: [groupID] },
        })
      ).status(),
    ).toBe(200);
    const metadataResponse = await page.request.get(mediaAPI);
    expect(metadataResponse.status()).toBe(200);
    const metadata = (await metadataResponse.json()) as components['schemas']['Media'];
    expect(metadata).toMatchObject({
      id: mediaID,
      album_id: albumID,
      kind: 'image',
      file_name: 'fixture.png',
      codec: 'png',
      width: 1,
      height: 1,
      animated: false,
      frame_count: 1,
      loop_count: 1,
      orientation: null,
      capture_at: null,
      offline_capable: false,
    });
    expect((await page.request.get(`${mediaAPI}/original`)).status()).toBe(404);
    await page.goto(`/albums/${albumID}`);
    await expect(page.getByRole('heading', { name: 'Real PNG fixture' })).toBeVisible();
    const card = page.getByRole('link', { name: 'View fixture.png details' });
    await expect(card).toBeVisible();
    await card.click();
    await expect(page).toHaveURL(new RegExp(`${detailPath}$`));
    await expect(page.getByRole('heading', { name: 'fixture.png' })).toBeVisible();
    await expect(page.getByRole('main')).toBeFocused();
    await page.keyboard.press('Tab');
    await expect(
      page
        .getByRole('navigation', { name: 'Gallery navigation' })
        .getByRole('link', { name: 'Albums' }),
    ).toBeFocused();
    const image = page.getByRole('img', { name: 'Preview of fixture.png' });
    await expect(image).toBeVisible();
    await expect.poll(() => image.evaluate((node: HTMLImageElement) => node.naturalWidth)).toBe(1);
    expect((await page.request.get(preview)).status()).toBe(200);
    for (const [label, expected] of [
      ['Encoded dimensions', '1 × 1 px'],
      ['Codec', 'png'],
      ['Captured', 'Unknown'],
      ['Orientation code', 'Unknown'],
      ['Animation', 'No'],
      ['Frames', '1'],
      ['Playthroughs', '1'],
      ['Cycle duration', 'Unknown'],
    ]) {
      const row = page.locator('.media-metadata > div').filter({
        has: page.getByText(label!, { exact: true }),
      });
      await expect(row.locator('dd')).toHaveText(expected!);
    }

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
        return ['.gallery-heading h1', '.media-metadata dt', '.media-detail-preview p'].map(
          (selector) => {
            const foreground = luminance(getComputedStyle(document.querySelector(selector)!).color);
            return (
              (Math.max(background, foreground) + 0.05) / (Math.min(background, foreground) + 0.05)
            );
          },
        );
      });
      for (const ratio of contrast) expect(ratio).toBeGreaterThanOrEqual(4.5);
      const back = await page.getByRole('link', { name: 'Back to album' }).boundingBox();
      expect(back?.height).toBeGreaterThanOrEqual(44);
      await page.screenshot({
        path: testInfo.outputPath(`media-detail-${width}-${scheme}.png`),
        fullPage: true,
      });
    }
    await page.getByRole('combobox', { name: 'Language' }).selectOption('zh-TW');
    await expect(page.getByRole('heading', { name: '媒體詳細資料' })).toBeVisible();
    await expect(page.getByText('編碼尺寸')).toBeVisible();
    await page.getByRole('combobox', { name: '語言' }).selectOption('zh-CN');
    await expect(page.getByRole('heading', { name: '媒体详情' })).toBeVisible();
    await expect(page.getByText('编码尺寸')).toBeVisible();
    await page.getByRole('combobox', { name: '语言' }).selectOption('en');

    const meResponse = await page.request.get('/api/v2/auth/me');
    expect(meResponse.status()).toBe(200);
    const me = (await meResponse.json()) as components['schemas']['User'];
    for (const endpoint of [mediaAPI, `${mediaAPI}/variants`]) {
      for (const mode of [
        'stripped-request',
        'partial-request',
        'wrong-echo',
        'wrong-request',
      ] as const) {
        const matches = (candidate: URL) => candidate.pathname === endpoint;
        let observed = false;
        await page.route(matches, async (route) => {
          const requestHeaders = await route.request().allHeaders();
          expect(requestHeaders['x-gallerygo-expected-user']).toBe(me.id);
          expect(requestHeaders['x-gallerygo-expected-scope']).toBe(me.scope_id);
          if (mode === 'stripped-request' || mode === 'partial-request') {
            delete requestHeaders['x-gallerygo-expected-scope'];
            if (mode === 'stripped-request') delete requestHeaders['x-gallerygo-expected-user'];
          } else if (mode === 'wrong-request') {
            requestHeaders['x-gallerygo-expected-user'] = 'different-account';
          }
          const upstream = await route.fetch({ headers: requestHeaders });
          const responseHeaders = upstream.headers();
          if (mode === 'wrong-request' || mode === 'partial-request') {
            expect(upstream.status()).toBe(mode === 'wrong-request' ? 409 : 400);
            expect(await upstream.json()).toMatchObject({
              code: mode === 'wrong-request' ? 'VERSION_CONFLICT' : 'INVALID_READ_CONTEXT',
            });
          } else if (mode === 'stripped-request') {
            expect(upstream.status()).toBe(200);
            expect(responseHeaders['x-gallerygo-expected-user']).toBeUndefined();
            expect(responseHeaders['x-gallerygo-expected-scope']).toBeUndefined();
          } else {
            expect(upstream.status()).toBe(200);
            expect(responseHeaders['x-gallerygo-expected-user']).toBe(me.id);
            expect(responseHeaders['x-gallerygo-expected-scope']).toBe(me.scope_id);
            responseHeaders['x-gallerygo-expected-scope'] = 'different-scope';
          }
          observed = true;
          await route.fulfill({ response: upstream, headers: responseHeaders });
        });
        try {
          await page.goto(detailPath);
          await expect(page.getByRole('alert')).toContainText(
            'The current account changed or could not be verified.',
          );
          expect(observed).toBe(true);
          await expect(page.getByRole('heading', { name: 'fixture.png' })).toHaveCount(0);
          await expect(page.getByText('Encoded dimensions')).toHaveCount(0);
          await expect(page.getByRole('img', { name: 'Preview of fixture.png' })).toHaveCount(0);
        } finally {
          await page.unroute(matches);
        }
        await page.getByRole('button', { name: 'Try again' }).click();
        await expect(page.getByRole('heading', { name: 'fixture.png' })).toBeVisible();
        await expect(page.getByRole('img', { name: 'Preview of fixture.png' })).toBeVisible();
      }
    }
    await page.getByRole('link', { name: 'Back to album' }).click();
    await expect(page).toHaveURL(new RegExp(`/albums/${albumID}$`));
    await page.getByRole('link', { name: 'View fixture.png details' }).click();
    await expect(page.getByRole('heading', { name: 'fixture.png' })).toBeVisible();

    const detailPattern = new RegExp(`${mediaAPI}$`);
    let release!: () => void;
    const pending = new Promise<void>((resolve) => {
      release = resolve;
    });
    await page.route(detailPattern, async (route) => {
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
      const started = page.waitForRequest((entry) => new URL(entry.url()).pathname === mediaAPI);
      await page.evaluate(() => window.dispatchEvent(new Event('visibilitychange')));
      await started;
      await expect(page.getByText('Loading media details…')).toBeVisible();
      await expect(page.getByRole('heading', { name: 'fixture.png' })).toHaveCount(0);
      await expect(page.getByText('Encoded dimensions')).toHaveCount(0);
      release();
      await expect(
        page.getByText('Could not load this page. Check your connection and try again.'),
      ).toBeVisible();
      await expect(page.getByText('fixture.png')).toHaveCount(0);
      await expect(page.getByText('private path')).toHaveCount(0);
    } finally {
      release();
      await page.unroute(detailPattern);
    }
    await page.getByRole('button', { name: 'Try again' }).click();
    await expect(page.getByRole('heading', { name: 'fixture.png' })).toBeVisible();
    await expect(page.getByRole('main')).toBeFocused();

    await page.goto('/status');
    await page.goto(detailPath);
    await expect(page.getByRole('heading', { name: 'fixture.png' })).toBeVisible();
    const variantPattern = new RegExp(`${mediaAPI}/variants(?:\\?.*)?$`);
    await page.route(variantPattern, (route) =>
      route.fulfill({
        status: 404,
        contentType: 'application/problem+json',
        body: JSON.stringify({
          code: 'NOT_FOUND',
          status: 404,
          title: 'Not found',
          type: 'urn:test',
        }),
      }),
    );
    try {
      await page.reload();
      await expect(
        page.getByText('This media item is unavailable or you no longer have access.'),
      ).toBeVisible();
      await expect(page.getByRole('heading', { name: 'fixture.png' })).toHaveCount(0);
      await expect(page.getByText('Encoded dimensions')).toHaveCount(0);
      await expect(
        page.getByRole('alert').getByRole('link', { name: 'Back to albums' }),
      ).toHaveAttribute('href', '/albums');
    } finally {
      await page.unroute(variantPattern);
    }
    let releaseImage!: () => void;
    let imageRequested!: () => void;
    const imageBlocked = new Promise<void>((resolve) => {
      releaseImage = resolve;
    });
    const imageStarted = new Promise<void>((resolve) => {
      imageRequested = resolve;
    });
    await page.route(preview, async (route) => {
      imageRequested();
      await imageBlocked;
      await route.continue();
    });
    try {
      await page.goto('/status');
      await page.goto(detailPath);
      await expect(page.getByRole('heading', { name: 'fixture.png' })).toBeVisible();
      await imageStarted;
      const current = await api.get(membership);
      expect(current.status()).toBe(200);
      expect(
        (
          await api.put(membership, {
            headers: { ...headers, 'If-Match': current.headers().etag! },
            data: { group_ids: [] },
          })
        ).status(),
      ).toBe(200);
      const contentResponse = page.waitForResponse(
        (response) => new URL(response.url()).pathname === preview,
      );
      releaseImage();
      expect((await contentResponse).status()).toBe(404);
      await expect(
        page.getByText('This media item is unavailable or you no longer have access.'),
      ).toBeVisible();
      await expect(page.getByText('fixture.png')).toHaveCount(0);
      await expect(page.getByText('Encoded dimensions')).toHaveCount(0);
    } finally {
      releaseImage();
      await page.unroute(preview);
    }
    await page.reload();
    await expect(
      page.getByText('This media item is unavailable or you no longer have access.'),
    ).toBeVisible();
    expect((await page.request.get(mediaAPI)).status()).toBe(404);
    expect((await page.request.get(preview)).status()).toBe(404);
    expect(requests.some((entry) => new URL(entry).pathname.endsWith('/original'))).toBe(false);
    expect(errors).toEqual([]);
  } finally {
    const current = await api.get(membership);
    expect(current.status()).toBe(200);
    expect(
      (
        await api.put(membership, {
          headers: { ...headers, 'If-Match': current.headers().etag! },
          data: { group_ids: [] },
        })
      ).status(),
    ).toBe(200);
  }
});
