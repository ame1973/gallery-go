import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('HTTPS people names, visible counts, covers and albums follow current grants and restriction latches', async ({
  account,
  administrator,
  page,
  context,
  request,
}, testInfo) => {
  const prefix = `people-${testInfo.project.name}`;
  const personPath = `/api/v2/people/${prefix}-person`;
  const libraryPolicy = `/api/v2/admin/libraries/${prefix}-library/access`;
  const albumPolicy = `/api/v2/admin/albums/${prefix}-album/access`;
  const membership = `/api/v2/admin/users/${account.user.id}/groups`;
  const { api, headers } = administrator;
  const groups: string[] = [];
  try {
    for (const label of ['Creator library', 'Creator restricted']) {
      const created = await api.post('/api/v2/admin/groups', {
        headers: { ...headers, 'Idempotency-Key': randomUUID() },
        data: { name: `${label}-${randomUUID()}` },
      });
      expect(created.status()).toBe(201);
      groups.push(((await created.json()) as components['schemas']['Group']).id);
    }
    const [base, restricted] = groups as [string, string];
    for (const path of ['/api/v2/people', personPath, `${personPath}/albums`]) {
      expect((await request.get(path)).status()).toBe(401);
    }
    await signIn(page, account);
    const hidden = await context.request.get('/api/v2/people');
    expect(hidden.status()).toBe(200);
    expect(await hidden.json()).toEqual({ items: [], next_cursor: null, has_more: false });
    expect((await context.request.get(personPath)).status()).toBe(404);
    expect((await context.request.get(`${personPath}/albums`)).status()).toBe(404);
    let current = await api.get(libraryPolicy);
    expect(
      (
        await api.put(libraryPolicy, {
          headers: { ...headers, 'If-Match': current.headers().etag! },
          data: { group_ids: [base] },
        })
      ).status(),
    ).toBe(200);
    current = await api.get(membership);
    expect(
      (
        await api.put(membership, {
          headers: { ...headers, 'If-Match': current.headers().etag! },
          data: { group_ids: [base] },
        })
      ).status(),
    ).toBe(200);
    const listed = await context.request.get('/api/v2/people?limit=1');
    expect(listed.status()).toBe(200);
    expect(listed.headers()['cache-control']).toBe('private, no-store');
    const people = (await listed.json()) as components['schemas']['PersonPage'];
    expect(people).toMatchObject({
      items: [
        {
          id: `${prefix}-person`,
          name: 'Visible creator',
          source_id: `${prefix}-source`,
          album_count: 1,
          cover_media_id: `${prefix}-media`,
          version: 1,
        },
      ],
      next_cursor: null,
      has_more: false,
    });
    const detail = await context.request.get(personPath);
    expect(detail.status()).toBe(200);
    expect(await detail.json()).toEqual(people.items[0]);
    const albums = await context.request.get(`${personPath}/albums?limit=1`);
    expect(albums.status()).toBe(200);
    const albumPage = (await albums.json()) as components['schemas']['AlbumPage'];
    expect(albumPage).toMatchObject({
      items: [{ id: `${prefix}-album`, media_count: 2 }],
      next_cursor: null,
      has_more: false,
    });
    expect(JSON.stringify({ people, albumPage })).not.toMatch(
      /relative_path|root_path|Private\/Creator|\.marker|\/private/,
    );
    expect((await context.request.get(`/api/v2/media/${prefix}-media/original`)).status()).toBe(
      404,
    );
    current = await api.get(albumPolicy);
    expect(
      (
        await api.put(albumPolicy, {
          headers: { ...headers, 'If-Match': current.headers().etag! },
          data: { restricted: true, group_ids: [restricted] },
        })
      ).status(),
    ).toBe(200);
    expect((await context.request.get(personPath)).status()).toBe(404);
    expect((await context.request.get(`${personPath}/albums`)).status()).toBe(404);
    current = await api.get(membership);
    expect(
      (
        await api.put(membership, {
          headers: { ...headers, 'If-Match': current.headers().etag! },
          data: { group_ids: [base, restricted] },
        })
      ).status(),
    ).toBe(200);
    expect((await context.request.get(personPath)).status()).toBe(200);
    const group = await api.get(`/api/v2/admin/groups/${restricted}`);
    expect(
      (
        await api.delete(`/api/v2/admin/groups/${restricted}`, {
          headers: { ...headers, 'If-Match': group.headers().etag! },
        })
      ).status(),
    ).toBe(204);
    const policy = (await (
      await api.get(albumPolicy)
    ).json()) as components['schemas']['AlbumPolicy'];
    expect(policy).toMatchObject({ restricted: true, group_ids: [] });
    expect((await context.request.get(personPath)).status()).toBe(404);
    const deniedAgain = await context.request.get('/api/v2/people');
    expect(await deniedAgain.json()).toEqual({ items: [], next_cursor: null, has_more: false });
  } finally {
    for (const id of groups) {
      const current = await api.get(`/api/v2/admin/groups/${id}`);
      if (current.status() !== 404) {
        expect(current.status()).toBe(200);
        expect(
          (
            await api.delete(`/api/v2/admin/groups/${id}`, {
              headers: { ...headers, 'If-Match': current.headers().etag! },
            })
          ).status(),
        ).toBe(204);
      }
    }
  }
});
