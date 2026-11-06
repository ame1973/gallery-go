import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { components, operations } from '../../web/src/lib/api/schema';

test('HTTPS album selectors intersect current grants before bounded cards and scoped keysets', async ({
  account,
  administrator,
  page,
  context,
  request,
}, testInfo) => {
  const prefix = `album-selection-${testInfo.project.name}`;
  const albums = ['album', 'other-album', 'private-album'].map((suffix) => `${prefix}-${suffix}`);
  const { api, headers } = administrator;
  const created = await api.post('/api/v2/admin/groups', {
    headers: { ...headers, 'Idempotency-Key': randomUUID() },
    data: { name: `Album selection-${randomUUID()}` },
  });
  expect(created.status()).toBe(201);
  const group = (await created.json()) as components['schemas']['Group'];
  let tag: components['schemas']['Tag'] | undefined;
  type Selection = NonNullable<operations['listAlbums']['parameters']['query']>;
  const queryString = (selection: Selection) =>
    new URLSearchParams(
      Object.entries(selection).map(([key, value]) => [key, String(value)]),
    ).toString();
  const selected = async (selection: Selection = {}) => {
    const response = await context.request.get(`/api/v2/albums?${queryString(selection)}`);
    expect(response.status()).toBe(200);
    expect(response.headers()['cache-control']).toBe('private, no-store');
    return (await response.json()) as components['schemas']['AlbumPage'];
  };
  const empty = { items: [], next_cursor: null, has_more: false };
  try {
    expect((await request.get('/api/v2/albums?source_id=unknown')).status()).toBe(401);
    await signIn(page, account);
    expect(await selected({ person_id: `${prefix}-person` })).toEqual(empty);
    for (const path of [
      `/api/v2/admin/libraries/${prefix}-library/access`,
      `/api/v2/admin/users/${account.user.id}/groups`,
    ]) {
      const current = await api.get(path);
      expect(current.status()).toBe(200);
      expect(
        (
          await api.put(path, {
            headers: { ...headers, 'If-Match': current.headers().etag! },
            data: { group_ids: [group.id] },
          })
        ).status(),
      ).toBe(200);
    }
    const createdTag = await api.post('/api/v2/admin/tags', {
      headers: { ...headers, 'Idempotency-Key': randomUUID() },
      data: { name: `Selection assigned private-${randomUUID()}` },
    });
    expect(createdTag.status()).toBe(201);
    tag = (await createdTag.json()) as components['schemas']['Tag'];
    for (const id of albums) {
      const path = `/api/v2/admin/albums/${id}/tags`;
      const current = await api.get(path);
      expect(current.status()).toBe(200);
      expect(
        (
          await api.put(path, {
            headers: {
              ...headers,
              'If-Match': current.headers().etag!,
              'Idempotency-Key': randomUUID(),
            },
            data: { tag_ids: [tag.id] },
          })
        ).status(),
      ).toBe(200);
    }
    const filter = {
      library_id: `${prefix}-library`,
      source_id: `${prefix}-source`,
      person_id: `${prefix}-person`,
      tag_id: tag.id,
    } satisfies Selection;
    for (const selection of [
      { library_id: filter.library_id },
      { source_id: filter.source_id },
      { person_id: filter.person_id },
      { tag_id: tag.id },
      filter,
    ] satisfies Selection[]) {
      const result = await selected(selection);
      expect(result.items.map((album) => album.id)).toEqual(albums.slice(0, 2));
    }
    const first = await selected({ ...filter, limit: 1 });
    expect(first).toMatchObject({
      items: [{ id: albums[0], media_count: 2, cover_media_id: `${prefix}-media` }],
      has_more: true,
    });
    expect(first.next_cursor).not.toBeNull();
    const last = await selected({ ...filter, limit: 100, cursor: first.next_cursor! });
    expect(last).toMatchObject({
      items: [{ id: albums[1], media_count: 0, cover_media_id: null }],
      has_more: false,
      next_cursor: null,
    });
    for (const album of [...first.items, ...last.items]) {
      const detail = await context.request.get(`/api/v2/albums/${album.id}`);
      expect(detail.status()).toBe(200);
      expect(await detail.json()).toEqual(album);
    }
    for (const sort of ['oldest', 'latest'] as const) {
      const expected = sort === 'oldest' ? albums.slice(0, 2) : albums.slice(0, 2).reverse();
      const ordered = await selected({ ...filter, sort, limit: 1 });
      expect(ordered.items.map((album) => album.id)).toEqual(expected.slice(0, 1));
      expect(ordered.items[0]!.indexed_at).toBe('2026-11-07T00:00:00.123Z');
      expect(ordered.next_cursor).not.toBeNull();
      const final = await selected({ ...filter, sort, cursor: ordered.next_cursor!, limit: 100 });
      expect(final.items.map((album) => album.id)).toEqual(expected.slice(1));
      expect(final).toMatchObject({ has_more: false, next_cursor: null });
      expect(final.items[0]!.indexed_at).toBe(ordered.items[0]!.indexed_at);
      const position = JSON.parse(
        Buffer.from(ordered.next_cursor!, 'base64url').toString('utf8'),
      ) as { after: string; position: string };
      expect(position).toMatchObject({
        after: ordered.items[0]!.id,
        position: String(Date.parse(ordered.items[0]!.indexed_at)),
      });
      const changedOrder = await context.request.get(
        `/api/v2/albums?${queryString({ ...filter, sort: sort === 'oldest' ? 'latest' : 'oldest', cursor: ordered.next_cursor! })}`,
      );
      expect(changedOrder.status()).toBe(400);
      expect(await changedOrder.json()).toMatchObject({ code: 'INVALID_CURSOR' });
    }
    const admin = await api.get(`/api/v2/albums?${queryString(filter)}`);
    expect(admin.status()).toBe(200);
    expect(
      ((await admin.json()) as components['schemas']['AlbumPage']).items.map((album) => album.id),
    ).toEqual(albums);
    for (const field of ['library_id', 'source_id', 'person_id', 'tag_id'] as const) {
      expect(await selected({ ...filter, [field]: 'unknown' })).toEqual(empty);
      const changed = await context.request.get(
        `/api/v2/albums?${queryString({ ...filter, [field]: 'unknown', cursor: first.next_cursor! })}`,
      );
      expect(changed.status()).toBe(400);
      expect(await changed.json()).toMatchObject({ code: 'INVALID_CURSOR' });
    }
    const wrongAccount = await api.get(
      `/api/v2/albums?${queryString({ ...filter, cursor: first.next_cursor! })}`,
    );
    expect(wrongAccount.status()).toBe(400);
    expect(await wrongAccount.json()).toMatchObject({ code: 'INVALID_CURSOR' });
    for (const query of [
      'sort=popular',
      'q=not-search',
      'tag_id=',
      'source_id=one&source_id=two',
    ]) {
      const invalid = await context.request.get(`/api/v2/albums?${query}`);
      expect(invalid.status()).toBe(400);
      expect(await invalid.json()).toMatchObject({ code: 'INVALID_FILTER' });
    }
    expect((await context.request.get('/api/v2/admin/tags')).status()).toBe(403);
    expect((await context.request.get(`/api/v2/media/${prefix}-media/original`)).status()).toBe(
      404,
    );
    expect(JSON.stringify({ first, last })).not.toMatch(
      /root_path|relative_path|storage_path|source_sha256|Private\/Creator|Selection assigned private|\/tmp\//,
    );
    const current = await api.get(`/api/v2/admin/groups/${group.id}`);
    expect(
      (
        await api.delete(`/api/v2/admin/groups/${group.id}`, {
          headers: { ...headers, 'If-Match': current.headers().etag! },
        })
      ).status(),
    ).toBe(204);
    expect(await selected(filter)).toEqual(empty);
    const stale = await context.request.get(
      `/api/v2/albums?${queryString({ ...filter, cursor: first.next_cursor! })}`,
    );
    expect(stale.status()).toBe(400);
    expect(await stale.json()).toMatchObject({ code: 'INVALID_CURSOR' });
  } finally {
    for (const id of albums) {
      const path = `/api/v2/admin/albums/${id}/tags`;
      const current = await api.get(path);
      expect(current.status()).toBe(200);
      const value = (await current.json()) as components['schemas']['AlbumTagSet'];
      if (value.tag_ids.length) {
        expect(
          (
            await api.put(path, {
              headers: {
                ...headers,
                'If-Match': current.headers().etag!,
                'Idempotency-Key': randomUUID(),
              },
              data: { tag_ids: [] },
            })
          ).status(),
        ).toBe(200);
      }
    }
    if (tag) {
      const current = await api.get(`/api/v2/admin/tags/${tag.id}`);
      expect(
        (
          await api.delete(`/api/v2/admin/tags/${tag.id}`, {
            headers: {
              ...headers,
              'If-Match': current.headers().etag!,
              'Idempotency-Key': randomUUID(),
            },
          })
        ).status(),
      ).toBe(204);
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
