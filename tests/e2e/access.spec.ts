import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('library grants, additive album restrictions, and deleted groups stay fail-closed over HTTPS', async ({
  account,
  administrator,
  page,
  context,
  request,
}, testInfo) => {
  const { api, headers } = administrator;
  const libraryID = `acl-${testInfo.project.name}-library`;
  const openID = `acl-${testInfo.project.name}-open`;
  const privateID = `acl-${testInfo.project.name}-restricted`;
  const libraryPolicy = `/api/v2/admin/libraries/${libraryID}/access`;
  const albumPolicy = `/api/v2/admin/albums/${privateID}/access`;
  const membership = `/api/v2/admin/users/${account.user.id}/groups`;
  const groups: string[] = [];
  try {
    for (const name of ['Library', 'Restricted']) {
      const response = await api.post('/api/v2/admin/groups', {
        headers: { ...headers, 'Idempotency-Key': randomUUID() },
        data: { name: `${name}-${randomUUID()}` },
      });
      expect(response.status()).toBe(201);
      groups.push(((await response.json()) as components['schemas']['Group']).id);
    }
    const [baseGroup, restrictedGroup] = groups as [string, string];
    await signIn(page, account);
    const initialCookie = (await context.cookies()).find(
      (cookie) => cookie.name === '__Host-gallerygo_session',
    )!;
    expect((await request.get(`/api/v2/albums/${privateID}`)).status()).toBe(401);
    expect((await context.request.get(`/api/v2/albums/${privateID}`)).status()).toBe(404);
    expect((await context.request.get('/api/v2/admin/groups')).status()).toBe(403);
    let current = await api.get(membership);
    expect(current.status()).toBe(200);
    expect(
      (
        await api.put(membership, {
          headers: { ...headers, 'If-Match': current.headers().etag! },
          data: { group_ids: [baseGroup, restrictedGroup] },
        })
      ).status(),
    ).toBe(200);
    current = await api.get(albumPolicy);
    expect(
      (
        await api.put(albumPolicy, {
          headers: { ...headers, 'If-Match': current.headers().etag! },
          data: { restricted: true, group_ids: [restrictedGroup] },
        })
      ).status(),
    ).toBe(200);
    // An album grant alone must never bypass the parent library's empty policy.
    expect((await context.request.get(`/api/v2/albums/${privateID}`)).status()).toBe(404);
    current = await api.get(libraryPolicy);
    expect(
      (
        await api.put(libraryPolicy, {
          headers: { ...headers, 'If-Match': current.headers().etag! },
          data: { group_ids: [baseGroup] },
        })
      ).status(),
    ).toBe(200);
    const first = await context.request.get('/api/v2/albums?limit=1');
    expect(first.status()).toBe(200);
    const firstPage = (await first.json()) as components['schemas']['AlbumPage'];
    expect(firstPage.has_more).toBe(true);
    expect(firstPage.items).toHaveLength(1);
    expect(firstPage.next_cursor).not.toBeNull();
    const second = await context.request.get(
      `/api/v2/albums?limit=1&cursor=${firstPage.next_cursor!}`,
    );
    expect(second.status()).toBe(200);
    const secondPage = (await second.json()) as components['schemas']['AlbumPage'];
    expect(secondPage.items).toHaveLength(1);
    expect([firstPage.items[0]!.id, secondPage.items[0]!.id].sort()).toEqual(
      [openID, privateID].sort(),
    );
    const deleted = await api.delete(`/api/v2/admin/groups/${restrictedGroup}`, {
      headers: { ...headers, 'If-Match': `"group:${restrictedGroup}:1"` },
    });
    expect(deleted.status()).toBe(204);
    const policy = (await (
      await api.get(albumPolicy)
    ).json()) as components['schemas']['AlbumPolicy'];
    expect(policy).toMatchObject({ restricted: true, group_ids: [] });
    const hidden = await context.request.get(`/api/v2/albums/${privateID}`);
    expect(hidden.status()).toBe(404);
    expect(await hidden.text()).not.toContain('Fixture restricted');
    expect((await context.request.get(`/api/v2/albums/${openID}`)).status()).toBe(200);
    const stalePage = await context.request.get(`/api/v2/albums?cursor=${firstPage.next_cursor!}`);
    expect(stalePage.status()).toBe(400);
    expect(await stalePage.json()).toMatchObject({ code: 'INVALID_CURSOR' });
    const me = (await (
      await context.request.get('/api/v2/auth/me')
    ).json()) as components['schemas']['User'];
    expect(me.scope_id).not.toBe(account.user.scope_id);
    expect(
      (await context.cookies()).find((cookie) => cookie.name === '__Host-gallerygo_session')!.value,
    ).toBe(initialCookie.value);
    current = await api.get(albumPolicy);
    expect(
      (
        await api.put(albumPolicy, {
          headers: { ...headers, 'If-Match': current.headers().etag! },
          data: { restricted: false, group_ids: [] },
        })
      ).status(),
    ).toBe(200);
    expect((await context.request.get(`/api/v2/albums/${privateID}`)).status()).toBe(200);
    current = await api.get(membership);
    expect(
      (
        await api.put(membership, {
          headers: { ...headers, 'If-Match': current.headers().etag! },
          data: { group_ids: [] },
        })
      ).status(),
    ).toBe(200);
    expect((await context.request.get(`/api/v2/albums/${openID}`)).status()).toBe(404);
    await page.reload();
    await expect(page.getByRole('heading', { name: 'Account security' })).toBeVisible();
  } finally {
    for (const id of groups) {
      const current = await api.get(`/api/v2/admin/groups/${id}`);
      if (current.status() === 404) continue;
      expect(current.status()).toBe(200);
      expect(
        (
          await api.delete(`/api/v2/admin/groups/${id}`, {
            headers: { ...headers, 'If-Match': current.headers().etag! },
          })
        ).status(),
      ).toBe(204);
    }
  }
});
