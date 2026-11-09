import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('HTTPS favorites keep private revisions through replay, clear, revoke and account removal without gallery grants', async ({
  account,
  administrator,
  page,
  context,
  request,
}, testInfo) => {
  const prefix = `favorites-${testInfo.project.name}`;
  const albumID = `${prefix}-album`;
  const path = `/api/v2/favorites/${albumID}`;
  const publicPath = `/api/v2/albums/${albumID}`;
  const { api, headers } = administrator;
  const created = await api.post('/api/v2/admin/groups', {
    headers: { ...headers, 'Idempotency-Key': randomUUID() },
    data: { name: `Favorite readers-${randomUUID()}` },
  });
  expect(created.status()).toBe(201);
  const group = (await created.json()) as components['schemas']['Group'];
  try {
    expect((await request.get(path)).status()).toBe(401);
    await signIn(page, account);
    expect((await context.request.get(path)).status()).toBe(404);
    const libraryPolicy = `/api/v2/admin/libraries/${prefix}-library/access`;
    let current = await api.get(libraryPolicy);
    expect(
      (
        await api.put(libraryPolicy, {
          headers: { ...headers, 'If-Match': current.headers().etag! },
          data: { group_ids: [group.id] },
        })
      ).status(),
    ).toBe(200);
    const membership = `/api/v2/admin/users/${account.user.id}/groups`;
    current = await api.get(membership);
    expect(
      (
        await api.put(membership, {
          headers: { ...headers, 'If-Match': current.headers().etag! },
          data: { group_ids: [group.id] },
        })
      ).status(),
    ).toBe(200);
    const csrfResponse = await context.request.get('/api/v2/auth/csrf');
    const { token } = (await csrfResponse.json()) as { token: string };
    const ownHeaders = { Origin: headers.Origin, 'X-CSRF-Token': token };
    const initial = await context.request.get(path);
    expect(initial.status()).toBe(200);
    expect(initial.headers()['cache-control']).toBe('private, no-store');
    expect((await initial.json()) as components['schemas']['AlbumFavorite']).toEqual({
      album_id: albumID,
      favorited: false,
      collection_id: null,
      updated_at: null,
      version: 1,
    });
    const adminInitial = await api.get(path);
    expect(adminInitial.status()).toBe(200);
    expect(adminInitial.headers().etag).not.toBe(initial.headers().etag);
    const meBefore = (await (
      await context.request.get('/api/v2/auth/me')
    ).json()) as components['schemas']['User'];
    const publicBefore = (await (
      await context.request.get(publicPath)
    ).json()) as components['schemas']['Album'];
    const firstHeaders = {
      ...ownHeaders,
      'If-Match': initial.headers().etag!,
      'Idempotency-Key': randomUUID(),
    };
    expect(
      (
        await context.request.put(path, {
          headers: { ...firstHeaders, 'X-CSRF-Token': '' },
          data: {},
        })
      ).status(),
    ).toBe(403);
    expect(
      (
        await context.request.put(path, {
          headers: { ...firstHeaders, 'If-Match': adminInitial.headers().etag! },
          data: {},
        })
      ).status(),
    ).toBe(400);
    const rating = await context.request.get(`/api/v2/albums/${albumID}/rating`);
    expect(
      (
        await context.request.put(path, {
          headers: { ...firstHeaders, 'If-Match': rating.headers().etag! },
          data: {},
        })
      ).status(),
    ).toBe(400);
    expect(
      (
        await context.request.put(path, { headers: firstHeaders, data: { favorited: true } })
      ).status(),
    ).toBe(400);
    const changedResponse = await context.request.put(path, { headers: firstHeaders, data: {} });
    expect(changedResponse.status()).toBe(200);
    const changed = (await changedResponse.json()) as components['schemas']['AlbumFavorite'];
    expect(changed).toMatchObject({
      album_id: albumID,
      favorited: true,
      collection_id: null,
      version: 2,
    });
    expect(changed.updated_at).not.toBeNull();
    const adminChanged = await api.put(path, {
      headers: {
        ...headers,
        'If-Match': adminInitial.headers().etag!,
        'Idempotency-Key': randomUUID(),
      },
      data: { collection_id: null },
    });
    expect(adminChanged.status()).toBe(200);
    const adminFavorite = (await adminChanged.json()) as components['schemas']['AlbumFavorite'];
    const independent = await context.request.get(path);
    expect(await independent.json()).toEqual(changed);
    expect(independent.headers().etag).toBe(changedResponse.headers().etag);
    const noop = await context.request.put(path, {
      headers: {
        ...ownHeaders,
        'If-Match': independent.headers().etag!,
        'Idempotency-Key': randomUUID(),
      },
      data: { collection_id: null },
    });
    expect(noop.status()).toBe(200);
    expect(await noop.json()).toEqual(changed);
    const clearedResponse = await context.request.delete(path, {
      headers: {
        ...ownHeaders,
        'If-Match': independent.headers().etag!,
        'Idempotency-Key': randomUUID(),
      },
    });
    expect(clearedResponse.status()).toBe(200);
    const cleared = (await clearedResponse.json()) as components['schemas']['AlbumFavorite'];
    expect(cleared).toMatchObject({
      album_id: albumID,
      favorited: false,
      collection_id: null,
      version: 3,
    });
    const replay = await context.request.put(path, {
      headers: firstHeaders,
      data: { collection_id: null },
    });
    expect(replay.status()).toBe(200);
    expect(await replay.json()).toEqual(changed);
    expect(await (await context.request.get(path)).json()).toEqual(cleared);
    expect(
      (
        await context.request.put(path, {
          headers: { ...firstHeaders, 'Idempotency-Key': randomUUID() },
          data: {},
        })
      ).status(),
    ).toBe(409);
    const recreatedResponse = await context.request.put(path, {
      headers: {
        ...ownHeaders,
        'If-Match': clearedResponse.headers().etag!,
        'Idempotency-Key': randomUUID(),
      },
      data: {},
    });
    expect(recreatedResponse.status()).toBe(200);
    const recreated = (await recreatedResponse.json()) as components['schemas']['AlbumFavorite'];
    expect(recreated).toMatchObject({ favorited: true, version: 4 });
    expect(await (await context.request.get(publicPath)).json()).toEqual(publicBefore);
    expect(await (await context.request.get('/api/v2/auth/me')).json()).toEqual(meBefore);
    expect((await context.request.get(`/api/v2/media/${prefix}-media/original`)).status()).toBe(
      404,
    );
    current = await api.get(membership);
    expect(
      (
        await api.put(membership, {
          headers: { ...headers, 'If-Match': current.headers().etag! },
          data: { group_ids: [] },
        })
      ).status(),
    ).toBe(200);
    expect((await context.request.get(path)).status()).toBe(404);
    expect((await context.request.put(path, { headers: firstHeaders, data: {} })).status()).toBe(
      404,
    );
    const userPath = `/api/v2/admin/users/${account.user.id}`;
    current = await api.get(userPath);
    expect(
      (
        await api.delete(userPath, {
          headers: { ...headers, 'If-Match': current.headers().etag! },
        })
      ).status(),
    ).toBe(204);
    expect((await context.request.get(path)).status()).toBe(401);
    expect(await (await api.get(path)).json()).toEqual(adminFavorite);
    expect(await (await api.get(publicPath)).json()).toEqual(publicBefore);
    expect(JSON.stringify({ changed, cleared, recreated })).not.toMatch(
      /user_id|root_path|relative_path|storage_path|\.marker|\/private/,
    );
  } finally {
    const own = await api.get(path);
    if (own.status() === 200) {
      expect(
        (
          await api.delete(path, {
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
