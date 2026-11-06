import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { Account, Administrator } from './fixtures';
import type { components, operations } from '../../web/src/lib/api/schema';

async function createGroup(admin: Administrator) {
  const response = await admin.api.post('/api/v2/admin/groups', {
    headers: { ...admin.headers, 'Idempotency-Key': randomUUID() },
    data: { name: `Media-${randomUUID()}` },
  });
  expect(response.status()).toBe(201);
  return ((await response.json()) as components['schemas']['Group']).id;
}

async function grant(admin: Administrator, account: Account, libraryID: string, groupID: string) {
  const path = `/api/v2/admin/users/${account.user.id}/groups`;
  const membership = await admin.api.get(path);
  expect(membership.status()).toBe(200);
  expect(
    (
      await admin.api.put(path, {
        headers: { ...admin.headers, 'If-Match': membership.headers().etag! },
        data: { group_ids: [groupID] },
      })
    ).status(),
  ).toBe(200);
  const access = `/api/v2/admin/libraries/${libraryID}/access`;
  const policy = await admin.api.get(access);
  expect(policy.status()).toBe(200);
  expect(
    (
      await admin.api.put(access, {
        headers: { ...admin.headers, 'If-Match': policy.headers().etag! },
        data: { group_ids: [groupID] },
      })
    ).status(),
  ).toBe(200);
}

async function removeGroup(admin: Administrator, id: string) {
  const path = `/api/v2/admin/groups/${id}`;
  const group = await admin.api.get(path);
  if (group.status() === 404) return;
  expect(group.status()).toBe(200);
  expect(
    (
      await admin.api.delete(path, {
        headers: { ...admin.headers, 'If-Match': group.headers().etag! },
      })
    ).status(),
  ).toBe(204);
}

