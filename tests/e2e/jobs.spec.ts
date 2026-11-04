import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('HTTPS job controls enforce administrator access, versions, filter cursors, and replay safety', async ({
  account,
  administrator,
  page,
  context,
  request,
}, testInfo) => {
  const path = `/api/v2/admin/jobs/jobs-${testInfo.project.name}-queued`;
  expect((await request.get(path)).status()).toBe(401);
  const initial = await administrator.api.get(path);
  expect(initial.status()).toBe(200);
  expect(initial.headers()['cache-control']).toBe('private, no-store');
  const first = (await initial.json()) as components['schemas']['Job'];
  expect(first).toMatchObject({ status: 'queued', attempts: 0, done: 0, total_known: false });
  expect(JSON.stringify(first)).not.toMatch(/private_path|lease_owner|dedupe|payload/);
  const initialTag = initial.headers().etag!;

  await signIn(page, account);
  for (const url of ['/api/v2/admin/jobs', path, `${path}/attempts`]) {
    expect((await context.request.get(url)).status()).toBe(403);
  }
  const csrf = (await (await context.request.get('/api/v2/auth/csrf')).json()) as { token: string };
  expect(
    (
      await context.request.post(`${path}/cancel`, {
        headers: {
          Origin: 'https://127.0.0.1:4318',
          'X-CSRF-Token': csrf.token,
          'If-Match': initialTag,
          'Idempotency-Key': randomUUID(),
        },
      })
    ).status(),
  ).toBe(403);

  const list = await administrator.api.get('/api/v2/admin/jobs?status=queued&limit=1');
  expect(list.status()).toBe(200);
  const pageResult = (await list.json()) as components['schemas']['JobPage'];
  expect(pageResult.has_more).toBe(true);
  expect(pageResult.next_cursor).not.toBeNull();
  const invalidCursor = await administrator.api.get(
    `/api/v2/admin/jobs?status=running&cursor=${pageResult.next_cursor!}`,
  );
  expect(invalidCursor.status()).toBe(400);
  expect(await invalidCursor.json()).toMatchObject({ code: 'INVALID_CURSOR' });

  const cancelHeaders = {
    ...administrator.headers,
    'If-Match': initialTag,
    'Idempotency-Key': randomUUID(),
  };
  const cancelled = await administrator.api.post(`${path}/cancel`, { headers: cancelHeaders });
  expect(cancelled.status()).toBe(200);
  const cancelledJob = (await cancelled.json()) as components['schemas']['Job'];
  expect(cancelledJob).toMatchObject({ status: 'cancelled', cancel_requested: true });
  const retryHeaders = {
    ...administrator.headers,
    'If-Match': cancelled.headers().etag!,
    'Idempotency-Key': randomUUID(),
  };
  const retry = await administrator.api.post(`${path}/retry`, { headers: retryHeaders });
  expect(retry.status()).toBe(202);
  const queued = (await retry.json()) as components['schemas']['Job'];
  expect(queued).toMatchObject({ status: 'queued', cancel_requested: false, attempts: 0 });
  const replay = await administrator.api.post(`${path}/cancel`, { headers: cancelHeaders });
  expect(replay.status()).toBe(200);
  expect(await replay.json()).toEqual(cancelledJob);
  const replayRetry = await administrator.api.post(`${path}/retry`, { headers: retryHeaders });
  expect(replayRetry.status()).toBe(202);
  expect(await replayRetry.json()).toEqual(queued);
  const current = await administrator.api.get(path);
  expect(current.headers().etag).toBe(retry.headers().etag);
  expect(await current.json()).toEqual(queued);
  const history = await administrator.api.get(`${path}/attempts`);
  expect(history.status()).toBe(200);
  expect(await history.json()).toEqual({ items: [], next_cursor: null, has_more: false });
  const invalidBody = await administrator.api.post(`${path}/cancel`, {
    headers: {
      ...administrator.headers,
      'If-Match': current.headers().etag!,
      'Idempotency-Key': randomUUID(),
    },
    data: { lease_owner: 'not-a-capability' },
  });
  expect(invalidBody.status()).toBe(400);
  expect(await invalidBody.json()).toMatchObject({ code: 'INVALID_BODY' });
});
