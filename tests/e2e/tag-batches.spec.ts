import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('HTTPS tag batch acceptance freezes metadata without applying it and preserves cancelled history', async ({
  account,
  administrator,
  page,
  context,
  request,
}, testInfo) => {
  const prefix = `tag-batches-${testInfo.project.name}`;
  const albumID = `${prefix}-album`;
  const sourcePath = `/api/v2/admin/sources/${prefix}-source`;
  const path = '/api/v2/admin/batches/tags';
  const albumPath = `/api/v2/admin/albums/${albumID}/tags`;
  const { api, headers } = administrator;
  const createdTag = await api.post('/api/v2/admin/tags', {
    headers: { ...headers, 'Idempotency-Key': randomUUID() },
    data: { name: `Frozen batch-${randomUUID()}` },
  });
  expect(createdTag.status()).toBe(201);
  const tag = (await createdTag.json()) as components['schemas']['Tag'];
  const jobIDs = new Set<string>();
  try {
    const body = { album_ids: [albumID], tag_ids: [tag.id] };
    expect((await request.post(path, { data: body })).status()).toBe(401);
    await signIn(page, account);
    expect((await context.request.post(path, { data: body })).status()).toBe(403);
    expect((await context.request.get('/api/v2/admin/batches/missing/targets')).status()).toBe(403);
    const initial = await api.get(albumPath);
    expect(initial.status()).toBe(200);
    const before = (await initial.json()) as components['schemas']['AlbumTagSet'];
    const commandHeaders = { ...headers, 'Idempotency-Key': randomUUID() };
    const response = await api.post(path, { headers: commandHeaders, data: body });
    expect(response.status()).toBe(202);
    expect(response.headers()['cache-control']).toBe('private, no-store');
    const accepted = (await response.json()) as components['schemas']['Job'];
    jobIDs.add(accepted.id);
    expect(accepted).toMatchObject({
      kind: 'batch_tags',
      status: 'queued',
      attempts: 0,
      total_known: false,
      total: 0,
      done: 0,
      succeeded: 0,
      failed: 0,
      skipped: 0,
      cancelled: 0,
    });
    expect(response.headers().location).toBe(`/api/v2/admin/jobs/${accepted.id}`);
    const targetPath = `/api/v2/admin/batches/${accepted.id}/targets`;
    const resultPath = `/api/v2/admin/batches/${accepted.id}/results`;
    // This control suite intentionally disables consumers. Actual application and
    // killed-worker recovery use the separate real compiled CLI integration tests.
    expect((await request.get(resultPath)).status()).toBe(401);
    expect((await context.request.get(resultPath)).status()).toBe(403);
    const resultsResponse = await api.get(resultPath);
    expect(resultsResponse.status()).toBe(200);
    expect(resultsResponse.headers()['cache-control']).toBe('private, no-store');
    const noResults = (await resultsResponse.json()) as components['schemas']['TagBatchResultPage'];
    expect(noResults).toEqual({ items: [], next_cursor: null, has_more: false });
    expect((await api.get(`${resultPath}?limit=101`)).status()).toBe(400);
    expect((await api.get(`${resultPath}?attempt=1`)).status()).toBe(400);
    expect((await request.get(targetPath)).status()).toBe(401);
    const targetsResponse = await api.get(targetPath);
    expect(targetsResponse.status()).toBe(200);
    const targets = (await targetsResponse.json()) as components['schemas']['TagBatchTargetPage'];
    expect(targets).toEqual({
      items: [
        {
          album_id: albumID,
          album_version: before.version,
          library_id: `${prefix}-library`,
          source_id: `${prefix}-source`,
          source_config_version: 1,
          tag_ids: [tag.id],
        },
      ],
      next_cursor: null,
      has_more: false,
    });
    expect(JSON.stringify({ accepted, targets })).not.toMatch(
      /root_path|relative_path|payload_json|lease_owner|\.marker|\/private/,
    );
    const coalesced = await api.post(path, {
      headers: { ...headers, 'Idempotency-Key': randomUUID() },
      data: body,
    });
    expect(coalesced.status()).toBe(202);
    expect(((await coalesced.json()) as components['schemas']['Job']).id).toBe(accepted.id);
    expect(await (await api.get(albumPath)).json()).toEqual(before);
    const source = await api.get(sourcePath);
    expect(source.status()).toBe(200);
    const edited = await api.patch(sourcePath, {
      headers: { ...headers, 'If-Match': source.headers().etag! },
      data: { minimum_files: 5 },
    });
    expect(edited.status()).toBe(200);
    const currentJob = await api.get(response.headers().location!);
    expect(currentJob.status()).toBe(200);
    expect(await currentJob.json()).toMatchObject({
      status: 'cancelled',
      attempts: 0,
      succeeded: 0,
    });
    expect(await (await api.get(targetPath)).json()).toEqual(targets);
    expect(await (await api.get(resultPath)).json()).toEqual(noResults);
    const replay = await api.post(path, { headers: commandHeaders, data: body });
    expect(replay.status()).toBe(202);
    expect(replay.headers().etag).toBe(response.headers().etag);
    expect(await replay.json()).toEqual(accepted);
    const freshResponse = await api.post(path, {
      headers: { ...headers, 'Idempotency-Key': randomUUID() },
      data: body,
    });
    expect(freshResponse.status()).toBe(202);
    const fresh = (await freshResponse.json()) as components['schemas']['Job'];
    jobIDs.add(fresh.id);
    expect(fresh.id).not.toBe(accepted.id);
    const freshTargets = await api.get(`/api/v2/admin/batches/${fresh.id}/targets`);
    expect(
      ((await freshTargets.json()) as components['schemas']['TagBatchTargetPage']).items[0]
        ?.source_config_version,
    ).toBe(2);
    expect(
      (
        await api.post(path, {
          headers: { ...headers, 'Idempotency-Key': randomUUID() },
          data: { album_ids: [albumID], tag_ids: null },
        })
      ).status(),
    ).toBe(400);
    expect(
      (
        await api.post(path, {
          headers: { ...headers, 'Idempotency-Key': randomUUID() },
          data: { album_ids: [albumID, albumID], tag_ids: [] },
        })
      ).status(),
    ).toBe(422);
    expect(await (await api.get(albumPath)).json()).toEqual(before);
    expect((await context.request.get(`/api/v2/media/${prefix}-media/original`)).status()).toBe(
      404,
    );
    const policy = await api.get(`/api/v2/admin/albums/${albumID}/access`);
    expect(await policy.json()).toMatchObject({ restricted: false, group_ids: [] });
  } finally {
    for (const id of jobIDs) {
      const current = await api.get(`/api/v2/admin/jobs/${id}`);
      expect(current.status()).toBe(200);
      const job = (await current.json()) as components['schemas']['Job'];
      if (job.status === 'queued' || job.status === 'running') {
        const cancelled = await api.post(`/api/v2/admin/jobs/${id}/cancel`, {
          headers: {
            ...headers,
            'If-Match': current.headers().etag!,
            'Idempotency-Key': randomUUID(),
          },
        });
        expect(cancelled.status()).toBe(200);
        expect(await cancelled.json()).toMatchObject({ status: 'cancelled', succeeded: 0 });
      }
    }
    const currentTag = await api.get(`/api/v2/admin/tags/${tag.id}`);
    expect(
      (
        await api.delete(`/api/v2/admin/tags/${tag.id}`, {
          headers: {
            ...headers,
            'If-Match': currentTag.headers().etag!,
            'Idempotency-Key': randomUUID(),
          },
        })
      ).status(),
    ).toBe(204);
  }
});
