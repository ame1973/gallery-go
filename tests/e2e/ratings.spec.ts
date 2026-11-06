import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('HTTPS personal ratings keep actor revisions and refresh aggregates through replay, revoke and account deletion', async ({
  account,
  administrator,
  page,
  context,
  request,
}, testInfo) => {
  const prefix = `ratings-${testInfo.project.name}`;
  const albumID = `${prefix}-album`;
  const path = `/api/v2/albums/${albumID}/rating`;
  const publicPath = `/api/v2/albums/${albumID}`;
  const { api, headers } = administrator;
  const created = await api.post('/api/v2/admin/groups', {
    headers: { ...headers, 'Idempotency-Key': randomUUID() },
    data: { name: `Rating voters-${randomUUID()}` },
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
    const ownInitial = (await initial.json()) as components['schemas']['AlbumRating'];
    expect(initial.status()).toBe(200);
    expect(initial.headers()['cache-control']).toBe('private, no-store');
    expect(ownInitial).toEqual({ album_id: albumID, rating: null, updated_at: null, version: 1 });
    const adminInitial = await api.get(path);
    expect(adminInitial.status()).toBe(200);
    expect(adminInitial.headers().etag).not.toBe(initial.headers().etag);
    const meBefore = (await (
      await context.request.get('/api/v2/auth/me')
    ).json()) as components['schemas']['User'];
    const publicBefore = (await (
      await context.request.get(publicPath)
    ).json()) as components['schemas']['Album'];
    const key = randomUUID();
    const firstHeaders = {
      ...ownHeaders,
      'If-Match': initial.headers().etag!,
      'Idempotency-Key': key,
    };
    expect(
      (
        await context.request.put(path, {
          headers: { ...firstHeaders, 'X-CSRF-Token': '' },
          data: { rating: 5 },
        })
      ).status(),
    ).toBe(403);
    expect(
      (
        await context.request.put(path, {
          headers: { ...firstHeaders, 'If-Match': adminInitial.headers().etag! },
          data: { rating: 5 },
        })
      ).status(),
    ).toBe(400);
    expect(
      (await context.request.put(path, { headers: firstHeaders, data: { rating: null } })).status(),
    ).toBe(400);
    const changedResponse = await context.request.put(path, {
      headers: firstHeaders,
      data: { rating: 5 },
    });
    expect(changedResponse.status()).toBe(200);
    const changed = (await changedResponse.json()) as components['schemas']['AlbumRating'];
    expect(changed).toMatchObject({ album_id: albumID, rating: 5, version: 2 });
    expect(changed.updated_at).not.toBeNull();
    expect(await (await context.request.get(publicPath)).json()).toMatchObject({
      rating_avg: 5,
      version: publicBefore.version + 1,
    });
    const adminChanged = await api.put(path, {
      headers: {
        ...headers,
        'If-Match': adminInitial.headers().etag!,
        'Idempotency-Key': randomUUID(),
      },
      data: { rating: 1 },
    });
    expect(adminChanged.status()).toBe(200);
    expect(await (await context.request.get(publicPath)).json()).toMatchObject({ rating_avg: 3 });
    const independent = await context.request.get(path);
    expect(await independent.json()).toEqual(changed);
    expect(independent.headers().etag).toBe(changedResponse.headers().etag);
    const noop = await context.request.put(path, {
      headers: {
        ...ownHeaders,
        'If-Match': independent.headers().etag!,
        'Idempotency-Key': randomUUID(),
      },
      data: { rating: 5 },
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
    const cleared = (await clearedResponse.json()) as components['schemas']['AlbumRating'];
    expect(cleared).toMatchObject({ album_id: albumID, rating: null, version: 3 });
    const replay = await context.request.put(path, { headers: firstHeaders, data: { rating: 5 } });
    expect(replay.status()).toBe(200);
    expect(await replay.json()).toEqual(changed);
    expect(await (await context.request.get(path)).json()).toEqual(cleared);
    expect(
      (
        await context.request.put(path, {
          headers: { ...firstHeaders, 'Idempotency-Key': randomUUID() },
          data: { rating: 5 },
        })
      ).status(),
    ).toBe(409);
    const recreatedResponse = await context.request.put(path, {
      headers: {
        ...ownHeaders,
        'If-Match': clearedResponse.headers().etag!,
        'Idempotency-Key': randomUUID(),
      },
      data: { rating: 3 },
    });
    expect(recreatedResponse.status()).toBe(200);
    const recreated = (await recreatedResponse.json()) as components['schemas']['AlbumRating'];
    expect(recreated).toMatchObject({ rating: 3, version: 4 });
    expect(await (await context.request.get(publicPath)).json()).toMatchObject({ rating_avg: 2 });
    expect((await context.request.get(`/api/v2/media/${prefix}-media/original`)).status()).toBe(
      404,
    );
    const meAfter = (await (
      await context.request.get('/api/v2/auth/me')
    ).json()) as components['schemas']['User'];
    expect(meAfter.scope_id).toBe(meBefore.scope_id);
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
    expect(
      (await context.request.put(path, { headers: firstHeaders, data: { rating: 5 } })).status(),
    ).toBe(404);
    // Account removal must refresh even a vote whose album is now hidden from it.
    const beforeDelete = (await (
      await api.get(publicPath)
    ).json()) as components['schemas']['Album'];
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
    expect(await (await api.get(publicPath)).json()).toMatchObject({
      rating_avg: 1,
      version: beforeDelete.version + 1,
    });
    expect(JSON.stringify({ changed, cleared, recreated })).not.toMatch(
      /user_id|root_path|relative_path|storage_path|\.marker|\/private/,
    );
  } finally {
    // Clear only this administrator's vote using the normal personal API.
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
