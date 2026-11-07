import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('HTTPS high-rating keysets use real means, frozen rerated anchors and fresh account scopes', async ({
  account,
  administrator,
  page,
  context,
  request,
}, testInfo) => {
  const prefix = `rating-order-${testInfo.project.name}`;
  const primary = `${prefix}-album`;
  const other = `${prefix}-other-album`;
  const hidden = `${prefix}-private-album`;
  const unrated = `${prefix}-unrated-album`;
  const { api, headers } = administrator;
  const created = await api.post('/api/v2/admin/groups', {
    headers: { ...headers, 'Idempotency-Key': randomUUID() },
    data: { name: `Rank readers-${randomUUID()}` },
  });
  expect(created.status()).toBe(201);
  const group = (await created.json()) as components['schemas']['Group'];
  try {
    expect((await request.get('/api/v2/albums?sort=top_rated')).status()).toBe(401);
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
    const vote = async (id: string, value: number, asReader = false) => {
      const client = asReader ? context.request : api;
      const mutationHeaders = asReader ? ownHeaders : headers;
      const path = `/api/v2/albums/${id}/rating`;
      const before = await client.get(path);
      expect(before.status()).toBe(200);
      const response = await client.put(path, {
        headers: {
          ...mutationHeaders,
          'If-Match': before.headers().etag!,
          'Idempotency-Key': randomUUID(),
        },
        data: { rating: value } satisfies components['schemas']['AlbumRatingRequest'],
      });
      expect(response.status()).toBe(200);
    };
    await vote(primary, 2);
    await vote(primary, 5, true);
    await vote(other, 5);
    await vote(other, 4, true);
    await vote(hidden, 5);
    const query = new URLSearchParams({
      source_id: `${prefix}-source`,
      sort: 'top_rated',
      limit: '1',
    });
    const response = await context.request.get(`/api/v2/albums?${query.toString()}`);
    expect(response.status()).toBe(200);
    expect(response.headers()['cache-control']).toBe('private, no-store');
    const first = (await response.json()) as components['schemas']['AlbumPage'];
    expect(first.items).toHaveLength(1);
    expect(first.items[0]).toMatchObject({ id: other, rating_avg: 4.5, media_count: 0 });
    expect(first.has_more).toBe(true);
    expect(first.next_cursor).not.toBeNull();
    const cursor = JSON.parse(Buffer.from(first.next_cursor!, 'base64url').toString('utf8')) as {
      after: string;
      position: string;
    };
    expect(cursor).toMatchObject({ after: other, position: 'r:4.5' });
    await vote(other, 1, true);
    query.set('cursor', first.next_cursor!);
    const continued = (await (
      await context.request.get(`/api/v2/albums?${query.toString()}`)
    ).json()) as components['schemas']['AlbumPage'];
    expect(continued.items).toHaveLength(1);
    expect(continued.items[0]).toMatchObject({ id: primary, rating_avg: 3.5 });
    const detail = (await (
      await context.request.get(`/api/v2/albums/${primary}`)
    ).json()) as components['schemas']['Album'];
    expect(continued.items[0]).toEqual(detail);
    expect((await context.request.get(`/api/v2/media/${prefix}-media/original`)).status()).toBe(
      404,
    );
    query.delete('cursor');
    query.set('limit', '100');
    const fresh = (await (
      await context.request.get(`/api/v2/albums?${query.toString()}`)
    ).json()) as components['schemas']['AlbumPage'];
    expect(fresh.items.map((item) => item.id)).toEqual([primary, other, unrated]);
    expect(fresh.items.map((item) => item.rating_avg)).toEqual([3.5, 3, null]);
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
    // The administrator sees their own higher hidden card; no fake public rank
    // or vote-count weighting substitutes for the actual remaining means.
    expect(afterDelete.items.map((item) => item.id)).toEqual([hidden, other, primary, unrated]);
    expect(afterDelete.items.map((item) => item.rating_avg)).toEqual([5, 5, 2, null]);
    expect((await context.request.get('/api/v2/albums?sort=top_rated')).status()).toBe(401);
  } finally {
    for (const id of [primary, other, hidden]) {
      const path = `/api/v2/albums/${id}/rating`;
      const current = await api.get(path);
      if (current.status() === 200) {
        expect(
          (
            await api.delete(path, {
              headers: {
                ...headers,
                'If-Match': current.headers().etag!,
                'Idempotency-Key': randomUUID(),
              },
            })
          ).status(),
        ).toBe(200);
      }
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
