import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('HTTPS album tag replacement shares current grants, canonical receipts and independent label versions', async ({
  account,
  administrator,
  page,
  context,
  request,
}, testInfo) => {
  const prefix = `album-tags-${testInfo.project.name}`;
  const albumID = `${prefix}-album`;
  const path = `/api/v2/admin/albums/${albumID}/tags`;
  const publicPath = `/api/v2/albums/${albumID}/tags`;
  const { api, headers } = administrator;
  const createdGroup = await api.post('/api/v2/admin/groups', {
    headers: { ...headers, 'Idempotency-Key': randomUUID() },
    data: { name: `Album tag readers-${randomUUID()}` },
  });
  expect(createdGroup.status()).toBe(201);
  const group = (await createdGroup.json()) as components['schemas']['Group'];
  const tags: components['schemas']['Tag'][] = [];
  try {
    expect((await request.get(publicPath)).status()).toBe(401);
    expect((await request.get(path)).status()).toBe(401);
    await signIn(page, account);
    expect((await context.request.get(publicPath)).status()).toBe(404);
    expect((await context.request.get(path)).status()).toBe(403);
    expect((await context.request.put(path, { data: { tag_ids: [] } })).status()).toBe(403);
    const policy = `/api/v2/admin/libraries/${prefix}-library/access`;
    let current = await api.get(policy);
    expect(
      (
        await api.put(policy, {
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
    const meBefore = (await (
      await context.request.get('/api/v2/auth/me')
    ).json()) as components['schemas']['User'];
    for (const label of ['First', 'Second', 'Unused private']) {
      const created = await api.post('/api/v2/admin/tags', {
        headers: { ...headers, 'Idempotency-Key': randomUUID() },
        data: { name: `${label}-${randomUUID()}` },
      });
      expect(created.status()).toBe(201);
      tags.push((await created.json()) as components['schemas']['Tag']);
    }
    const [firstTag, secondTag] = tags as [
      components['schemas']['Tag'],
      components['schemas']['Tag'],
      components['schemas']['Tag'],
    ];
    const initial = await api.get(path);
    expect(initial.status()).toBe(200);
    const before = (await initial.json()) as components['schemas']['AlbumTagSet'];
    expect(before.tag_ids).toEqual([]);
    const ids = [firstTag.id, secondTag.id].sort();
    const commandHeaders = {
      ...headers,
      'If-Match': initial.headers().etag!,
      'Idempotency-Key': randomUUID(),
    };
    const body = { tag_ids: [secondTag.id, firstTag.id] };
    const changedResponse = await api.put(path, { headers: commandHeaders, data: body });
    expect(changedResponse.status()).toBe(200);
    const changed = (await changedResponse.json()) as components['schemas']['AlbumTagSet'];
    expect(changed).toEqual({ album_id: albumID, tag_ids: ids, version: before.version + 1 });
    const visibleResponse = await context.request.get(`${publicPath}?limit=1`);
    expect(visibleResponse.status()).toBe(200);
    expect(visibleResponse.headers()['cache-control']).toBe('private, no-store');
    expect(visibleResponse.headers().etag).toBeUndefined();
    const visible = (await visibleResponse.json()) as components['schemas']['TagPage'];
    expect(visible.items).toHaveLength(1);
    expect(visible.items[0]?.id).toBe(ids[0]);
    expect(visible.has_more).toBe(true);
    const last = await context.request.get(`${publicPath}?limit=1&cursor=${visible.next_cursor}`);
    expect(last.status()).toBe(200);
    expect(await last.json()).toMatchObject({
      items: [{ id: ids[1] }],
      has_more: false,
      next_cursor: null,
    });
    const tagCurrent = await api.get(`/api/v2/admin/tags/${firstTag.id}`);
    const renamed = await api.patch(`/api/v2/admin/tags/${firstTag.id}`, {
      headers: {
        ...headers,
        'If-Match': tagCurrent.headers().etag!,
        'Idempotency-Key': randomUUID(),
      },
      data: { name: `Renamed visible-${randomUUID()}` },
    });
    expect(renamed.status()).toBe(200);
    const renamedTag = (await renamed.json()) as components['schemas']['Tag'];
    const stable = await api.get(path);
    expect(await stable.json()).toEqual(changed);
    expect(stable.headers().etag).toBe(changedResponse.headers().etag);
    const labelsResponse = await context.request.get(publicPath);
    expect(labelsResponse.headers().etag).toBeUndefined();
    const labels = (await labelsResponse.json()) as components['schemas']['TagPage'];
    expect(labels.items.map((tag) => tag.id)).toEqual(ids);
    expect(labels.items.find((tag) => tag.id === firstTag.id)).toEqual(renamedTag);
    expect(labels.items.some((tag) => tag.id === tags[2]?.id)).toBe(false);
    const noop = await api.put(path, {
      headers: { ...headers, 'If-Match': stable.headers().etag!, 'Idempotency-Key': randomUUID() },
      data: { tag_ids: ids },
    });
    expect(noop.status()).toBe(200);
    expect(await noop.json()).toEqual(changed);
    expect(
      (
        await api.put(path, {
          headers: { ...commandHeaders, 'Idempotency-Key': randomUUID() },
          data: { tag_ids: [] },
        })
      ).status(),
    ).toBe(409);
    const meAfterMetadata = (await (
      await context.request.get('/api/v2/auth/me')
    ).json()) as components['schemas']['User'];
    expect(meAfterMetadata.scope_id).toBe(meBefore.scope_id);
    expect((await context.request.get(`/api/v2/media/${prefix}-media/original`)).status()).toBe(
      404,
    );
    const albumPolicy = `/api/v2/admin/albums/${albumID}/access`;
    current = await api.get(albumPolicy);
    const restricted = await api.put(albumPolicy, {
      headers: { ...headers, 'If-Match': current.headers().etag! },
      data: { restricted: true, group_ids: [] },
    });
    expect(restricted.status()).toBe(200);
    expect((await context.request.get(publicPath)).status()).toBe(404);
    expect(
      ((await (await api.get(path)).json()) as components['schemas']['AlbumTagSet']).tag_ids,
    ).toEqual(ids);
    const restored = await api.put(albumPolicy, {
      headers: { ...headers, 'If-Match': restricted.headers().etag! },
      data: { restricted: false, group_ids: [] },
    });
    expect(restored.status()).toBe(200);
    expect((await context.request.get(publicPath)).status()).toBe(200);
    current = await api.get(path);
    const currentSet = (await current.json()) as components['schemas']['AlbumTagSet'];
    const clearResponse = await api.put(path, {
      headers: { ...headers, 'If-Match': current.headers().etag!, 'Idempotency-Key': randomUUID() },
      data: { tag_ids: [] },
    });
    expect(clearResponse.status()).toBe(200);
    const cleared = (await clearResponse.json()) as components['schemas']['AlbumTagSet'];
    expect(cleared).toEqual({ album_id: albumID, tag_ids: [], version: currentSet.version + 1 });
    expect(await (await context.request.get(publicPath)).json()).toEqual({
      items: [],
      next_cursor: null,
      has_more: false,
    });
    const replay = await api.put(path, { headers: commandHeaders, data: body });
    expect(replay.status()).toBe(200);
    expect(await replay.json()).toEqual(changed);
    expect(replay.headers().etag).toBe(changedResponse.headers().etag);
    expect(await (await api.get(path)).json()).toEqual(cleared);
    expect((await api.get(`/api/v2/admin/tags/${firstTag.id}`)).status()).toBe(200);
  } finally {
    const current = await api.get(path);
    if (current.status() === 200) {
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
    for (const tag of tags) {
      const current = await api.get(`/api/v2/admin/tags/${tag.id}`);
      if (current.status() !== 404) {
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
    }
    const currentGroup = await api.get(`/api/v2/admin/groups/${group.id}`);
    expect(
      (
        await api.delete(`/api/v2/admin/groups/${group.id}`, {
          headers: { ...headers, 'If-Match': currentGroup.headers().etag! },
        })
      ).status(),
    ).toBe(204);
  }
});