test('real browser media decode, HEAD, single Range, and original policy share one authorization boundary', async ({
  account,
  administrator,
  page,
  context,
  request,
  browser,
}, testInfo) => {
  const prefix = `media-${testInfo.project.name}`;
  const mediaID = `${prefix}-media`;
  const variantURL = `/api/v2/media/${mediaID}/variants/${prefix}-media-variant/content`;
  const originalURL = `/api/v2/media/${mediaID}/original`;
  const groupID = await createGroup(administrator);
  try {
    await grant(administrator, account, `${prefix}-library`, groupID);
    await signIn(page, account);
    const anonymous = await request.get(variantURL, { headers: { Range: 'bytes=0-3' } });
    expect(anonymous.status()).toBe(401);
    expect((await request.head(variantURL)).status()).toBe(401);
    for (const method of ['get', 'head'] as const) {
      expect(
        (await context.request[method](originalURL, { headers: { Range: 'bytes=0-3' } })).status(),
      ).toBe(404);
    }
    const response = await context.request.get(variantURL);
    expect(response.status()).toBe(200);
    expect(response.headers()['content-type']).toBe('image/png');
    expect(response.headers()['cache-control']).toBe('private, no-store');
    expect(response.headers()['content-security-policy']).toContain('sandbox');
    const bytes = await response.body();
    expect([...bytes.subarray(0, 4)]).toEqual([137, 80, 78, 71]);
    const etag = response.headers().etag!;
    const decoded = await page.evaluate(async (url) => {
      const image = new Image();
      image.src = url;
      await image.decode();
      return { width: image.naturalWidth, height: image.naturalHeight };
    }, variantURL);
    expect(decoded).toEqual({ width: 1, height: 1 });
    const head = await context.request.head(variantURL, { headers: { Range: 'bytes=0-3' } });
    expect(head.status()).toBe(200);
    expect((await head.body()).byteLength).toBe(0);
    expect(Number(head.headers()['content-length'])).toBe(bytes.byteLength);
    const range = await context.request.get(variantURL, {
      headers: { Range: 'bytes=0-3', 'If-Range': etag },
    });
    expect(range.status()).toBe(206);
    expect([...(await range.body())]).toEqual([137, 80, 78, 71]);
    expect(range.headers()['content-range']).toBe(`bytes 0-3/${bytes.byteLength}`);
    const stale = await context.request.get(variantURL, {
      headers: { Range: 'bytes=0-3', 'If-Range': '"old"' },
    });
    expect(stale.status()).toBe(200);
    expect(await stale.body()).toEqual(bytes);
    const invalid = await context.request.get(variantURL, { headers: { Range: 'bytes=0-3,6-9' } });
    expect(invalid.status()).toBe(416);
    expect(invalid.headers()['cache-control']).toBe('private, no-store');
    expect(invalid.headers()['content-range']).toBe(`bytes */${bytes.byteLength}`);
    expect(await invalid.json()).toMatchObject({ code: 'INVALID_RANGE' });

    const otherContext = await browser.newContext({
      baseURL: 'https://127.0.0.1:4318',
      ignoreHTTPSErrors: true,
    });
    const otherResponse = await administrator.api.post('/api/v2/admin/users', {
      headers: { ...administrator.headers, 'Idempotency-Key': randomUUID() },
      data: { username: `other-${randomUUID()}`, password: account.password },
    });
    expect(otherResponse.status()).toBe(201);
    const otherUser = (await otherResponse.json()) as Account['user'];
    try {
      await signIn(await otherContext.newPage(), { user: otherUser, password: account.password });
      const denied = await otherContext.request.get(variantURL, {
        headers: { Range: 'bytes=0-3', 'If-None-Match': etag },
      });
      expect(denied.status()).toBe(404);
      expect(denied.headers().etag).toBeUndefined();
    } finally {
      await otherContext.close();
      const current = await administrator.api.get(`/api/v2/admin/users/${otherUser.id}`);
      expect(current.status()).toBe(200);
      expect(
        (
          await administrator.api.delete(`/api/v2/admin/users/${otherUser.id}`, {
            headers: { ...administrator.headers, 'If-Match': current.headers().etag! },
          })
        ).status(),
      ).toBe(204);
    }

    const userPath = `/api/v2/admin/users/${account.user.id}`;
    const current = await administrator.api.get(userPath);
    expect(
      (
        await administrator.api.patch(userPath, {
          headers: { ...administrator.headers, 'If-Match': current.headers().etag! },
          data: { download_original: true },
        })
      ).status(),
    ).toBe(200);
    await page.reload();
    await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
    await signIn(page, account);
    const original = await context.request.get(originalURL);
    expect(original.status()).toBe(200);
    expect(original.headers()['content-disposition']).toContain('attachment');
    expect(await original.body()).toEqual(bytes);
    const originalHead = await context.request.head(originalURL);
    expect(originalHead.status()).toBe(200);
    expect((await originalHead.body()).byteLength).toBe(0);
    const originalRange = await context.request.get(originalURL, {
      headers: { Range: 'bytes=-4', 'If-Range': etag },
    });
    expect(originalRange.status()).toBe(206);
    expect(await originalRange.body()).toEqual(bytes.subarray(-4));
    const notModified = await context.request.get(originalURL, {
      headers: { 'If-None-Match': etag },
    });
    expect(notModified.status()).toBe(304);
    expect((await notModified.body()).byteLength).toBe(0);
    const membershipPath = `${userPath}/groups`;
    const membership = await administrator.api.get(membershipPath);
    expect(
      (
        await administrator.api.put(membershipPath, {
          headers: { ...administrator.headers, 'If-Match': membership.headers().etag! },
          data: { group_ids: [] },
        })
      ).status(),
    ).toBe(200);
    for (const url of [variantURL, originalURL]) {
      expect(
        (
          await context.request.get(url, { headers: { Range: 'bytes=0-3', 'If-None-Match': etag } })
        ).status(),
      ).toBe(404);
      expect((await context.request.head(url)).status()).toBe(404);
    }
    expect((await context.request.get('/api/v2/auth/me')).status()).toBe(200);
  } finally {
    await removeGroup(administrator, groupID);
  }
});

test('an unavailable source preserves private ready previews and rejects old or mismatched variant IDs', async ({
  account,
  administrator,
  page,
  context,
}, testInfo) => {
  const prefix = `outage-${testInfo.project.name}`;
  const mediaID = `${prefix}-media`;
  const groupID = await createGroup(administrator);
  try {
    await grant(administrator, account, `${prefix}-library`, groupID);
    await signIn(page, account);
    const variantURL = `/api/v2/media/${mediaID}/variants/${prefix}-media-variant/content`;
    const ready = await context.request.get(variantURL);
    expect(ready.status()).toBe(200);
    const metadata = await context.request.get(`/api/v2/media/${mediaID}/variants`);
    expect(metadata.status()).toBe(200);
    const variants = (await metadata.json()) as components['schemas']['VariantPage'];
    expect(variants.items).toHaveLength(1);
    expect(variants.items[0]!.content_url).toBe(variantURL);
    expect(await metadata.text()).not.toContain('storage_path');
    expect(await metadata.text()).not.toContain('/tmp/');
    const original = await administrator.api.get(`/api/v2/media/${mediaID}/original`);
    expect(original.status()).toBe(503);
    expect(await original.json()).toMatchObject({ code: 'SOURCE_UNAVAILABLE' });
    for (const variantID of [`${prefix}-media-old-variant`, `${prefix}-other-media-variant`]) {
      const denied = await context.request.get(
        `/api/v2/media/${mediaID}/variants/${variantID}/content`,
        { headers: { Range: 'bytes=0-3' } },
      );
      expect(denied.status()).toBe(404);
    }
    const ownOther = await context.request.get(
      `/api/v2/media/${prefix}-other-media/variants/${prefix}-other-media-variant/content`,
    );
    expect(ownOther.status()).toBe(200);
  } finally {
    await removeGroup(administrator, groupID);
  }
});

