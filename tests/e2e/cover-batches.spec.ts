import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('HTTPS cover batches freeze exact media epochs without grants and retain cancelled control history', async ({
  account,
  administrator,
  page,
  context,
  request,
}, testInfo) => {
  const prefix = `cover-batches-${testInfo.project.name}`;
  const albumID = `${prefix}-album`;
  const selected = `${prefix}-other-media`;
  const path = '/api/v2/admin/batches/covers';
  const { api, headers } = administrator;
  const jobs = new Set<string>();
  const body = { targets: [{ album_id: albumID, cover_media_id: selected }] };
  try {
    expect((await request.post(path, { data: body })).status()).toBe(401);
    await signIn(page, account);
    expect((await context.request.post(path, { data: body })).status()).toBe(403);
    const initial = await api.get(`/api/v2/admin/albums/${albumID}`);
    expect(initial.status()).toBe(200);
    const before = (await initial.json()) as components['schemas']['AlbumMetadata'];
    const commandHeaders = { ...headers, 'Idempotency-Key': randomUUID() };
    const acceptedResponse = await api.post(path, { headers: commandHeaders, data: body });
    expect(acceptedResponse.status()).toBe(202);
    expect(acceptedResponse.headers()['cache-control']).toBe('private, no-store');
    const accepted = (await acceptedResponse.json()) as components['schemas']['Job'];
    jobs.add(accepted.id);
    expect(accepted).toMatchObject({
      kind: 'batch_covers',
      status: 'queued',
      attempts: 0,
      total_known: false,
      done: 0,
    });
    const targetPath = `/api/v2/admin/batches/${accepted.id}/covers`;
    expect((await request.get(targetPath)).status()).toBe(401);
    expect((await context.request.get(targetPath)).status()).toBe(403);
    const targetResponse = await api.get(targetPath);
    expect(targetResponse.status()).toBe(200);
    const targets = (await targetResponse.json()) as components['schemas']['CoverBatchTargetPage'];
    expect(targets).toEqual({
      items: [
        {
          album_id: albumID,
          album_version: before.version,
          library_id: `${prefix}-library`,
          source_id: `${prefix}-source`,
          source_config_version: 1,
          cover_media_id: selected,
          projected_cover_media_id: selected,
          media_version: 1,
          revision_id: `${selected}-revision`,
        },
      ],
      next_cursor: null,
      has_more: false,
    });
    expect(JSON.stringify({ accepted, targets })).not.toMatch(
      /root_path|relative_path|lease_owner|payload_json|\.marker|\/private/,
    );
    const coalesced = await api.post(path, {
      headers: { ...headers, 'Idempotency-Key': randomUUID() },
      data: body,
    });
    expect(coalesced.status()).toBe(202);
    expect(((await coalesced.json()) as components['schemas']['Job']).id).toBe(accepted.id);
    // Consumers are deliberately disabled in this control harness; real cover
    // application and SIGKILL/reopen are exercised by the compiled CLI regressions.
    const resultPath = `/api/v2/admin/batches/${accepted.id}/results`;
    expect(await (await api.get(resultPath)).json()).toEqual({
      items: [],
      next_cursor: null,
      has_more: false,
    });
    expect(await (await api.get(`/api/v2/admin/albums/${albumID}`)).json()).toEqual(before);
    const sourcePath = `/api/v2/admin/sources/${prefix}-source`;
    const source = await api.get(sourcePath);
    const changed = await api.patch(sourcePath, {
      headers: { ...headers, 'If-Match': source.headers().etag! },
      data: { minimum_files: 5 },
    });
    expect(changed.status()).toBe(200);
    const current = await api.get(`/api/v2/admin/jobs/${accepted.id}`);
    expect(await current.json()).toMatchObject({ status: 'cancelled', attempts: 0, succeeded: 0 });
    expect(await (await api.get(targetPath)).json()).toEqual(targets);
    const historical = await api.post(path, { headers: commandHeaders, data: body });
    expect(historical.status()).toBe(202);
    expect(historical.headers().etag).toBe(acceptedResponse.headers().etag);
    expect(await historical.json()).toEqual(accepted);
    const freshResponse = await api.post(path, {
      headers: { ...headers, 'Idempotency-Key': randomUUID() },
      data: body,
    });
    expect(freshResponse.status()).toBe(202);
    const fresh = (await freshResponse.json()) as components['schemas']['Job'];
    jobs.add(fresh.id);
    expect(fresh.id).not.toBe(accepted.id);
    expect(
      (
        await api.post(path, {
          headers: { ...headers, 'Idempotency-Key': randomUUID() },
          data: { targets: [{ album_id: albumID, cover_media_id: null }] },
        })
      ).status(),
    ).toBe(400);
    expect(
      (
        await api.post(path, {
          headers: { ...headers, 'Idempotency-Key': randomUUID() },
          data: { targets: [{ album_id: albumID }] },
        })
      ).status(),
    ).toBe(422);
    expect((await api.get(`${targetPath}?limit=101`)).status()).toBe(400);
    expect((await api.get(`${targetPath}?status=queued`)).status()).toBe(400);
    expect((await context.request.get(`/api/v2/media/${selected}/original`)).status()).toBe(404);
    const policy = await api.get(`/api/v2/admin/albums/${albumID}/access`);
    expect(await policy.json()).toMatchObject({ restricted: false, group_ids: [] });
  } finally {
    for (const id of jobs) {
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
  }
});
