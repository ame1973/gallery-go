import { randomUUID } from 'node:crypto';
import { readFile, stat } from 'node:fs/promises';
import { expect, signIn } from './fixtures';
import { isolatedTest } from './isolated-fixtures';
import type { components } from '../../web/src/lib/api/schema';

isolatedTest.use({ realScan: true });

isolatedTest(
  'a fresh installation scans a read-only source and serves an authorized download',
  async ({ account, administrator, isolatedServer, page, request }) => {
    isolatedTest.setTimeout(90_000);
    const sourcePath = isolatedServer.scanSource;
    if (sourcePath === null) throw new Error('Missing isolated scan source');
    const sourceBefore = await stat(sourcePath, { bigint: true });
    const original = await readFile(sourcePath);
    expect(sourceBefore.mode & 0o777n).toBe(0o400n);
    expect(original).toEqual(
      Buffer.from(
        'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAAEElEQVR4nGKSq9gCCAAA//8CCgFNa0v7mgAAAABJRU5ErkJggg==',
        'base64',
      ),
    );
    const { api, headers } = administrator;
    for (const path of [
      '/api/v2/admin/libraries',
      '/api/v2/admin/sources',
      '/api/v2/admin/jobs',
      '/api/v2/albums',
    ]) {
      const empty = await api.get(path);
      expect(empty.status()).toBe(200);
      expect(await empty.json()).toMatchObject({ items: [] });
    }
    const roots = await api.get('/api/v2/admin/source-roots');
    expect(roots.status()).toBe(200);
    expect(await roots.json()).toMatchObject({ items: [{ root_name: 'minimum-flow' }] });
    const libraryResponse = await api.post('/api/v2/admin/libraries', {
      headers: { ...headers, 'Idempotency-Key': randomUUID() },
      data: { name: 'Isolated real scan' },
    });
    expect(libraryResponse.status()).toBe(201);
    const library = (await libraryResponse.json()) as components['schemas']['Library'];
    const sourceResponse = await api.post('/api/v2/admin/sources', {
      headers: { ...headers, 'Idempotency-Key': randomUUID() },
      data: { library_id: library.id, root_name: 'minimum-flow', minimum_files: 1 },
    });
    expect(sourceResponse.status()).toBe(201);
    const source = (await sourceResponse.json()) as components['schemas']['Source'];
    expect(source.scan_generation).toBe(0);
    const accepted = await api.post('/api/v2/admin/scans', {
      headers: { ...headers, 'Idempotency-Key': randomUUID() },
      data: { source_id: source.id, mode: 'full' },
    });
    expect(accepted.status()).toBe(202);
    const initial = (await accepted.json()) as components['schemas']['Job'];
    let job = initial;
    await expect
      .poll(
        async () => {
          const response = await api.get(`/api/v2/admin/jobs/${initial.id}`);
          expect(response.status()).toBe(200);
          job = (await response.json()) as components['schemas']['Job'];
          if (!['queued', 'running', 'succeeded'].includes(job.status)) {
            throw new Error(`Real scan failed: ${job.status} ${job.error_code ?? ''}`);
          }
          return job.status;
        },
        { timeout: 30_000, intervals: [100, 250, 500] },
      )
      .toBe('succeeded');
    expect(job).toMatchObject({
      kind: 'scan',
      attempts: 1,
      total_known: true,
      total: 3,
      done: 3,
      succeeded: 3,
      failed: 0,
      skipped: 0,
      cancelled: 0,
    });

    const albumsResponse = await api.get('/api/v2/albums');
    expect(albumsResponse.status()).toBe(200);
    const albums = (await albumsResponse.json()) as components['schemas']['AlbumPage'];
    expect(albums.items).toHaveLength(1);
    expect(albums.items[0]!.title).toBe('Album');
    const mediaResponse = await api.get(`/api/v2/albums/${albums.items[0]!.id}/media`);
    expect(mediaResponse.status()).toBe(200);
    const media = (await mediaResponse.json()) as components['schemas']['MediaPage'];
    expect(media.items).toHaveLength(1);
    expect(media.items[0]).toMatchObject({
      file_name: 'photo.png',
      kind: 'image',
      width: 1,
      height: 1,
    });
    const mediaID = media.items[0]!.id;
    const revision = media.items[0]!.revision;
    expect(revision).toBeTruthy();
    const originalPath = `/api/v2/media/${mediaID}/original`;
    expect((await request.get(originalPath)).status()).toBe(401);

    const accountPath = `/api/v2/admin/users/${account.user.id}`;
    const currentAccount = await api.get(accountPath);
    expect(currentAccount.status()).toBe(200);
    const withOriginal = await api.patch(accountPath, {
      headers: { ...headers, 'If-Match': currentAccount.headers().etag! },
      data: { download_original: true },
    });
    expect(withOriginal.status()).toBe(200);
    await signIn(page, account);
    await page.goto('/albums');
    await expect(page.getByText('No albums are available on this page.')).toBeVisible();
    expect((await page.request.get(originalPath)).status()).toBe(404);

    const createdGroup = await api.post('/api/v2/admin/groups', {
      headers: { ...headers, 'Idempotency-Key': randomUUID() },
      data: { name: `Minimum flow ${randomUUID()}` },
    });
    expect(createdGroup.status()).toBe(201);
    const group = (await createdGroup.json()) as components['schemas']['Group'];
    const memberPath = `/api/v2/admin/users/${account.user.id}/groups`;
    for (const path of [`/api/v2/admin/libraries/${library.id}/access`, memberPath]) {
      const current = await api.get(path);
      expect(current.status()).toBe(200);
      const updated = await api.put(path, {
        headers: { ...headers, 'If-Match': current.headers().etag! },
        data: { group_ids: [group.id] },
      });
      expect(updated.status()).toBe(200);
    }
    const originalRequests: string[] = [];
    page.on('request', (entry) => {
      if (new URL(entry.url()).pathname === originalPath) originalRequests.push(entry.url());
    });
    await page.reload();
    await page.locator('.gallery-grid').getByRole('link', { name: /Album/ }).click();
    await expect(page.getByRole('heading', { name: 'Album', exact: true })).toBeVisible();
    await page.getByRole('link', { name: 'View photo.png details' }).click();
    await expect(page.getByRole('heading', { name: 'photo.png' })).toBeVisible();
    await expect(page.getByText('Preview not shown')).toBeVisible();
    const variants = await page.request.get(`/api/v2/media/${mediaID}/variants`);
    expect(variants.status()).toBe(200);
    expect(await variants.json()).toMatchObject({ items: [] });
    expect(originalRequests).toEqual([]);
    const link = page.getByRole('link', { name: 'Download original' });
    const href = await link.getAttribute('href');
    expect(href).toBe(
      `${originalPath}?expected_user_id=${account.user.id}&expected_revision_id=${revision}`,
    );
    const downloadReady = page.waitForEvent('download');
    await link.click();
    const download = await downloadReady;
    expect(await download.failure()).toBeNull();
    expect(download.suggestedFilename()).toBe('photo.png');
    const saved = await download.path();
    expect(saved).not.toBeNull();
    expect(await readFile(saved!)).toEqual(original);
    expect(originalRequests).toHaveLength(1);

    const membership = await api.get(memberPath);
    expect(membership.status()).toBe(200);
    const revoked = await api.put(memberPath, {
      headers: { ...headers, 'If-Match': membership.headers().etag! },
      data: { group_ids: [] },
    });
    expect(revoked.status()).toBe(200);
    expect((await page.request.get(href!)).status()).toBe(404);
    const sourceAfter = await stat(sourcePath, { bigint: true });
    expect(sourceAfter.mode).toBe(sourceBefore.mode);
    expect(sourceAfter.size).toBe(sourceBefore.size);
    expect(sourceAfter.mtimeNs).toBe(sourceBefore.mtimeNs);
    expect(await readFile(sourcePath)).toEqual(original);
  },
);
