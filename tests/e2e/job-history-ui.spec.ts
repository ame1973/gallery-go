import type { Page } from '@playwright/test';
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

function responseFor(page: Page, path: string, cursor: string) {
  return page.waitForResponse((response) => {
    const url = new URL(response.url());
    return url.pathname === path && url.searchParams.get('cursor') === cursor;
  });
}

test('administrators browse live job and history pages over HTTPS', async ({
  account,
  administrator,
  page,
  request,
}, testInfo) => {
  test.setTimeout(90_000);
  const prefix = `history-${testInfo.project.name}`;
  const id = `${prefix}-zz.with.dot`;
  const apiPath = `/api/v2/admin/jobs/${id}`;
  const detailPath = `/admin/jobs/${id}/details`;
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  page.on('console', (message) => {
    if (message.text().includes('Content Security Policy')) errors.push(message.text());
  });

  expect((await request.get('/api/v2/admin/jobs')).status()).toBe(401);
  await page.goto(detailPath);
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
  await changeRole(administrator, account, 'admin');
  await signIn(page, account);
  await page
    .getByRole('navigation', { name: 'Gallery navigation' })
    .getByRole('link', { name: 'Jobs' })
    .click();
  await expect(page.getByRole('heading', { name: 'Jobs', exact: true })).toBeVisible();
  await page.getByRole('combobox', { name: 'Status' }).selectOption('dead');
  await expect(page.getByRole('link', { name: 'View job history-chromium-00' })).toBeVisible();
  await expect(page.locator('.job-list > li')).toHaveCount(30);
  await expect(
    page.getByText(
      'Counts include all attempts. Finished includes succeeded, failed, skipped and cancelled work; it does not mean success.',
    ),
  ).toBeVisible();
  const listNavigation = page.getByRole('navigation', { name: 'Page navigation' });
  const firstNext = await listNavigation
    .getByRole('link', { name: 'Next page' })
    .getAttribute('href');
  const listCursor = new URL(firstNext!, 'https://127.0.0.1:4318').searchParams.get('cursor')!;
  expect(listCursor).toBeTruthy();
  const wrongFilter = await page.request.get(
    `/api/v2/admin/jobs?status=queued&cursor=${encodeURIComponent(listCursor)}`,
  );
  expect(wrongFilter.status()).toBe(400);
  expect(await wrongFilter.json()).toMatchObject({ code: 'INVALID_CURSOR' });
  const wrongActor = await administrator.api.get(
    `/api/v2/admin/jobs?status=dead&cursor=${encodeURIComponent(listCursor)}`,
  );
  expect(wrongActor.status()).toBe(400);
  expect(await wrongActor.json()).toMatchObject({ code: 'INVALID_CURSOR' });

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
        '.job-entry-link span',
        '.job-status',
      ].map((selector) => {
        const foreground = luminance(getComputedStyle(document.querySelector(selector)!).color);
        return (
          (Math.max(background, foreground) + 0.05) / (Math.min(background, foreground) + 0.05)
        );
      });
    });
    for (const ratio of contrast) expect(ratio).toBeGreaterThanOrEqual(4.5);
    for (const target of [
      page
        .getByRole('navigation', { name: 'Gallery navigation' })
        .getByRole('link', { name: 'Jobs' }),
      page.getByRole('combobox', { name: 'Status' }),
      page.getByRole('button', { name: 'Refresh' }),
      listNavigation.getByRole('link', { name: 'Next page' }),
    ]) {
      const box = await target.boundingBox();
      expect(box?.width).toBeGreaterThanOrEqual(44);
      expect(box?.height).toBeGreaterThanOrEqual(44);
    }
    await page.screenshot({ path: testInfo.outputPath(`job-list-${width}-${scheme}.png`) });
  }
  await page.getByRole('combobox', { name: 'Language' }).selectOption('zh-TW');
  await expect(page.getByRole('heading', { name: '任務', exact: true })).toBeVisible();
  await page.getByRole('combobox', { name: '語言' }).selectOption('zh-CN');
  await expect(page.getByRole('heading', { name: '任务', exact: true })).toBeVisible();
  await page.getByRole('combobox', { name: '语言' }).selectOption('en');

  const target = page.getByRole('link', { name: `View job ${id}` });
  for (let pageNumber = 0; pageNumber < 4 && (await target.count()) === 0; pageNumber += 1) {
    const next = page
      .getByRole('navigation', { name: 'Page navigation' })
      .getByRole('link', { name: 'Next page' });
    await expect(next).toBeVisible();
    const href = await next.getAttribute('href');
    const cursor = new URL(href!, 'https://127.0.0.1:4318').searchParams.get('cursor')!;
    const response = responseFor(page, '/api/v2/admin/jobs', cursor);
    await next.click();
    const result = (await (await response).json()) as components['schemas']['JobPage'];
    await expect(page.getByRole('link', { name: `View job ${result.items[0]!.id}` })).toBeVisible();
    await expect(page.getByRole('main')).toBeFocused();
  }
  await expect(target).toBeVisible();
  const returnURL = new URL(page.url());
  await target.click();
  await expect(page).toHaveURL(new RegExp(`${id.replaceAll('.', '\\.')}/details$`));
  await expect(page.getByRole('heading', { name: `Scan ${id}` })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Back to jobs' })).toHaveAttribute(
    'href',
    returnURL.pathname + returnURL.search,
  );
  const summary = page.locator('.job-section').first();
  await expect(summary.locator('.job-counts > div').nth(0)).toHaveText(/Discovered\s*93/);
  await expect(summary.locator('.job-counts > div').nth(1)).toHaveText(/Finished\s*93/);
  await expect(summary.locator('.job-counts > div').nth(2)).toHaveText(/Succeeded\s*31/);
  await expect(summary.locator('.job-counts > div').nth(3)).toHaveText(/Failed\s*31/);
  await expect(summary.locator('.job-counts > div').nth(4)).toHaveText(/Skipped\s*31/);
  await expect(summary.locator('.job-counts > div').nth(5)).toHaveText(/Cancelled\s*0/);
  await expect(
    page.getByText('Each attempt shows its own work units, not cumulative job counts.'),
  ).toBeVisible();
  await expect(
    page.getByText(
      'These are historical diagnostics, not a count of failed work or proof that a source is currently available.',
    ),
  ).toBeVisible();
  const attempts = page
    .locator('section')
    .filter({ has: page.getByRole('heading', { name: 'Attempts', exact: true }) });
  const issues = page
    .locator('section')
    .filter({ has: page.getByRole('heading', { name: 'Scan diagnostics', exact: true }) });
  await expect(attempts.locator('ol > li')).toHaveCount(30);
  await expect(issues.locator('ol > li')).toHaveCount(30);
  await expect(issues.getByText('Album/entry-00.png')).toBeVisible();
  await expect(issues.getByText(`${prefix}-source`).first()).toBeVisible();
  expect(await (await page.request.get(apiPath)).text()).not.toContain('private_path');
  await expect(page.getByText('private_path')).toHaveCount(0);
  await page.screenshot({ path: testInfo.outputPath('job-details.png') });

  const attemptNavigation = page.getByRole('navigation', { name: 'Attempt pages' });
  const attemptHref = await attemptNavigation
    .getByRole('link', { name: 'Next page' })
    .getAttribute('href');
  const attemptCursor = new URL(attemptHref!, 'https://127.0.0.1:4318').searchParams.get(
    'attempt_cursor',
  )!;
  const wrongHistory = await page.request.get(
    `${apiPath}/issues?cursor=${encodeURIComponent(attemptCursor)}`,
  );
  expect(wrongHistory.status()).toBe(400);
  await attemptNavigation.getByRole('link', { name: 'Next page' }).click();
  await expect(attempts.getByRole('heading', { name: 'Attempt 31', exact: true })).toBeVisible();
  await expect(page.getByRole('main')).toBeFocused();
  await expect(issues.getByText('Album/entry-00.png')).toBeVisible();
  const issueNavigation = page.getByRole('navigation', { name: 'Diagnostic pages' });
  const issueHref = await issueNavigation
    .getByRole('link', { name: 'Next page' })
    .getAttribute('href');
  expect(new URL(issueHref!, 'https://127.0.0.1:4318').searchParams.get('attempt_cursor')).toBe(
    attemptCursor,
  );
  await issueNavigation.getByRole('link', { name: 'Next page' }).click();
  await expect(issues.getByText('Album/entry-30.png')).toBeVisible();
  await expect(issues.getByText('Album/entry-00.png')).toHaveCount(0);
  await expect(issues.getByText('../outside-private-path')).toHaveCount(0);
  await expect(attempts.getByRole('heading', { name: 'Attempt 31', exact: true })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Back to jobs' })).toHaveAttribute(
    'href',
    returnURL.pathname + returnURL.search,
  );
  await attemptNavigation.getByRole('link', { name: 'First page' }).click();
  await expect(attempts.getByRole('heading', { name: 'Attempt 1', exact: true })).toBeVisible();
  await expect(issues.getByText('Album/entry-30.png')).toBeVisible();

  let release!: () => void;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  const isIssues = (url: URL) => url.pathname === `${apiPath}/issues`;
  await page.route(isIssues, async (route) => {
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
    const pending = page.waitForRequest(
      (entry) => new URL(entry.url()).pathname === `${apiPath}/issues`,
    );
    await page.getByRole('button', { name: 'Refresh' }).click();
    await pending;
    await expect(page.getByText('Loading job history…')).toBeVisible();
    await expect(page.getByText(`${prefix}-source`)).toHaveCount(0);
    await expect(page.getByText('Album/entry-30.png')).toHaveCount(0);
    release();
    await expect(
      page.getByText('Could not load job history. Check your connection and try again.'),
    ).toBeVisible();
    await expect(page.getByText('/private/secret')).toHaveCount(0);
    await expect(page.getByText('Album/entry-30.png')).toHaveCount(0);
  } finally {
    release();
    await page.unroute(isIssues);
  }
  await page.getByRole('button', { name: 'Try again' }).click();
  await expect(issues.getByText('Album/entry-30.png')).toBeVisible();
  await expect(page.getByRole('main')).toBeFocused();
  expect(errors).toEqual([]);
});

