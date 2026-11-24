import { randomBytes, randomUUID } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('an explicit original download remains bound to the page account', async ({
  account,
  administrator,
  page,
}, testInfo) => {
  const prefix = `media-detail-ui-${testInfo.project.name}`;
  const mediaID = `${prefix}-unprocessed-media`;
  const revisionID = `${prefix}-unprocessed-revision`;
  const mediaPath = `/api/v2/media/${mediaID}`;
  const groupID = `${prefix}-group`;
  const { api, headers } = administrator;
  const originalRequests: string[] = [];
  page.on('request', (request) => {
    if (new URL(request.url()).pathname === `${mediaPath}/original`) {
      originalRequests.push(request.url());
    }
  });
  async function addGroup(userID: string) {
    const path = `/api/v2/admin/users/${userID}/groups`;
    const current = await api.get(path);
    expect(current.status()).toBe(200);
    const changed = await api.put(path, {
      headers: { ...headers, 'If-Match': current.headers().etag! },
      data: { group_ids: [groupID] },
    });
    expect(changed.status()).toBe(200);
  }
  await addGroup(account.user.id);
  await signIn(page, account);
  await page.goto(`/inspect/${mediaID}/details`);
  await expect(page.getByRole('heading', { name: 'unprocessed.png' })).toBeVisible();
  await expect(page.getByText('Preview not shown')).toBeVisible();
  const metadata = await page.request.get(mediaPath);
  expect(metadata.status()).toBe(200);
  expect(await metadata.json()).toMatchObject({ id: mediaID, revision: revisionID });
  const variants = await page.request.get(`${mediaPath}/variants`);
  expect(variants.status()).toBe(200);
  expect(await variants.json()).toMatchObject({ items: [] });
  await expect(page.getByRole('link', { name: 'Download original' })).toHaveCount(0);
  expect(originalRequests).toEqual([]);
  expect(
    (
      await page.request.get(
        `${mediaPath}/original?expected_user_id=${account.user.id}&expected_revision_id=${revisionID}`,
      )
    ).status(),
  ).toBe(404);

  const accountPath = `/api/v2/admin/users/${account.user.id}`;
  const current = await api.get(accountPath);
  expect(current.status()).toBe(200);
  const enabled = await api.patch(accountPath, {
    headers: { ...headers, 'If-Match': current.headers().etag! },
    data: { download_original: true },
  });
  expect(enabled.status()).toBe(200);
  await signIn(page, account);
  await page.goto(`/inspect/${mediaID}/details`);
  const link = page.getByRole('link', { name: 'Download original' });
  await expect(link).toBeVisible();
  await expect(link).toHaveAttribute(
    'href',
    `${mediaPath}/original?expected_user_id=${account.user.id}&expected_revision_id=${revisionID}`,
  );
  await expect(
    page.getByText('Original files may contain private metadata, including location.'),
  ).toBeVisible();
  const staleRevision = await page.request.get(
    `${mediaPath}/original?expected_user_id=${account.user.id}&expected_revision_id=old-revision`,
  );
  expect(staleRevision.status()).toBe(404);
  expect(staleRevision.headers().etag).toBeUndefined();
  expect(originalRequests).toEqual([]);
  const target = await link.boundingBox();
  expect(target?.height).toBeGreaterThanOrEqual(44);
  const downloadReady = page.waitForEvent('download');
  await link.click();
  const download = await downloadReady;
  expect(await download.failure()).toBeNull();
  expect(download.suggestedFilename()).toBe('unprocessed.png');
  const saved = await download.path();
  expect(saved).not.toBeNull();
  expect(await readFile(saved!)).toEqual(
    Buffer.from(
      'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAAEElEQVR4nGKSq9gCCAAA//8CCgFNa0v7mgAAAABJRU5ErkJggg==',
      'base64',
    ),
  );

  const membership = `/api/v2/admin/users/${account.user.id}/groups`;
  const groupBefore = await api.get(membership);
  expect(groupBefore.status()).toBe(200);
  expect(
    (
      await api.put(membership, {
        headers: { ...headers, 'If-Match': groupBefore.headers().etag! },
        data: { group_ids: [] },
      })
    ).status(),
  ).toBe(200);
  const deniedAfterRevoke = await page.request.get(
    `${mediaPath}/original?expected_user_id=${account.user.id}&expected_revision_id=${revisionID}`,
  );
  expect(deniedAfterRevoke.status()).toBe(404);
  const groupAfter = await api.get(membership);
  expect(groupAfter.status()).toBe(200);
  expect(
    (
      await api.put(membership, {
        headers: { ...headers, 'If-Match': groupAfter.headers().etag! },
        data: { group_ids: [groupID] },
      })
    ).status(),
  ).toBe(200);

  const password = randomBytes(24).toString('base64url');
  const created = await api.post('/api/v2/admin/users', {
    headers: { ...headers, 'Idempotency-Key': randomUUID() },
    data: { username: `download-${randomUUID()}`, password, download_original: true },
  });
  expect(created.status()).toBe(201);
  const other = (await created.json()) as components['schemas']['User'];
  const otherPath = `/api/v2/admin/users/${other.id}`;
  try {
    await addGroup(other.id);
    const login = await page.request.post('/api/v2/auth/login', {
      headers: { Origin: new URL(page.url()).origin },
      data: { username: other.username, password },
    });
    expect(login.status()).toBe(200);
    expect(await (await page.request.get('/api/v2/auth/me')).json()).toMatchObject({
      id: other.id,
    });
    const own = await page.request.get(
      `${mediaPath}/original?expected_user_id=${other.id}&expected_revision_id=${revisionID}`,
    );
    expect(own.status()).toBe(200);
    const rejected = page.waitForResponse(
      (response) => new URL(response.url()).pathname === `${mediaPath}/original`,
    );
    await link.click();
    const response = await rejected;
    expect(response.status()).toBe(404);
    expect(response.headers().etag).toBeUndefined();
    expect(await response.json()).toMatchObject({ code: 'NOT_FOUND' });
  } finally {
    const latest = await api.get(otherPath);
    if (latest.status() !== 404) {
      expect(latest.status()).toBe(200);
      expect(
        (
          await api.delete(otherPath, {
            headers: { ...headers, 'If-Match': latest.headers().etag! },
          })
        ).status(),
      ).toBe(204);
    }
  }
});