test('HTTPS album media filters and chronological keysets retain ties, missing capture and fresh grants', async ({
  account,
  administrator,
  page,
  context,
  request,
}, testInfo) => {
  const prefix = `media-selection-${testInfo.project.name}`;
  const path = `/api/v2/albums/${prefix}-album/media`;
  type Selection = NonNullable<operations['listAlbumMedia']['parameters']['query']>;
  const queryString = (selection: Selection) =>
    new URLSearchParams(
      Object.entries(selection).map(([key, value]) => [key, String(value)]),
    ).toString();
  const selected = async (selection: Selection = {}) => {
    const response = await context.request.get(`${path}?${queryString(selection)}`);
    expect(response.status()).toBe(200);
    expect(response.headers()['cache-control']).toBe('private, no-store');
    return (await response.json()) as components['schemas']['MediaPage'];
  };
  expect((await request.get(path)).status()).toBe(401);
  await signIn(page, account);
  expect((await context.request.get(path)).status()).toBe(404);
  const groupID = await createGroup(administrator);
  try {
    await grant(administrator, account, `${prefix}-library`, groupID);
    const first = await selected({ sort: 'captured_desc', limit: 1 });
    expect(first.items[0]).toMatchObject({
      id: `${prefix}-c`,
      capture_at: '2026-11-06T01:02:03.124Z',
      revision: null,
      offline_capable: false,
    });
    expect(first.has_more).toBe(true);
    expect(first.next_cursor).not.toBeNull();
    const ids: string[] = [];
    let current = first;
    for (let index = 0; index < 6; index++) {
      ids.push(...current.items.map((item) => item.id));
      if (!current.next_cursor) break;
      current = await selected({
        sort: 'captured_desc',
        cursor: current.next_cursor,
        limit: 1,
      });
    }
    expect(ids).toEqual(
      ['c', 'b', 'a', 'other-media', 'media'].map((suffix) => `${prefix}-${suffix}`),
    );
    expect(current).toMatchObject({ has_more: false, next_cursor: null });
    expect(current.items[0]!.capture_at).toBeNull();
    const ascending = await selected({ sort: 'captured_asc', limit: 2 });
    expect(ascending.items.map((item) => item.id)).toEqual([
      `${prefix}-media`,
      `${prefix}-other-media`,
    ]);
    expect(ascending.items.every((item) => item.capture_at === null)).toBe(true);
    const interval = await selected({
      kind: 'image',
      captured_from: '2026-11-06T01:02:03.123Z',
      captured_before: '2026-11-06T01:02:03.124Z',
      sort: 'captured_desc',
    });
    expect(interval.items.map((item) => item.id)).toEqual([`${prefix}-a`]);
    const video = await selected({ kind: 'video', sort: 'id_desc' });
    expect(video.items.map((item) => item.id)).toEqual([`${prefix}-b`]);
    for (const selection of [
      { cursor: first.next_cursor! },
      { sort: 'captured_asc', cursor: first.next_cursor! },
      { sort: 'captured_desc', kind: 'image', cursor: first.next_cursor! },
    ] satisfies Selection[]) {
      const invalid = await context.request.get(`${path}?${queryString(selection)}`);
      expect(invalid.status()).toBe(400);
      expect(await invalid.json()).toMatchObject({ code: 'INVALID_CURSOR' });
    }
    const accountMismatch = await administrator.api.get(
      `${path}?${queryString({ sort: 'captured_desc', cursor: first.next_cursor! })}`,
    );
    expect(accountMismatch.status()).toBe(400);
    expect(await accountMismatch.json()).toMatchObject({ code: 'INVALID_CURSOR' });
    const unsupported = await context.request.get(`${path}?q=not-a-search`);
    expect(unsupported.status()).toBe(400);
    expect(await unsupported.json()).toMatchObject({ code: 'INVALID_FILTER' });
    expect((await context.request.get(`/api/v2/media/${prefix}-media/original`)).status()).toBe(
      404,
    );
    expect(JSON.stringify({ first, ascending, interval, video })).not.toMatch(
      /source_sha256|root_path|relative_path|storage_path|\/tmp\/|GPS/,
    );
    await removeGroup(administrator, groupID);
    expect((await context.request.get(path)).status()).toBe(404);
    const stale = await context.request.get(
      `${path}?${queryString({ sort: 'captured_desc', cursor: first.next_cursor! })}`,
    );
    expect(stale.status()).toBe(400);
    expect(await stale.json()).toMatchObject({ code: 'INVALID_CURSOR' });
  } finally {
    await removeGroup(administrator, groupID);
  }
});