test('role revocation hides held history and invalidates old scoped cursors', async ({
  account,
  administrator,
  page,
  request,
}, testInfo) => {
  test.setTimeout(90_000);
  const prefix = `history-${testInfo.project.name}`;
  const id = `${prefix}-zz.with.dot`;
  const apiPath = `/api/v2/admin/jobs/${id}`;
  await page.goto('/admin/jobs');
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
  expect((await request.get(apiPath)).status()).toBe(401);
  await signIn(page, account);
  await page.goto('/admin/jobs');
  await expect(
    page.getByText('Administrator access is required to view job history.'),
  ).toBeVisible();
  await expect(page.getByRole('link', { name: 'Jobs', exact: true })).toHaveCount(0);
  expect((await page.request.get(apiPath)).status()).toBe(403);

  const promoted = await changeRole(administrator, account, 'admin');
  expect(promoted.scope_id).not.toBe(account.user.scope_id);
  await page.reload();
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
  await signIn(page, account);
  const list = await page.request.get('/api/v2/admin/jobs?status=dead&limit=30');
  expect(list.status()).toBe(200);
  const cursor = ((await list.json()) as components['schemas']['JobPage']).next_cursor!;
  expect(cursor).toBeTruthy();
  await page.goto(`/admin/jobs/${prefix}-running/details`);
  await expect(
    page.getByText('Cancellation has been requested; this job is still running.'),
  ).toBeVisible();
  await expect(page.getByText('The total is not known yet and may increase.')).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Scan diagnostics', exact: true })).toBeVisible();
  await page.goto(`/admin/jobs/${id}/details`);
  await expect(page.getByText('Album/entry-00.png')).toBeVisible();

  let release!: () => void;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  const isIssues = (url: URL) => url.pathname === `${apiPath}/issues`;
  await page.route(isIssues, async (route) => {
    await held;
    await route.continue();
  });
  try {
    const pending = page.waitForRequest(
      (entry) => new URL(entry.url()).pathname === `${apiPath}/issues`,
    );
    const denied = page.waitForResponse(
      (response) =>
        new URL(response.url()).pathname === `${apiPath}/issues` && response.status() === 401,
    );
    await page.getByRole('button', { name: 'Refresh' }).click();
    await pending;
    await expect(page.getByText('Loading job history…')).toBeVisible();
    await expect(page.getByText('Album/entry-00.png')).toHaveCount(0);
    const demoted = await changeRole(administrator, account, 'user');
    expect(demoted.scope_id).not.toBe(promoted.scope_id);
    release();
    expect((await denied).status()).toBe(401);
    await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
    await expect(page.getByText('Album/entry-00.png')).toHaveCount(0);
    await expect(page.getByText(`${prefix}-source`)).toHaveCount(0);
  } finally {
    release();
    await page.unroute(isIssues);
  }
  await signIn(page, account);
  const jobRequests: string[] = [];
  page.on('request', (entry) => {
    if (new URL(entry.url()).pathname.startsWith('/api/v2/admin/jobs'))
      jobRequests.push(entry.url());
  });
  await page.goto(`/admin/jobs/${id}/details`);
  await expect(
    page.getByText('Administrator access is required to view job history.'),
  ).toBeVisible();
  expect(jobRequests).toEqual([]);
  expect((await page.request.get(apiPath)).status()).toBe(403);

  const restored = await changeRole(administrator, account, 'admin');
  expect(restored.scope_id).not.toBe(promoted.scope_id);
  await signIn(page, account);
  await page.goto(`/admin/jobs?status=dead&cursor=${encodeURIComponent(cursor)}`);
  await expect(
    page.getByText('This page link is invalid or expired. Return to the first page.'),
  ).toBeVisible();
  await expect(page.locator('.job-list > li')).toHaveCount(0);
  await page.getByRole('link', { name: 'First page' }).click();
  await expect(page.getByRole('link', { name: 'View job history-chromium-00' })).toBeVisible();
});
