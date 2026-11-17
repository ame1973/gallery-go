import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('HTTPS random home samples apply fresh permissions before the bounded draw', async ({
  account,
  administrator,
  page,
  context,
  request,
}, testInfo) => {
  const prefix = `random-home-${testInfo.project.name}`;
  const path = '/api/v2/home/random-albums';
  const membership = `/api/v2/admin/users/${account.user.id}/groups`;
  const { api, headers } = administrator;

  expect((await request.get(path)).status()).toBe(401);
  await signIn(page, account);
  const cookie = (await context.cookies()).find(
    (value) => value.name === '__Host-gallerygo_session',
  );
  expect(await (await context.request.get(path)).json()).toEqual({ items: [] });

  let policy = await api.get(membership);
  expect(policy.status()).toBe(200);
  expect(
    (
      await api.put(membership, {
        headers: { ...headers, 'If-Match': policy.headers().etag! },
        data: { group_ids: [`${prefix}-group`] },
      })
    ).status(),
  ).toBe(200);
  const viewsPath = `/api/v2/albums/${prefix}-album/views`;
  const before = await context.request.get(viewsPath);
  expect(before.status()).toBe(200);
  const beforeViews = await before.json();
  const response = await context.request.get(`${path}?limit=30`);
  expect(response.status()).toBe(200);
  expect(response.headers()['cache-control']).toBe('private, no-store');
  const sample = (await response.json()) as components['schemas']['AlbumSample'];
  expect(Object.keys(sample)).toEqual(['items']);
  expect(sample.items).toHaveLength(30);
  expect(new Set(sample.items.map((album) => album.id)).size).toBe(30);
  for (const album of sample.items) {
    expect(album.id.startsWith(`${prefix}-`)).toBe(true);
    expect(album.id).not.toBe(`${prefix}-hidden`);
    expect(album.library_id).toBe(`${prefix}-library`);
  }
  for (const album of sample.items.slice(0, 3)) {
    const detail = await context.request.get(`/api/v2/albums/${album.id}`);
    expect(detail.status()).toBe(200);
    expect(await detail.json()).toEqual(album);
  }
  const one = (await (
    await context.request.get(`${path}?limit=1`)
  ).json()) as components['schemas']['AlbumSample'];
  expect(one.items).toHaveLength(1);
  const all = (await (
    await context.request.get(`${path}?limit=100`)
  ).json()) as components['schemas']['AlbumSample'];
  expect(all.items).toHaveLength(34);
  expect(all.items.map((album) => album.id)).not.toContain(`${prefix}-hidden`);
  for (const query of ['limit=0', 'limit=101', 'limit=1&limit=2', 'cursor=bad', 'sort=latest']) {
    expect((await context.request.get(`${path}?${query}`)).status()).toBe(400);
  }
  expect(await (await context.request.get(viewsPath)).json()).toEqual(beforeViews);

  policy = await api.get(membership);
  expect(
    (
      await api.put(membership, {
        headers: { ...headers, 'If-Match': policy.headers().etag! },
        data: { group_ids: [] },
      })
    ).status(),
  ).toBe(200);
  expect(await (await context.request.get(path)).json()).toEqual({ items: [] });
  expect(
    (await context.cookies()).find((value) => value.name === '__Host-gallerygo_session')!.value,
  ).toBe(cookie!.value);
});
