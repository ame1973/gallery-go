import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('HTTPS album display and cover preferences enforce versions, historical receipts and no original bypass', async ({
  account,
  administrator,
  page,
  context,
  request,
}, testInfo) => {
  const prefix = `album-metadata-${testInfo.project.name}`;
  const albumID = `${prefix}-album`;
  const path = `/api/v2/admin/albums/${albumID}`;
  const publicPath = `/api/v2/albums/${albumID}`;
  const { api, headers } = administrator;
  const created = await api.post('/api/v2/admin/groups', {
    headers: { ...headers, 'Idempotency-Key': randomUUID() },
    data: { name: `Album editors-${randomUUID()}` },
  });
  expect(created.status()).toBe(201);
  const group = (await created.json()) as components['schemas']['Group'];
  try {
    expect((await request.get(path)).status()).toBe(401);
    await signIn(page, account);
    expect((await context.request.get(path)).status()).toBe(403);
    expect((await context.request.patch(path, { data: { display_name: 'Denied' } })).status()).toBe(
      403,
    );
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
    const meBefore = (await (
      await context.request.get('/api/v2/auth/me')
    ).json()) as components['schemas']['User'];
    const initial = await api.get(path);
    expect(initial.status()).toBe(200);
    expect(initial.headers()['cache-control']).toBe('private, no-store');
    const before = (await initial.json()) as components['schemas']['AlbumMetadata'];
    expect(before).toMatchObject({ id: albumID, display_name: null, cover_override_id: null });
    const key = randomUUID();
    const commandHeaders = {
      ...headers,
      'If-Match': initial.headers().etag!,
      'Idempotency-Key': key,
    };
    const patch = { display_name: 'My display override', cover_media_id: `${prefix}-other-media` };
    const changedResponse = await api.patch(path, { headers: commandHeaders, data: patch });
    expect(changedResponse.status()).toBe(200);
    const changed = (await changedResponse.json()) as components['schemas']['AlbumMetadata'];
    expect(changed).toMatchObject({
      id: albumID,
      source_name: before.source_name,
      display_name: patch.display_name,
      cover_media_id: patch.cover_media_id,
      cover_override_id: patch.cover_media_id,
      version: before.version + 1,
    });
    const visible = await context.request.get(publicPath);
    expect(visible.status()).toBe(200);
    expect(await visible.json()).toMatchObject({
      title: patch.display_name,
      cover_media_id: patch.cover_media_id,
      version: changed.version,
    });
    const meAfter = (await (
      await context.request.get('/api/v2/auth/me')
    ).json()) as components['schemas']['User'];
    expect(meAfter.scope_id).toBe(meBefore.scope_id);
    expect(
      (await context.request.get(`/api/v2/media/${prefix}-other-media/original`)).status(),
    ).toBe(404);
    const invalidNull = await api.patch(path, {
      headers: {
        ...headers,
        'If-Match': changedResponse.headers().etag!,
        'Idempotency-Key': randomUUID(),
      },
      data: { display_name: null },
    });
    expect(invalidNull.status()).toBe(400);
    const clearResponse = await api.patch(path, {
      headers: {
        ...headers,
        'If-Match': changedResponse.headers().etag!,
        'Idempotency-Key': randomUUID(),
      },
      data: { display_name: '', cover_media_id: '' },
    });
    expect(clearResponse.status()).toBe(200);
    const cleared = (await clearResponse.json()) as components['schemas']['AlbumMetadata'];
    expect(cleared).toMatchObject({
      display_name: null,
      cover_override_id: null,
      cover_media_id: patch.cover_media_id,
      version: changed.version + 1,
    });
    const replay = await api.patch(path, { headers: commandHeaders, data: patch });
    expect(replay.status()).toBe(200);
    expect(await replay.json()).toEqual(changed);
    expect(replay.headers().etag).toBe(changedResponse.headers().etag);
    const latest = await api.get(path);
    expect(await latest.json()).toEqual(cleared);
    const publicCurrent = await context.request.get(publicPath);
    expect(await publicCurrent.json()).toMatchObject({
      title: before.source_name,
      cover_media_id: patch.cover_media_id,
    });
    const wrongKeyBody = await api.patch(path, {
      headers: commandHeaders,
      data: { display_name: 'Different' },
    });
    expect(wrongKeyBody.status()).toBe(409);
    expect(await wrongKeyBody.json()).toMatchObject({ code: 'IDEMPOTENCY_CONFLICT' });
    expect(JSON.stringify({ changed, cleared })).not.toMatch(
      /root_path|relative_path|storage_path|\.marker|\/private/,
    );
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
