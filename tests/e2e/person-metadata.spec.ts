import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('HTTPS Person label overrides preserve source identity, scopes and historical receipts without content grants', async ({
  account,
  administrator,
  page,
  context,
  request,
}, testInfo) => {
  const prefix = `person-metadata-${testInfo.project.name}`;
  const personID = `${prefix}-person`;
  const path = `/api/v2/admin/people/${personID}`;
  const publicPath = `/api/v2/people/${personID}`;
  const { api, headers } = administrator;
  const created = await api.post('/api/v2/admin/groups', {
    headers: { ...headers, 'Idempotency-Key': randomUUID() },
    data: { name: `Creator readers-${randomUUID()}` },
  });
  expect(created.status()).toBe(201);
  const group = (await created.json()) as components['schemas']['Group'];
  try {
    expect((await request.get(path)).status()).toBe(401);
    await signIn(page, account);
    expect((await context.request.get(path)).status()).toBe(403);
    expect((await context.request.get(publicPath)).status()).toBe(404);
    expect((await context.request.patch(path, { data: { display_name: 'Denied' } })).status()).toBe(
      403,
    );
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
    const initial = await api.get(path);
    expect(initial.status()).toBe(200);
    expect(initial.headers()['cache-control']).toBe('private, no-store');
    const before = (await initial.json()) as components['schemas']['PersonMetadata'];
    expect(before).toMatchObject({
      id: personID,
      source_id: `${prefix}-source`,
      source_name: 'Creator',
      display_name: 'Visible creator',
    });
    const patch = { display_name: 'My creator override 字' };
    const commandHeaders = {
      ...headers,
      'If-Match': initial.headers().etag!,
      'Idempotency-Key': randomUUID(),
    };
    const changedResponse = await api.patch(path, { headers: commandHeaders, data: patch });
    expect(changedResponse.status()).toBe(200);
    const changed = (await changedResponse.json()) as components['schemas']['PersonMetadata'];
    expect(changed).toEqual({
      ...before,
      display_name: patch.display_name,
      version: before.version + 1,
    });
    const visibleResponse = await context.request.get(publicPath);
    expect(visibleResponse.status()).toBe(200);
    const visible = (await visibleResponse.json()) as components['schemas']['Person'];
    expect(visible).toMatchObject({
      id: personID,
      name: patch.display_name,
      source_id: before.source_id,
      album_count: 1,
      version: changed.version,
    });
    const meAfter = (await (
      await context.request.get('/api/v2/auth/me')
    ).json()) as components['schemas']['User'];
    expect(meAfter.scope_id).toBe(meBefore.scope_id);
    expect((await context.request.get(`/api/v2/media/${prefix}-media/original`)).status()).toBe(
      404,
    );
    const noop = await api.patch(path, {
      headers: {
        ...headers,
        'If-Match': changedResponse.headers().etag!,
        'Idempotency-Key': randomUUID(),
      },
      data: patch,
    });
    expect(noop.status()).toBe(200);
    expect(await noop.json()).toEqual(changed);
    for (const data of [
      { display_name: null },
      { source_name: 'Rename source' },
      { source_id: 'other' },
    ]) {
      expect(
        (
          await api.patch(path, {
            headers: {
              ...headers,
              'If-Match': changedResponse.headers().etag!,
              'Idempotency-Key': randomUUID(),
            },
            data,
          })
        ).status(),
      ).toBe(400);
    }
    const clearResponse = await api.patch(path, {
      headers: {
        ...headers,
        'If-Match': changedResponse.headers().etag!,
        'Idempotency-Key': randomUUID(),
      },
      data: { display_name: '' },
    });
    expect(clearResponse.status()).toBe(200);
    const cleared = (await clearResponse.json()) as components['schemas']['PersonMetadata'];
    expect(cleared).toEqual({ ...before, display_name: null, version: changed.version + 1 });
    expect(await (await context.request.get(publicPath)).json()).toMatchObject({
      name: before.source_name,
      source_id: before.source_id,
      album_count: visible.album_count,
      cover_media_id: visible.cover_media_id,
      version: cleared.version,
    });
    const replay = await api.patch(path, { headers: commandHeaders, data: patch });
    expect(replay.status()).toBe(200);
    expect(replay.headers().etag).toBe(changedResponse.headers().etag);
    expect(await replay.json()).toEqual(changed);
    expect(await (await api.get(path)).json()).toEqual(cleared);
    const conflict = await api.patch(path, {
      headers: commandHeaders,
      data: { display_name: 'Different' },
    });
    expect(conflict.status()).toBe(409);
    expect(await conflict.json()).toMatchObject({ code: 'IDEMPOTENCY_CONFLICT' });
    expect(JSON.stringify({ before, changed, cleared })).not.toMatch(
      /root_path|relative_path|storage_path|\.marker|Private\/Creator|\/private/,
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
