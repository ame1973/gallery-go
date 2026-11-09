import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('HTTPS personal collections keep owned hierarchy, receipts and favorite assignments separate from grants', async ({
  account,
  administrator,
  page,
  context,
  request,
}, testInfo) => {
  const prefix = `favorite-collections-${testInfo.project.name}`;
  const album = `${prefix}-album`;
  const { api, headers } = administrator;
  const createdGroup = await api.post('/api/v2/admin/groups', {
    headers: { ...headers, 'Idempotency-Key': randomUUID() },
    data: { name: `Collection readers-${randomUUID()}` },
  });
  expect(createdGroup.status()).toBe(201);
  const group = (await createdGroup.json()) as components['schemas']['Group'];
  const path = '/api/v2/favorite-collections';
  type Collection = components['schemas']['FavoriteCollection'];
  type Page = components['schemas']['FavoriteCollectionPage'];
  try {
    expect((await request.get(path)).status()).toBe(401);
    await signIn(page, account);
    for (const grant of [
      `/api/v2/admin/libraries/${prefix}-library/access`,
      `/api/v2/admin/users/${account.user.id}/groups`,
    ]) {
      const current = await api.get(grant);
      expect(current.status()).toBe(200);
      expect(
        (
          await api.put(grant, {
            headers: { ...headers, 'If-Match': current.headers().etag! },
            data: { group_ids: [group.id] },
          })
        ).status(),
      ).toBe(200);
    }
    const csrf = (await (await context.request.get('/api/v2/auth/csrf')).json()) as {
      token: string;
    };
    const ownHeaders = { Origin: headers.Origin, 'X-CSRF-Token': csrf.token };
    const createKey = randomUUID();
    const rootResponse = await context.request.post(path, {
      headers: { ...ownHeaders, 'Idempotency-Key': createKey },
      data: { name: '  Trips  ' },
    });
    expect(rootResponse.status()).toBe(201);
    expect(rootResponse.headers()['cache-control']).toBe('private, no-store');
    const root = (await rootResponse.json()) as Collection;
    expect(root).toMatchObject({ name: 'Trips', parent_id: null, version: 1 });
    expect(root.created_at).toBe(root.updated_at);
    expect(rootResponse.headers().location).toBe(`${path}/${root.id}`);
    expect((await api.get(`${path}/${root.id}`)).status()).toBe(404);
    const childResponse = await context.request.post(path, {
      headers: { ...ownHeaders, 'Idempotency-Key': randomUUID() },
      data: { name: 'Summer', parent_id: root.id },
    });
    expect(childResponse.status()).toBe(201);
    const child = (await childResponse.json()) as Collection;
    expect(child).toMatchObject({ name: 'Summer', parent_id: root.id, version: 1 });
    const secondRoot = await context.request.post(path, {
      headers: { ...ownHeaders, 'Idempotency-Key': randomUUID() },
      data: { name: 'Other', parent_id: null },
    });
    expect(secondRoot.status()).toBe(201);
    const children = await context.request.get(`${path}?parent_id=${encodeURIComponent(root.id)}`);
    expect(children.status()).toBe(200);
    expect((await children.json()) as Page).toMatchObject({
      items: [{ id: child.id, parent_id: root.id }],
      has_more: false,
      next_cursor: null,
    });
    const first = await context.request.get(`${path}?root=true&limit=1`);
    expect(first.status()).toBe(200);
    const firstPage = (await first.json()) as Page;
    expect(firstPage.items).toHaveLength(1);
    expect(firstPage.next_cursor).not.toBeNull();
    const continued = new URLSearchParams({
      root: 'true',
      limit: '100',
      cursor: firstPage.next_cursor!,
    });
    const last = await context.request.get(`${path}?${continued}`);
    expect(last.status()).toBe(200);
    const lastPage = (await last.json()) as Page;
    expect(lastPage.items).toHaveLength(1);
    expect(lastPage.has_more).toBe(false);
    expect((await api.get(`${path}?${continued}`)).status()).toBe(400);
    expect(
      (
        await context.request.get(
          `${path}?parent_id=${encodeURIComponent(root.id)}&cursor=${encodeURIComponent(firstPage.next_cursor!)}`,
        )
      ).status(),
    ).toBe(400);

    const favoritePath = `/api/v2/favorites/${album}`;
    const initial = await context.request.get(favoritePath);
    expect(initial.status()).toBe(200);
    const albumBefore = await (await context.request.get(`/api/v2/albums/${album}`)).json();
    const favoriteKey = randomUUID();
    const favorite = await context.request.put(favoritePath, {
      headers: {
        ...ownHeaders,
        'If-Match': initial.headers().etag!,
        'Idempotency-Key': favoriteKey,
      },
      data: { collection_id: child.id },
    });
    expect(favorite.status()).toBe(200);
    expect(await favorite.json()).toMatchObject({ favorited: true, collection_id: child.id });
    const albums = async (selector: Record<string, string>) => {
      const query = new URLSearchParams({ library_id: `${prefix}-library`, ...selector });
      const response = await context.request.get(`/api/v2/albums?${query}`);
      expect(response.status()).toBe(200);
      expect(response.headers()['cache-control']).toBe('private, no-store');
      return (await response.json()) as components['schemas']['AlbumPage'];
    };
    expect(
      (await albums({ favorite_collection_id: child.id })).items.map((item) => item.id),
    ).toEqual([album]);
    expect((await albums({ favorite_collection_id: root.id })).items).toEqual([]);
    expect((await albums({ favorite_unfiled: 'true' })).items).toEqual([]);
    expect((await albums({ favorite_collection_id: randomUUID() })).items).toEqual([]);
    expect(
      (
        (await (
          await api.get(`/api/v2/albums?favorite_collection_id=${child.id}`)
        ).json()) as components['schemas']['AlbumPage']
      ).items,
    ).toEqual([]);
    expect(
      (
        await context.request.get(
          `/api/v2/albums?favorite=false&favorite_collection_id=${child.id}`,
        )
      ).status(),
    ).toBe(400);
    const moved = await context.request.put(favoritePath, {
      headers: {
        ...ownHeaders,
        'If-Match': favorite.headers().etag!,
        'Idempotency-Key': randomUUID(),
      },
      data: { collection_id: null },
    });
    expect(moved.status()).toBe(200);
    expect((await albums({ favorite_collection_id: child.id })).items).toEqual([]);
    expect((await albums({ favorite_unfiled: 'true' })).items.map((item) => item.id)).toEqual([
      album,
    ]);
    const returned = await context.request.put(favoritePath, {
      headers: {
        ...ownHeaders,
        'If-Match': moved.headers().etag!,
        'Idempotency-Key': randomUUID(),
      },
      data: { collection_id: child.id },
    });
    expect(returned.status()).toBe(200);
    const favoriteReplay = await context.request.put(favoritePath, {
      headers: {
        ...ownHeaders,
        'If-Match': initial.headers().etag!,
        'Idempotency-Key': favoriteKey,
      },
      data: { collection_id: child.id },
    });
    expect(favoriteReplay.status()).toBe(200);
    expect(await favoriteReplay.json()).toMatchObject({ version: 2, collection_id: child.id });
    expect(await (await context.request.get(favoritePath)).json()).toMatchObject({
      version: 4,
      collection_id: child.id,
    });
    expect(
      (await albums({ favorite_collection_id: child.id })).items.map((item) => item.id),
    ).toEqual([album]);
    const renameKey = randomUUID();
    const renamed = await context.request.patch(`${path}/${root.id}`, {
      headers: {
        ...ownHeaders,
        'If-Match': rootResponse.headers().etag!,
        'Idempotency-Key': renameKey,
      },
      data: { name: 'Journeys' },
    });
    expect(renamed.status()).toBe(200);
    const renamedValue = (await renamed.json()) as Collection;
    expect(renamedValue).toMatchObject({ id: root.id, name: 'Journeys', version: 2 });
    const replay = await context.request.post(path, {
      headers: { ...ownHeaders, 'Idempotency-Key': createKey },
      data: { name: 'Trips', parent_id: null },
    });
    expect(replay.status()).toBe(201);
    expect(await replay.json()).toEqual(root);
    expect(await (await context.request.get(`${path}/${root.id}`)).json()).toEqual(renamedValue);
    expect(await (await context.request.get(favoritePath)).json()).toMatchObject({
      favorited: true,
      collection_id: child.id,
    });
    expect(await (await context.request.get(`/api/v2/albums/${album}`)).json()).toEqual(
      albumBefore,
    );
    expect((await context.request.get(`/api/v2/media/${prefix}-media/original`)).status()).toBe(
      404,
    );
    expect(JSON.stringify({ root, child, renamedValue })).not.toMatch(
      /user_id|album_id|root_path|relative_path|storage_path|source_sha256/,
    );
    const groupResponse = await api.get(`/api/v2/admin/groups/${group.id}`);
    expect(
      (
        await api.delete(`/api/v2/admin/groups/${group.id}`, {
          headers: { ...headers, 'If-Match': groupResponse.headers().etag! },
        })
      ).status(),
    ).toBe(204);
    expect((await context.request.get(favoritePath)).status()).toBe(404);
    expect((await albums({ favorite_collection_id: child.id })).items).toEqual([]);
    expect((await albums({ favorite_unfiled: 'true' })).items).toEqual([]);
    expect((await context.request.get(`${path}/${root.id}`)).status()).toBe(200);
    expect((await context.request.get(`${path}?${continued}`)).status()).toBe(400);
  } finally {
    const current = await api.get(`/api/v2/admin/groups/${group.id}`);
    if (current.status() !== 404) {
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
