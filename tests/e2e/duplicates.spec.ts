import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('HTTPS indexed exact duplicates stay administrative, filtered, current-revision-only and cursor-bound', async ({
  account,
  administrator,
  page,
  context,
  request,
}, testInfo) => {
  const prefix = `duplicates-${testInfo.project.name}`;
  const path = '/api/v2/admin/duplicates/exact';
  const filters = new URLSearchParams({
    library_id: `${prefix}-library`,
    source_id: `${prefix}-source`,
  }).toString();
  const groupID = `${prefix}-media-revision`;
  const memberPath = `${path}/${groupID}/media`;
  for (const url of [path, memberPath]) {
    expect((await request.get(url)).status()).toBe(401);
  }
  await signIn(page, account);
  for (const url of [path, memberPath]) {
    expect((await context.request.get(url)).status()).toBe(403);
  }
  const response = await administrator.api.get(`${path}?${filters}`);
  expect(response.status()).toBe(200);
  expect(response.headers()['cache-control']).toBe('private, no-store');
  const groups = (await response.json()) as components['schemas']['ExactDuplicateGroupPage'];
  expect(groups.items).toHaveLength(1);
  expect(groups).toMatchObject({
    items: [{ id: groupID, media_count: 2 }],
    has_more: false,
    next_cursor: null,
  });
  expect(groups.items[0]!.source_size).toBeGreaterThan(0);
  expect(Object.keys(groups.items[0]!).sort()).toEqual(['id', 'media_count', 'source_size']);
  const membersResponse = await administrator.api.get(`${memberPath}?${filters}&limit=1`);
  expect(membersResponse.status()).toBe(200);
  expect(membersResponse.headers()['cache-control']).toBe('private, no-store');
  const first = (await membersResponse.json()) as components['schemas']['MediaPage'];
  expect(first.items).toHaveLength(1);
  expect(first.items[0]).toMatchObject({
    id: `${prefix}-media`,
    revision: `${prefix}-media-revision`,
    file_name: 'fixture.png',
    offline_capable: false,
  });
  expect(first.has_more).toBe(true);
  expect(first.next_cursor).not.toBeNull();
  const last = await administrator.api.get(
    `${memberPath}?${filters}&limit=1&cursor=${first.next_cursor!}`,
  );
  expect(last.status()).toBe(200);
  const finalPage = (await last.json()) as components['schemas']['MediaPage'];
  expect(finalPage).toMatchObject({
    items: [{ id: `${prefix}-other-media`, revision: `${prefix}-other-media-revision` }],
    has_more: false,
    next_cursor: null,
  });
  expect(JSON.stringify({ groups, first, finalPage })).not.toMatch(
    /source_sha256|relative_path|root_path|storage_path|\.marker|\/private/,
  );
  for (const url of [
    `${path}?cursor=${first.next_cursor!}`,
    `${memberPath}?cursor=${first.next_cursor!}`,
    `${path}/${prefix}-other-media-revision/media?${filters}&cursor=${first.next_cursor!}`,
  ]) {
    const invalid = await administrator.api.get(url);
    expect(invalid.status()).toBe(400);
    expect(await invalid.json()).toMatchObject({ code: 'INVALID_CURSOR' });
  }
  const empty = await administrator.api.get(`${path}?source_id=unknown-source`);
  expect(empty.status()).toBe(200);
  expect(await empty.json()).toEqual({ items: [], next_cursor: null, has_more: false });
  const historical = await administrator.api.get(`${path}/${prefix}-media-revision-old/media`);
  expect(historical.status()).toBe(404);
  const unsupported = await administrator.api.get(`${path}?perceptual=true`);
  expect(unsupported.status()).toBe(400);
  expect(await unsupported.json()).toMatchObject({ code: 'INVALID_FILTER' });
});
