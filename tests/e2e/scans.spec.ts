import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('HTTPS scan enqueue freezes private intents and preserves administrator-only control', async ({
  account,
  administrator,
  page,
  context,
  request,
}, testInfo) => {
  const path = '/api/v2/admin/scans';
  const sourceID = `media-${testInfo.project.name}-source`;
  const albumID = `media-${testInfo.project.name}-album`;
  expect((await request.post(path, { data: { mode: 'full' } })).status()).toBe(401);
  await signIn(page, account);
  const csrf = (await (await context.request.get('/api/v2/auth/csrf')).json()) as { token: string };
  expect(
    (
      await context.request.post(path, {
        headers: {
          Origin: 'https://127.0.0.1:4318',
          'X-CSRF-Token': csrf.token,
          'Idempotency-Key': randomUUID(),
        },
        data: { source_id: sourceID, mode: 'full' },
      })
    ).status(),
  ).toBe(403);

  const acceptedJobs = new Set<string>();
  try {
    const body: components['schemas']['ScanRequest'] = { source_id: sourceID, mode: 'full' };
    const headers = { ...administrator.headers, 'Idempotency-Key': randomUUID() };
    const accepted = await administrator.api.post(path, { headers, data: body });
    expect(accepted.status()).toBe(202);
    expect(accepted.headers()['cache-control']).toBe('private, no-store');
    const job = (await accepted.json()) as components['schemas']['Job'];
    acceptedJobs.add(job.id);
    expect(job).toMatchObject({
      kind: 'scan',
      status: 'queued',
      attempts: 0,
      total_known: false,
      done: 0,
    });
    expect(accepted.headers().location).toBe(`/api/v2/admin/jobs/${job.id}`);
    expect(accepted.headers().etag).toBe(`"job:${job.id}:${job.version}"`);
    expect(JSON.stringify(job)).not.toMatch(
      /scope_path|payload|lease_owner|root_path|source_config_version/,
    );
    const replay = await administrator.api.post(path, { headers, data: body });
    expect(replay.status()).toBe(202);
    expect(await replay.json()).toEqual(job);
    const coalesced = await administrator.api.post(path, {
      headers: { ...administrator.headers, 'Idempotency-Key': randomUUID() },
      data: body,
    });
    expect(coalesced.status()).toBe(202);
    expect(((await coalesced.json()) as components['schemas']['Job']).id).toBe(job.id);

    const allHeaders = { ...administrator.headers, 'Idempotency-Key': randomUUID() };
    const all = await administrator.api.post(path, {
      headers: allHeaders,
      data: { source_id: null, album_id: null, mode: 'full' },
    });
    expect(all.status()).toBe(202);
    const allJob = (await all.json()) as components['schemas']['Job'];
    acceptedJobs.add(allJob.id);
    expect(allJob.id).not.toBe(job.id);
    const nullReplay = await administrator.api.post(path, {
      headers: allHeaders,
      data: { mode: 'full' },
    });
    expect(nullReplay.status()).toBe(202);
    expect(await nullReplay.json()).toEqual(allJob);

    const target = await administrator.api.post(path, {
      headers: { ...administrator.headers, 'Idempotency-Key': randomUUID() },
      data: { album_id: albumID, mode: 'incremental' },
    });
    expect(target.status()).toBe(202);
    const targetJob = (await target.json()) as components['schemas']['Job'];
    acceptedJobs.add(targetJob.id);
    expect([job.id, allJob.id]).not.toContain(targetJob.id);
    const bad = await administrator.api.post(path, {
      headers: { ...administrator.headers, 'Idempotency-Key': randomUUID() },
      data: { mode: null },
    });
    expect(bad.status()).toBe(400);
    expect(await bad.json()).toMatchObject({ code: 'INVALID_JSON' });

    const source = await administrator.api.get(`/api/v2/admin/sources/${sourceID}`);
    expect(source.status()).toBe(200);
    expect(await source.json()).toMatchObject({ scan_generation: 0, last_full_scan_at: null });
    // Accepted queue records do not imply a worker, metadata publication, or codecs.
  } finally {
    for (const id of acceptedJobs) {
      const current = await administrator.api.get(`/api/v2/admin/jobs/${id}`);
      expect(current.status()).toBe(200);
      const state = (await current.json()) as components['schemas']['Job'];
      if (state.status !== 'queued' && state.status !== 'running') continue;
      const cancel = await administrator.api.post(`/api/v2/admin/jobs/${id}/cancel`, {
        headers: {
          ...administrator.headers,
          'If-Match': current.headers().etag!,
          'Idempotency-Key': randomUUID(),
        },
      });
      expect(cancel.status()).toBe(200);
    }
  }
});
