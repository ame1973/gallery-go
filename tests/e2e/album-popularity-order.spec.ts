import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('HTTPS popularity keysets use recorded view totals and fresh account scopes', async ({
  account,
  administrator,
  page,
  context,
  request,
}, testInfo) => {
  const prefix = `popular-order-${testInfo.project.name}`;
  const primary = `${prefix}-album`;
  const other = `${prefix}-other-album`;
  const hidden = `${prefix}-private-album`;
  const unviewed = `${prefix}-unrated-album`;
  const { api, headers } = administrator;
  const created = await api.post('/api/v2/admin/groups', {
    headers: { ...headers, 'Idempotency-Key': randomUUID() },
    data: { name: `Popular readers-${randomUUID()}` },
  });
  expect(created.status()).toBe(201);
  const group = (await created.json()) as components['schemas']['Group'];
  try {
    expect((await request.get('/api/v2/albums?sort=popular')).status()).toBe(401);
    await signIn(page, account);
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
    const view = async (id: string, asReader = false, key = randomUUID()) => {
      const client = asReader ? context.request : api;
      const mutationHeaders = asReader ? ownHeaders : headers;
      const response = await client.post(`/api/v2/albums/${id}/views`, {
        headers: { ...mutationHeaders, 'Idempotency-Key': key },
      });
      expect(response.status()).toBe(200);
      return (await response.json()) as components['schemas']['AlbumView'];
    };
    await view(primary);
    await view(primary);
    await view(primary, true);
    await view(other);
    await view(other);
    await view(other, true);
    for (let i = 0; i < 4; i++) await view(hidden);
    const query = new URLSearchParams({
      source_id: `${prefix}-source`,
      sort: 'popular',
      limit: '1',
    });
    const response = await context.request.get(`/api/v2/albums?${query.toString()}`);
    expect(response.status()).toBe(200);
    expect(response.headers()['cache-control']).toBe('private, no-store');
    const first = (await response.json()) as components['schemas']['AlbumPage'];
    expect(first.items).toHaveLength(1);
    expect(first.items[0]).toMatchObject({ id: other, view_count: 3, media_count: 0 });
    expect(first.has_more).toBe(true);
    expect(first.next_cursor).not.toBeNull();
    const cursor = JSON.parse(Buffer.from(first.next_cursor!, 'base64url').toString('utf8')) as {
      after: string;
      position: string;
    };
    expect(cursor).toMatchObject({ after: other, position: 'v:3' });
    const key = randomUUID();
    const recorded = await view(other, false, key);
    expect(await view(other, false, key)).toEqual(recorded);
    query.set('cursor', first.next_cursor!);
    const continued = (await (
      await context.request.get(`/api/v2/albums?${query.toString()}`)
    ).json()) as components['schemas']['AlbumPage'];
    expect(continued.items).toHaveLength(1);
    expect(continued.items[0]).toMatchObject({ id: primary, view_count: 3 });
    const detail = (await (
      await context.request.get(`/api/v2/albums/${primary}`)
    ).json()) as components['schemas']['Album'];
    expect(continued.items[0]).toEqual(detail);
    expect((await api.get(`/api/v2/albums?${query.toString()}`)).status()).toBe(400);
    expect((await context.request.get(`/api/v2/media/${prefix}-media/original`)).status()).toBe(
      404,
    );
    query.delete('cursor');
    query.set('limit', '100');
    const fresh = (await (
      await context.request.get(`/api/v2/albums?${query.toString()}`)
    ).json()) as components['schemas']['AlbumPage'];
    expect(fresh.items.map((item) => item.id)).toEqual([other, primary, unviewed]);
    expect(fresh.items.map((item) => item.view_count)).toEqual([4, 3, 0]);
    current = await api.get(membership);
    expect(
      (
        await api.put(membership, {
          headers: { ...headers, 'If-Match': current.headers().etag! },
          data: { group_ids: [] },
        })
      ).status(),
    ).toBe(200);
    query.set('cursor', first.next_cursor!);
    expect((await context.request.get(`/api/v2/albums?${query.toString()}`)).status()).toBe(400);
    query.delete('cursor');
    expect(await (await context.request.get(`/api/v2/albums?${query.toString()}`)).json()).toEqual({
      items: [],
      next_cursor: null,
      has_more: false,
    });
    const userPath = `/api/v2/admin/users/${account.user.id}`;
    current = await api.get(userPath);
    expect(
      (
        await api.delete(userPath, {
          headers: { ...headers, 'If-Match': current.headers().etag! },
        })
      ).status(),
    ).toBe(204);
    const afterDelete = (await (
      await api.get(`/api/v2/albums?${query.toString()}`)
    ).json()) as components['schemas']['AlbumPage'];
    expect(afterDelete.items.map((item) => item.id)).toEqual([hidden, other, primary, unviewed]);
    expect(afterDelete.items.map((item) => item.view_count)).toEqual([4, 3, 2, 0]);
    expect((await context.request.get('/api/v2/albums?sort=popular')).status()).toBe(401);
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
