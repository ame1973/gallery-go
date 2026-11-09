import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('HTTPS album views keep personal and shared counts truthful through replay and account deletion', async ({
  account,
  administrator,
  page,
  context,
  request,
}, testInfo) => {
  const prefix = `views-${testInfo.project.name}`;
  const albumID = `${prefix}-album`;
  const path = `/api/v2/albums/${albumID}/views`;
  const albumPath = `/api/v2/albums/${albumID}`;
  const { api, headers } = administrator;
  const created = await api.post('/api/v2/admin/groups', {
    headers: { ...headers, 'Idempotency-Key': randomUUID() },
    data: { name: `View readers-${randomUUID()}` },
  });
  expect(created.status()).toBe(201);
  const group = (await created.json()) as components['schemas']['Group'];
  try {
    expect((await request.get(path)).status()).toBe(401);
    await signIn(page, account);
    expect((await context.request.get(path)).status()).toBe(404);
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
    const initial = await context.request.get(path);
    expect(initial.status()).toBe(200);
    expect(initial.headers()['cache-control']).toBe('private, no-store');
    expect((await initial.json()) as components['schemas']['AlbumView']).toEqual({
      album_id: albumID,
      view_count: 0,
      last_viewed_at: null,
      version: 1,
    });
    const initialAdmin = await api.get(path);
    expect(initialAdmin.status()).toBe(200);
    expect(initialAdmin.headers().etag).not.toBe(initial.headers().etag);
    const key = randomUUID();
    const firstHeaders = { ...ownHeaders, 'Idempotency-Key': key };
    expect(
      (
        await context.request.post(path, { headers: { ...firstHeaders, 'X-CSRF-Token': '' } })
      ).status(),
    ).toBe(403);
    expect((await context.request.post(path, { headers: firstHeaders, data: {} })).status()).toBe(
      400,
    );
    const first = await context.request.post(path, { headers: firstHeaders });
    expect(first.status()).toBe(200);
    const firstValue = (await first.json()) as components['schemas']['AlbumView'];
    expect(firstValue).toMatchObject({ album_id: albumID, view_count: 1, version: 2 });
    expect(firstValue.last_viewed_at).not.toBeNull();
    const adminView = await api.post(path, { headers: { ...headers, 'Idempotency-Key': key } });
    expect(adminView.status()).toBe(200);
    expect(await adminView.json()).toMatchObject({ view_count: 1, version: 2 });
    const replay = await context.request.post(path, { headers: firstHeaders });
    expect(replay.status()).toBe(200);
    expect(await replay.json()).toEqual(firstValue);
    expect(
      (
        await context.request.post(path, {
          headers: { ...ownHeaders, 'Idempotency-Key': randomUUID() },
        })
      ).status(),
    ).toBe(200);
    expect(await (await context.request.get(path)).json()).toMatchObject({
      view_count: 2,
      version: 3,
    });
    expect(await (await context.request.get(albumPath)).json()).toMatchObject({
      view_count: 3,
      version: 4,
    });
    expect((await context.request.get(`/api/v2/media/${prefix}-media/original`)).status()).toBe(
      404,
    );
    const groupResponse = await api.get(`/api/v2/admin/groups/${group.id}`);
    expect(
      (
        await api.delete(`/api/v2/admin/groups/${group.id}`, {
          headers: { ...headers, 'If-Match': groupResponse.headers().etag! },
        })
      ).status(),
    ).toBe(204);
    expect((await context.request.get(path)).status()).toBe(404);
    expect((await context.request.post(path, { headers: firstHeaders })).status()).toBe(404);
    const userPath = `/api/v2/admin/users/${account.user.id}`;
    const user = await api.get(userPath);
    expect(
      (
        await api.delete(userPath, { headers: { ...headers, 'If-Match': user.headers().etag! } })
      ).status(),
    ).toBe(204);
    expect(await (await api.get(albumPath)).json()).toMatchObject({ view_count: 1, version: 5 });
    expect(await (await api.get(path)).json()).toMatchObject({ view_count: 1, version: 2 });
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
