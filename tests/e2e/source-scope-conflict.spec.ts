import { randomUUID } from 'node:crypto';
import { expect, signIn } from './fixtures';
import { isolatedTest } from './isolated-fixtures';
import type { components } from '../../web/src/lib/api/schema';

isolatedTest(
  'scope changes reject stale account versions without revoking the active session',
  async ({ account, administrator, page }) => {
    const { api, headers } = administrator;
    const accountPath = `/api/v2/admin/users/${account.user.id}`;
    const initial = await api.get(accountPath);
    expect(initial.status()).toBe(200);
    const promoted = await api.patch(accountPath, {
      headers: { ...headers, 'If-Match': initial.headers().etag! },
      data: { role: 'admin' },
    });
    expect(promoted.status()).toBe(200);
    await signIn(page, account);
    const before = await page.request.get('/api/v2/auth/me');
    expect(before.status()).toBe(200);
    const previous = (await before.json()) as components['schemas']['User'];
    const accountRead = await api.get(accountPath);
    expect(accountRead.status()).toBe(200);
    const tag = accountRead.headers().etag!;
    const created = await api.post('/api/v2/admin/libraries', {
      headers: { ...headers, 'Idempotency-Key': randomUUID() },
      data: { name: `isolated-scope-${randomUUID()}` },
    });
    expect(created.status()).toBe(201);
    const library = (await created.json()) as components['schemas']['Library'];
    let libraryTag = created.headers().etag!;
    try {
      const changed = await api.patch(`/api/v2/admin/libraries/${library.id}`, {
        headers: { ...headers, 'If-Match': libraryTag },
        data: { active: false },
      });
      expect(changed.status()).toBe(200);
      libraryTag = changed.headers().etag!;
      const stale = await api.patch(accountPath, {
        headers: { ...headers, 'If-Match': tag },
        data: { role: 'user' },
      });
      expect(stale.status()).toBe(409);
      expect(await stale.json()).toMatchObject({ code: 'VERSION_CONFLICT' });
      const after = await page.request.get('/api/v2/auth/me');
      expect(after.status()).toBe(200);
      const current = (await after.json()) as components['schemas']['User'];
      expect(current.id).toBe(previous.id);
      expect(current.role).toBe('admin');
      expect(current.scope_id).not.toBe(previous.scope_id);
      expect(current.version).toBeGreaterThan(previous.version);
      const latest = await api.get(accountPath);
      expect(latest.status()).toBe(200);
      const demoted = await api.patch(accountPath, {
        headers: { ...headers, 'If-Match': latest.headers().etag! },
        data: { role: 'user' },
      });
      expect(demoted.status()).toBe(200);
      expect((await page.request.get('/api/v2/auth/me')).status()).toBe(401);
    } finally {
      const removed = await api.delete(`/api/v2/admin/libraries/${library.id}`, {
        headers: { ...headers, 'If-Match': libraryTag },
      });
      expect(removed.status()).toBe(204);
    }
  },
);
