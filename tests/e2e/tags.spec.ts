import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('HTTPS tag catalog keeps private labels administrative, versioned and replayable without implicit cascades', async ({
  account,
  administrator,
  page,
  context,
  request,
}, testInfo) => {
  const { api, headers } = administrator;
  const collection = '/api/v2/admin/tags';
  const name = `Label 字-${randomUUID()}`;
  const createHeaders = { ...headers, 'Idempotency-Key': randomUUID() };
  const createdResponse = await api.post(collection, {
    headers: createHeaders,
    data: { name: ` ${name} ` },
  });
  expect(createdResponse.status()).toBe(201);
  const created = (await createdResponse.json()) as components['schemas']['Tag'];
  const path = `${collection}/${created.id}`;
  try {
    expect(created).toEqual({ id: created.id, name, version: 1 });
    expect(createdResponse.headers().location).toBe(path);
    expect(createdResponse.headers()['cache-control']).toBe('private, no-store');
    expect((await request.get(collection)).status()).toBe(401);
    expect((await request.get(path)).status()).toBe(401);
    await signIn(page, account);
    for (const route of [collection, path, `${collection}/missing`]) {
      expect((await context.request.get(route)).status()).toBe(403);
    }
    expect((await context.request.post(collection, { data: { name: 'Denied' } })).status()).toBe(
      403,
    );
    expect((await context.request.patch(path, { data: { name: 'Denied' } })).status()).toBe(403);
    expect((await context.request.delete(path)).status()).toBe(403);
    const meBefore = (await (
      await context.request.get('/api/v2/auth/me')
    ).json()) as components['schemas']['User'];
    const firstPage = await api.get(`${collection}?limit=1`);
    expect(firstPage.status()).toBe(200);
    const catalog = (await firstPage.json()) as components['schemas']['TagPage'];
    expect(catalog.items).toHaveLength(1);
    expect(catalog.has_more).toBe(true);
    expect(catalog.next_cursor).not.toBeNull();
    expect((await api.get(`/api/v2/admin/groups?cursor=${catalog.next_cursor}`)).status()).toBe(
      400,
    );
    expect((await api.get(`${collection}?name=ignored-search`)).status()).toBe(400);
    const renameHeaders = {
      ...headers,
      'If-Match': createdResponse.headers().etag!,
      'Idempotency-Key': randomUUID(),
    };
    const patch = { name: `Renamed-${randomUUID()}` };
    const changedResponse = await api.patch(path, { headers: renameHeaders, data: patch });
    expect(changedResponse.status()).toBe(200);
    const changed = (await changedResponse.json()) as components['schemas']['Tag'];
    expect(changed).toEqual({ id: created.id, name: patch.name, version: 2 });
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
    expect(
      (
        await api.patch(path, {
          headers: { ...renameHeaders, 'Idempotency-Key': randomUUID() },
          data: { name: 'New stale command' },
        })
      ).status(),
    ).toBe(409);
    const replayCreate = await api.post(collection, { headers: createHeaders, data: { name } });
    expect(replayCreate.status()).toBe(201);
    expect(await replayCreate.json()).toEqual(created);
    expect(replayCreate.headers().etag).toBe(createdResponse.headers().etag);
    expect(await (await api.get(path)).json()).toEqual(changed);
    const duplicate = await api.post(collection, {
      headers: { ...headers, 'Idempotency-Key': randomUUID() },
      data: patch,
    });
    expect(duplicate.status()).toBe(409);
    expect(await duplicate.json()).toMatchObject({ code: 'TAG_NAME_TAKEN' });
    for (const data of [{ name: null }, { version: 7 }, { album_ids: ['hidden'] }]) {
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
    const assignedPath = `${collection}/tags-${testInfo.project.name}-assigned`;
    const assignedResponse = await api.get(assignedPath);
    expect(assignedResponse.status()).toBe(200);
    const rejectedDelete = await api.delete(assignedPath, {
      headers: {
        ...headers,
        'If-Match': assignedResponse.headers().etag!,
        'Idempotency-Key': randomUUID(),
      },
    });
    expect(rejectedDelete.status()).toBe(409);
    expect(await rejectedDelete.json()).toMatchObject({ code: 'TAG_IN_USE' });
    expect(await (await api.get(assignedPath)).json()).toEqual(await assignedResponse.json());
    const deleteHeaders = {
      ...headers,
      'If-Match': changedResponse.headers().etag!,
      'Idempotency-Key': randomUUID(),
    };
    for (let attempt = 0; attempt < 2; attempt++) {
      const deleted = await api.delete(path, { headers: deleteHeaders });
      expect(deleted.status()).toBe(204);
      expect(await deleted.body()).toHaveLength(0);
    }
    expect((await api.get(path)).status()).toBe(404);
    const replayRename = await api.patch(path, { headers: renameHeaders, data: patch });
    expect(replayRename.status()).toBe(200);
    expect(await replayRename.json()).toEqual(changed);
    expect(replayRename.headers().etag).toBe(changedResponse.headers().etag);
    const meAfter = (await (
      await context.request.get('/api/v2/auth/me')
    ).json()) as components['schemas']['User'];
    expect(meAfter.scope_id).toBe(meBefore.scope_id);
    expect(JSON.stringify({ created, changed })).not.toMatch(
      /root_path|relative_path|storage_path|source_config|\.marker|\/private/,
    );
  } finally {
    const current = await api.get(path);
    if (current.status() !== 404) {
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
      ).toBe(204);
    }
  }
});
