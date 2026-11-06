import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('HTTPS creator classification preserves mapped provenance, current scopes and historical choices without grant changes', async ({
  account,
  administrator,
  page,
  context,
  request,
}, testInfo) => {
  const prefix = `creators-${testInfo.project.name}`;
  const albumID = `${prefix}-album`;
  const path = `/api/v2/admin/albums/${albumID}/creator`;
  const mappedPath = `/api/v2/people/${prefix}-mapped`;
  const manualPath = `/api/v2/people/${prefix}-manual`;
  const { api, headers } = administrator;
  const created = await api.post('/api/v2/admin/groups', {
    headers: { ...headers, 'Idempotency-Key': randomUUID() },
    data: { name: `Creator classification readers-${randomUUID()}` },
  });
  expect(created.status()).toBe(201);
  const group = (await created.json()) as components['schemas']['Group'];
  try {
    expect((await request.get(path)).status()).toBe(401);
    await signIn(page, account);
    expect((await context.request.get(path)).status()).toBe(403);
    expect((await context.request.put(path, { data: { mode: 'none' } })).status()).toBe(403);
    expect((await context.request.get(mappedPath)).status()).toBe(404);
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
    expect((await context.request.get(mappedPath)).status()).toBe(200);
    expect((await context.request.get(manualPath)).status()).toBe(404);
    const meBefore = (await (
      await context.request.get('/api/v2/auth/me')
    ).json()) as components['schemas']['User'];
    const initial = await api.get(path);
    expect(initial.status()).toBe(200);
    expect(initial.headers()['cache-control']).toBe('private, no-store');
    const before = (await initial.json()) as components['schemas']['AlbumCreator'];
    expect(before).toMatchObject({
      album_id: albumID,
      mode: 'mapped',
      person_id: `${prefix}-mapped`,
      mapped_person_id: `${prefix}-mapped`,
      override_person_id: null,
    });
    const commandHeaders = {
      ...headers,
      'If-Match': initial.headers().etag!,
      'Idempotency-Key': randomUUID(),
    };
    const choice = { mode: 'person', person_id: `${prefix}-manual` };
    const changedResponse = await api.put(path, { headers: commandHeaders, data: choice });
    expect(changedResponse.status()).toBe(200);
    const changed = (await changedResponse.json()) as components['schemas']['AlbumCreator'];
    expect(changed).toEqual({
      ...before,
      mode: 'person',
      person_id: `${prefix}-manual`,
      override_person_id: `${prefix}-manual`,
      version: before.version + 1,
    });
    const meAfter = (await (
      await context.request.get('/api/v2/auth/me')
    ).json()) as components['schemas']['User'];
    expect(meAfter.scope_id).not.toBe(meBefore.scope_id);
    expect((await context.request.get(mappedPath)).status()).toBe(404);
    const manual = await context.request.get(manualPath);
    expect(manual.status()).toBe(200);
    expect(await manual.json()).toMatchObject({ name: 'manual creator', album_count: 1 });
    const noop = await api.put(path, {
      headers: {
        ...headers,
        'If-Match': changedResponse.headers().etag!,
        'Idempotency-Key': randomUUID(),
      },
      data: choice,
    });
    expect(noop.status()).toBe(200);
    expect(await noop.json()).toEqual(changed);
    const meNoop = (await (
      await context.request.get('/api/v2/auth/me')
    ).json()) as components['schemas']['User'];
    expect(meNoop.scope_id).toBe(meAfter.scope_id);
    const foreign = await api.put(path, {
      headers: {
        ...headers,
        'If-Match': changedResponse.headers().etag!,
        'Idempotency-Key': randomUUID(),
      },
      data: { mode: 'person', person_id: `people-${testInfo.project.name}-person` },
    });
    expect(foreign.status()).toBe(422);
    const ambiguous = await api.put(path, {
      headers: {
        ...headers,
        'If-Match': changedResponse.headers().etag!,
        'Idempotency-Key': randomUUID(),
      },
      data: { mode: 'person', person_id: null },
    });
    expect(ambiguous.status()).toBe(400);
    const noneResponse = await api.put(path, {
      headers: {
        ...headers,
        'If-Match': changedResponse.headers().etag!,
        'Idempotency-Key': randomUUID(),
      },
      data: { mode: 'none' },
    });
    expect(noneResponse.status()).toBe(200);
    const none = (await noneResponse.json()) as components['schemas']['AlbumCreator'];
    expect(none).toMatchObject({
      mode: 'none',
      person_id: null,
      mapped_person_id: before.mapped_person_id,
      override_person_id: null,
      version: changed.version + 1,
    });
    expect((await context.request.get(mappedPath)).status()).toBe(404);
    expect((await context.request.get(manualPath)).status()).toBe(404);
    const restoredResponse = await api.put(path, {
      headers: {
        ...headers,
        'If-Match': noneResponse.headers().etag!,
        'Idempotency-Key': randomUUID(),
      },
      data: { mode: 'mapped' },
    });
    expect(restoredResponse.status()).toBe(200);
    const restored = (await restoredResponse.json()) as components['schemas']['AlbumCreator'];
    expect(restored).toMatchObject({
      mode: 'mapped',
      person_id: before.mapped_person_id,
      mapped_person_id: before.mapped_person_id,
      override_person_id: null,
      version: none.version + 1,
    });
    expect((await context.request.get(mappedPath)).status()).toBe(200);
    expect((await context.request.get(manualPath)).status()).toBe(404);
    const replay = await api.put(path, { headers: commandHeaders, data: choice });
    expect(replay.status()).toBe(200);
    expect(replay.headers().etag).toBe(changedResponse.headers().etag);
    expect(await replay.json()).toEqual(changed);
    expect(await (await api.get(path)).json()).toEqual(restored);
    expect((await context.request.get(`/api/v2/media/${prefix}-media/original`)).status()).toBe(
      404,
    );
    const policy = await api.get(`/api/v2/admin/albums/${albumID}/access`);
    expect(await policy.json()).toMatchObject({ restricted: false, group_ids: [] });
    expect(JSON.stringify({ changed, none, restored })).not.toMatch(
      /root_path|relative_path|source_config|Private\/|\.marker|\/private/,
    );
  } finally {
    const current = await api.get(path);
    if (current.status() === 200) {
      const value = (await current.json()) as components['schemas']['AlbumCreator'];
      if (value.mode !== 'mapped') {
        expect(
          (
            await api.put(path, {
              headers: {
                ...headers,
                'If-Match': current.headers().etag!,
                'Idempotency-Key': randomUUID(),
              },
              data: { mode: 'mapped' },
            })
          ).status(),
        ).toBe(200);
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
