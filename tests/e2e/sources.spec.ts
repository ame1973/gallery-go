import { randomUUID } from 'node:crypto';
import { expect, signIn, test } from './fixtures';
import type { components } from '../../web/src/lib/api/schema';

test('HTTPS source configuration uses declared roots and preserves unavailable mounts', async ({
  account,
  administrator,
  page,
  context,
  request,
}, testInfo) => {
  expect((await request.get('/api/v2/admin/source-roots')).status()).toBe(401);
  const rootsResponse = await administrator.api.get('/api/v2/admin/source-roots?limit=100');
  expect(rootsResponse.status()).toBe(200);
  const roots = (await rootsResponse.json()) as components['schemas']['SourceRootPage'];
  const rootName = `managed-${testInfo.project.name}`;
  expect(roots.items).toContainEqual({ root_name: rootName });
  for (const root of roots.items) expect(Object.keys(root)).toEqual(['root_name']);

  const createdLibrary = await administrator.api.post('/api/v2/admin/libraries', {
    headers: { ...administrator.headers, 'Idempotency-Key': randomUUID() },
    data: { name: `Source management ${randomUUID()}` },
  });
  expect(createdLibrary.status()).toBe(201);
  const library = (await createdLibrary.json()) as components['schemas']['Library'];
  const registrations: string[] = [];
  try {
    const body = {
      library_id: library.id,
      root_name: rootName,
      mapping: { album_depth: 3, person_depth: 2 },
      minimum_files: 10,
    };
    const creationHeaders = { ...administrator.headers, 'Idempotency-Key': randomUUID() };
    const created = await administrator.api.post('/api/v2/admin/sources', {
      headers: creationHeaders,
      data: body,
    });
    expect(created.status()).toBe(201);
    const source = (await created.json()) as components['schemas']['Source'];
    const path = `/api/v2/admin/sources/${source.id}`;
    registrations.push(path);
    expect(source).toMatchObject({
      active: true,
      configured: true,
      state: 'unknown',
      scan_generation: 0,
    });
    expect(source.mapping).toEqual(body.mapping);
    expect(JSON.stringify(source)).not.toMatch(/root_path|root_config_sha256|\.marker/);
    const replay = await administrator.api.post('/api/v2/admin/sources', {
      headers: creationHeaders,
      data: body,
    });
    expect(replay.status()).toBe(201);
    expect(await replay.json()).toEqual(source);

    await signIn(page, account);
    for (const url of ['/api/v2/admin/source-roots', '/api/v2/admin/sources', path]) {
      expect((await context.request.get(url)).status()).toBe(403);
    }
    const probeHeaders = {
      ...administrator.headers,
      'If-Match': created.headers().etag!,
      'Idempotency-Key': randomUUID(),
    };
    const probe = await administrator.api.post(`${path}/check`, { headers: probeHeaders });
    expect(probe.status()).toBe(200);
    const healthy = (await probe.json()) as components['schemas']['Source'];
    // Marker reachability is not inventory, nor a successful minimum-files guard.
    expect(healthy).toMatchObject({
      state: 'healthy',
      scan_generation: 0,
      last_full_scan_at: null,
    });
    expect(healthy.config_version).toBe(source.config_version);
    const updated = await administrator.api.patch(path, {
      headers: { ...administrator.headers, 'If-Match': probe.headers().etag! },
      data: { active: false },
    });
    expect(updated.status()).toBe(200);
    const inactive = (await updated.json()) as components['schemas']['Source'];
    expect(inactive).toMatchObject({ active: false, state: 'unknown', last_checked_at: null });
    expect(inactive.config_version).toBe(source.config_version + 1);
    const oldProbe = await administrator.api.post(`${path}/check`, { headers: probeHeaders });
    expect(oldProbe.status()).toBe(200);
    expect(await oldProbe.json()).toEqual(healthy);
    const current = await administrator.api.get(path);
    expect(await current.json()).toEqual(inactive);
    expect(
      (
        await administrator.api.delete(path, {
          headers: { ...administrator.headers, 'If-Match': current.headers().etag! },
        })
      ).status(),
    ).toBe(204);

    const unavailable = await administrator.api.post('/api/v2/admin/sources', {
      headers: { ...administrator.headers, 'Idempotency-Key': randomUUID() },
      data: { library_id: library.id, root_name: `unmounted-${testInfo.project.name}` },
    });
    expect(unavailable.status()).toBe(201);
    const missing = (await unavailable.json()) as components['schemas']['Source'];
    const missingPath = `/api/v2/admin/sources/${missing.id}`;
    registrations.push(missingPath);
    const checked = await administrator.api.post(`${missingPath}/check`, {
      headers: {
        ...administrator.headers,
        'If-Match': unavailable.headers().etag!,
        'Idempotency-Key': randomUUID(),
      },
    });
    expect(checked.status()).toBe(200);
    expect(await checked.json()).toMatchObject({
      state: 'unavailable',
      configured: true,
      last_error_code: 'SOURCE_UNAVAILABLE',
      last_healthy_at: null,
      scan_generation: 0,
    });
  } finally {
    for (const path of registrations) {
      const source = await administrator.api.get(path);
      if (source.status() === 404) continue;
      expect(source.status()).toBe(200);
      expect(
        (
          await administrator.api.delete(path, {
            headers: { ...administrator.headers, 'If-Match': source.headers().etag! },
          })
        ).status(),
      ).toBe(204);
    }
    const currentLibrary = await administrator.api.get(`/api/v2/admin/libraries/${library.id}`);
    expect(currentLibrary.status()).toBe(200);
    expect(
      (
        await administrator.api.delete(`/api/v2/admin/libraries/${library.id}`, {
          headers: { ...administrator.headers, 'If-Match': currentLibrary.headers().etag! },
        })
      ).status(),
    ).toBe(204);
  }
});
