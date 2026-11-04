import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('administrative account changes take effect in the real browser without restoring old sessions', async ({
  account,
  administrator,
  page,
  context,
}) => {
  const { api, headers } = administrator;
  const path = `/api/v2/admin/users/${account.user.id}`;
  await signIn(page, account);
  const originalCookie = (await context.cookies()).find(
    (cookie) => cookie.name === '__Host-gallerygo_session',
  )!;
  expect((await context.request.get('/api/v2/admin/users')).status()).toBe(403);
  const csrf = (await (await context.request.get('/api/v2/auth/csrf')).json()) as { token: string };
  const escalation = await context.request.patch(path, {
    headers: { Origin: 'https://127.0.0.1:4318', 'X-CSRF-Token': csrf.token },
    data: { role: 'admin' },
  });
  expect(escalation.status()).toBe(403);
  expect(await escalation.json()).toMatchObject({ code: 'ADMIN_REQUIRED' });

  const original = await api.get(path);
  expect(original.status()).toBe(200);
  const originalTag = original.headers().etag!;
  const disabled = await api.patch(path, {
    headers: { ...headers, 'If-Match': originalTag },
    data: { active: false },
  });
  expect(disabled.status()).toBe(200);
  expect(await disabled.json()).toMatchObject({ active: false, version: 2 });
  await page.reload();
  await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
  const stale = await api.patch(path, {
    headers: { ...headers, 'If-Match': originalTag },
    data: { active: true },
  });
  expect(stale.status()).toBe(409);
  expect(await stale.json()).toMatchObject({ code: 'VERSION_CONFLICT' });

  const enabled = await api.patch(path, {
    headers: { ...headers, 'If-Match': disabled.headers().etag! },
    data: { active: true },
  });
  expect(enabled.status()).toBe(200);
  expect((await context.request.get('/api/v2/auth/me')).status()).toBe(401);
  await signIn(page, account);
  const freshCookie = (await context.cookies()).find(
    (cookie) => cookie.name === '__Host-gallerygo_session',
  )!;
  expect(freshCookie.value).not.toBe(originalCookie.value);

  const resetOptions = {
    headers: {
      ...headers,
      'If-Match': enabled.headers().etag!,
      'Idempotency-Key': randomUUID(),
    },
    data: { new_password: account.password + '-reset' },
  };
  const reset = await api.post(`${path}/reset-password`, resetOptions);
  expect(reset.status()).toBe(200);
  await page.reload();
  await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
  await signIn(page, account, resetOptions.data.new_password);
  const replay = await api.post(`${path}/reset-password`, resetOptions);
  expect(replay.status()).toBe(200);
  expect(await replay.json()).toEqual(await reset.json());
  expect((await context.request.get('/api/v2/auth/me')).status()).toBe(200);

  const deleted = await api.delete(path, {
    headers: { ...headers, 'If-Match': reset.headers().etag! },
  });
  expect(deleted.status()).toBe(204);
  expect((await api.get(path)).status()).toBe(404);
  await page.reload();
  await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
});

test('administrators can inspect and revoke another account session with an audit trail', async ({
  account,
  administrator,
  page,
  context,
}) => {
  await signIn(page, account);
  const { api, headers } = administrator;
  const path = `/api/v2/admin/users/${account.user.id}/sessions`;
  const response = await api.get(path);
  expect(response.status()).toBe(200);
  const sessions = (await response.json()) as components['schemas']['SessionPage'];
  expect(sessions.items).toHaveLength(1);
  const session = sessions.items[0]!;
  expect(session.current).toBe(false);
  const cookies = await context.cookies();
  for (const cookie of cookies) expect(await response.text()).not.toContain(cookie.value);
  expect((await context.request.get('/api/v2/admin/audit')).status()).toBe(403);
  const revoked = await api.delete(`${path}/${session.id}`, { headers });
  expect(revoked.status()).toBe(204);
  await page.reload();
  await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
  const auditResponse = await api.get('/api/v2/admin/audit?limit=100');
  expect(auditResponse.status()).toBe(200);
  const audit = (await auditResponse.json()) as components['schemas']['AuditPage'];
  expect(
    audit.items.find(
      (event) => event.event_type === 'admin_session_revoked' && event.resource_id === session.id,
    ),
  ).toMatchObject({ detail: { user_id: account.user.id, current: false } });
  expect(await auditResponse.text()).not.toContain(account.password);
});
