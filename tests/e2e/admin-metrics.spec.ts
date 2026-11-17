import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('HTTPS metrics expose only current job counts to administrators', async ({
  account,
  administrator,
  context,
  page,
  request,
}) => {
  const path = '/api/v2/admin/metrics';
  expect((await request.get(path)).status()).toBe(401);
  await signIn(page, account);
  expect((await context.request.get(path)).status()).toBe(403);

  const response = await administrator.api.get(path);
  expect(response.status()).toBe(200);
  expect(response.headers()['cache-control']).toBe('private, no-store');
  const metrics = (await response.json()) as components['schemas']['AdminMetrics'];
  expect(Object.keys(metrics)).toEqual(['jobs']);
  expect(Object.keys(metrics.jobs).sort()).toEqual([
    'cancelled',
    'dead',
    'failed',
    'queued',
    'running',
    'succeeded',
  ]);
  for (const count of Object.values(metrics.jobs)) {
    expect(Number.isSafeInteger(count)).toBe(true);
    expect(count).toBeGreaterThanOrEqual(0);
  }
  // The isolated HTTPS server seeds 34 immutable dead-history rows per project.
  expect(metrics.jobs.dead).toBeGreaterThanOrEqual(68);
  expect(JSON.stringify(metrics)).not.toMatch(/private|lease|payload|path|token/);

  const invalid = await administrator.api.get(`${path}?status=queued`);
  expect(invalid.status()).toBe(400);
  expect(await invalid.json()).toMatchObject({ code: 'INVALID_FILTER' });
});
