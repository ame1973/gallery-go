import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { App } from '../../app/App';
import type { components } from '../../lib/api/schema';
import { safeID } from './api';

const user: components['schemas']['User'] = {
  id: 'account-a',
  username: 'reader',
  role: 'user',
  active: true,
  scope_id: 'scope-a',
  version: 1,
  download_original: false,
};
const album: components['schemas']['Album'] = {
  id: 'album-one',
  title: 'A family trip',
  library_id: 'library',
  cover_media_id: 'media-one',
  media_count: 1,
  rating_avg: null,
  view_count: 0,
  indexed_at: '2026-11-01T00:00:00Z',
  version: 1,
};
const media: components['schemas']['Media'] = {
  id: 'media-one',
  album_id: 'album-one',
  kind: 'image',
  codec: 'png',
  file_name: 'photo.png',
  width: 4032,
  height: 3024,
  duration_ms: 1250,
  animated: true,
  frame_count: 3,
  loop_count: 0,
  orientation: 6,
  capture_at: '2026-01-02T03:04:05.123Z',
  revision: 'revision-one',
  offline_capable: false,
};
const variant: components['schemas']['Variant'] = {
  id: 'variant-one',
  revision_id: 'revision-one',
  preset_key: 'thumb_320',
  preset_version: 1,
  mime: 'image/png',
  size_bytes: 64,
  sha256: 'a'.repeat(64),
  width: 1,
  height: 1,
  duration_ms: null,
  content_url: 'https://untrusted.example/private',
};
const emptyPage = { items: [], next_cursor: null, has_more: false };

function renderPage(path: string | { pathname: string; search?: string; state: unknown }) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <App />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return client;
}

function problem(status: number, code: string) {
  return Response.json(
    { code, status, type: 'urn:test', title: 'Test', request_id: 'id', detail: 'private path' },
    { status },
  );
}

function installFetch(handler: (url: URL) => Response | Promise<Response>) {
  const fetch = vi.fn(async (request: Request) => handler(new URL(request.url)));
  vi.stubGlobal('fetch', fetch);
  return fetch;
}

function value(label: string) {
  return screen.getByText(label).nextElementSibling;
}

afterEach(() => vi.unstubAllGlobals());

describe('protected media detail', () => {
  it('requires the real session before requesting a media item', async () => {
    const fetch = installFetch(() => problem(401, 'UNAUTHENTICATED'));
    renderPage('/inspect/media-one/details');
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
    expect(fetch.mock.calls.map(([request]) => new URL(request.url).pathname)).toEqual([
      '/api/v2/auth/me',
    ]);
  });

  it('shows actual media details and only a current same-origin ready preview', async () => {
    const calls: URL[] = [];
    installFetch((url) => {
      calls.push(url);
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname.endsWith('/variants'))
        return Response.json({ ...emptyPage, items: [variant] });
      if (url.pathname === '/api/v2/media/media-one') return Response.json(media);
      return problem(404, 'NOT_FOUND');
    });
    renderPage('/inspect/media-one/details');
    expect(await screen.findByRole('heading', { name: 'photo.png' })).toBeInTheDocument();
    expect(screen.getByRole('main')).toHaveFocus();
    expect(value('Encoded dimensions')).toHaveTextContent('4,032 × 3,024 px');
    expect(value('Codec')).toHaveTextContent('png');
    expect(value('Orientation code')).toHaveTextContent('6');
    expect(value('Animation')).toHaveTextContent('Yes');
    expect(value('Frames')).toHaveTextContent('3');
    expect(value('Playthroughs')).toHaveTextContent('Infinite');
    expect(value('Cycle duration')).toHaveTextContent('1.25 seconds');
    const captured = value('Captured');
    expect(captured?.querySelector('time')).toHaveAttribute('datetime', media.capture_at);
    expect(captured).toHaveTextContent('03:04:05.123');
    expect(captured).toHaveTextContent('UTC');
    const image = await screen.findByRole('img', { name: 'Preview of photo.png' });
    expect(image).toHaveAttribute('src', '/api/v2/media/media-one/variants/variant-one/content');
    expect(screen.getByRole('link', { name: 'Back to album' })).toHaveAttribute(
      'href',
      '/albums/album-one',
    );
    expect(calls.every((url) => url.origin !== 'https://untrusted.example')).toBe(true);
    expect(calls.every((url) => !url.pathname.endsWith('/original'))).toBe(true);
  });

  it('opens an album item and preserves media, person and people page context on return', async () => {
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname === '/api/v2/albums/album-one') return Response.json(album);
      if (url.pathname === '/api/v2/albums/album-one/rating')
        return Response.json(
          { album_id: 'album-one', rating: null, updated_at: null, version: 1 },
          { headers: { ETag: '"rating-test"' } },
        );
      if (url.pathname === '/api/v2/albums/album-one/media')
        return Response.json({ ...emptyPage, items: [media] });
      if (url.pathname === '/api/v2/media/media-one') return Response.json(media);
      if (url.pathname.endsWith('/variants')) return Response.json(emptyPage);
      if (url.pathname === '/api/v2/people/person-one')
        return Response.json({
          id: 'person-one',
          name: 'Person one',
          source_id: 'source-one',
          album_count: 0,
          cover_media_id: null,
          version: 1,
        });
      if (url.pathname === '/api/v2/people/person-one/albums') return Response.json(emptyPage);
      return problem(404, 'NOT_FOUND');
    });
    renderPage({
      pathname: '/albums/album-one',
      search: '?cursor=media-cursor',
      state: {
        from: '/people/person-one?cursor=album-cursor',
        peopleFrom: '/people?cursor=people-cursor',
      },
    });
    await userEvent.click(await screen.findByRole('link', { name: 'View photo.png details' }));
    expect(await screen.findByRole('heading', { name: 'photo.png' })).toBeInTheDocument();
    const back = screen.getByRole('link', { name: 'Back to album' });
    expect(back).toHaveAttribute('href', '/albums/album-one?cursor=media-cursor');
    await userEvent.click(back);
    await userEvent.click(await screen.findByRole('link', { name: 'Back to person' }));
    expect(await screen.findByRole('heading', { name: 'Person one' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to people' })).toHaveAttribute(
      'href',
      '/people?cursor=people-cursor',
    );
  });

  it.each([
    { ...media, id: '\ud800' },
    { ...media, album_id: 'another-album' },
  ])('rejects unsafe or unrelated media in an album result', async (item) => {
    const fetch = installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname === '/api/v2/albums/album-one') return Response.json(album);
      if (url.pathname.endsWith('/rating'))
        return Response.json(
          { album_id: 'album-one', rating: null, updated_at: null, version: 1 },
          { headers: { ETag: '"rating-test"' } },
        );
      if (url.pathname.endsWith('/media')) return Response.json({ ...emptyPage, items: [item] });
      return problem(404, 'NOT_FOUND');
    });
    renderPage('/albums/album-one');
    expect(
      await screen.findByText('Could not load this page. Check your connection and try again.'),
    ).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'View photo.png details' })).not.toBeInTheDocument();
    expect(fetch.mock.calls.some(([request]) => request.url.includes('/variants'))).toBe(false);
  });

  it('shows unknown metadata honestly and translates the detail labels', async () => {
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname.endsWith('/variants')) return Response.json(emptyPage);
      return Response.json({
        ...media,
        codec: null,
        width: null,
        height: null,
        duration_ms: null,
        animated: null,
        frame_count: null,
        loop_count: null,
        orientation: null,
        capture_at: null,
      });
    });
    renderPage('/inspect/media-one/details');
    expect(await screen.findByRole('heading', { name: 'photo.png' })).toBeInTheDocument();
    for (const label of [
      'Encoded dimensions',
      'Codec',
      'Captured',
      'Orientation code',
      'Animation',
      'Frames',
      'Playthroughs',
      'Cycle duration',
    ]) {
      expect(value(label)).toHaveTextContent('Unknown');
    }
    expect(await screen.findByText('Preview not shown')).toBeInTheDocument();
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Language' }), 'zh-TW');
    expect(screen.getByRole('heading', { name: '媒體詳細資料' })).toBeInTheDocument();
    expect(value('編碼尺寸')).toHaveTextContent('未知');
    await userEvent.selectOptions(screen.getByRole('combobox', { name: '語言' }), 'zh-CN');
    expect(screen.getByRole('heading', { name: '媒体详情' })).toBeInTheDocument();
    expect(value('编码尺寸')).toHaveTextContent('未知');
  });

  it('reports video duration separately and does not fabricate animation information', async () => {
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname.endsWith('/variants')) return Response.json(emptyPage);
      return Response.json({
        ...media,
        kind: 'video',
        codec: 'h264',
        duration_ms: 62001,
        animated: null,
        frame_count: null,
        loop_count: null,
      });
    });
    renderPage('/inspect/media-one/details');
    expect(await screen.findByRole('heading', { name: 'photo.png' })).toBeInTheDocument();
    expect(value('Duration')).toHaveTextContent('62.001 seconds');
    expect(value('Codec')).toHaveTextContent('h264');
    expect(screen.queryByText('Animation')).not.toBeInTheDocument();
    expect(screen.queryByText('Playthroughs')).not.toBeInTheDocument();
  });

  it('does not display a variant from a different revision or request an original', async () => {
    const fetch = installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname.endsWith('/variants'))
        return Response.json({
          ...emptyPage,
          items: [{ ...variant, revision_id: 'revision-two' }],
        });
      return Response.json(media);
    });
    renderPage('/inspect/media-one/details');
    expect(await screen.findByRole('heading', { name: 'photo.png' })).toBeInTheDocument();
    expect(await screen.findByText('Preview not shown')).toBeInTheDocument();
    expect(screen.queryByRole('img', { name: 'Preview of photo.png' })).not.toBeInTheDocument();
    expect(fetch.mock.calls.every(([request]) => !request.url.includes('/original'))).toBe(true);
  });

  it('replaces the preview when a fresh metadata read reports another revision', async () => {
    let revision = 'revision-one';
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname.endsWith('/variants'))
        return Response.json({ ...emptyPage, items: [variant] });
      return Response.json({ ...media, revision });
    });
    const client = renderPage('/inspect/media-one/details');
    expect(await screen.findByRole('img', { name: 'Preview of photo.png' })).toHaveAttribute(
      'src',
      '/api/v2/media/media-one/variants/variant-one/content',
    );
    revision = 'revision-two';
    await act(async () => {
      await client.invalidateQueries({ queryKey: [user.id, user.scope_id, 'media', media.id] });
    });
    expect(await screen.findByText('Preview not shown')).toBeInTheDocument();
    expect(screen.queryByRole('img', { name: 'Preview of photo.png' })).not.toBeInTheDocument();
  });

  it.each([
    { ...media, id: 'different-media' },
    { ...media, album_id: '../private' },
  ])(
    'rejects inconsistent media identity before rendering or requesting variants',
    async (item) => {
      const fetch = installFetch((url) =>
        url.pathname.endsWith('/auth/me') ? Response.json(user) : Response.json(item),
      );
      renderPage('/inspect/media-one/details');
      expect(
        await screen.findByText('Could not load this page. Check your connection and try again.'),
      ).toBeInTheDocument();
      expect(screen.queryByText('photo.png')).not.toBeInTheDocument();
      expect(fetch.mock.calls.some(([request]) => request.url.includes('/variants'))).toBe(false);
    },
  );

  it('does not request a preview while the current revision is unknown', async () => {
    const fetch = installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      return Response.json({ ...media, revision: null });
    });
    renderPage('/inspect/media-one/details');
    expect(await screen.findByRole('heading', { name: 'photo.png' })).toBeInTheDocument();
    expect(screen.getByText('Preview not shown')).toBeInTheDocument();
    expect(fetch.mock.calls.some(([request]) => request.url.includes('/variants'))).toBe(false);
  });

  it.each([
    'https://untrusted.example/albums/album-one',
    '//untrusted.example/albums/album-one',
    '/albums/..',
    '/albums/%2e%2e',
    '/albums/a%2Fb',
    '/albums/album-one?sort=name',
    '/albums/album-one?cursor=',
    '/albums/album-one?cursor=a&cursor=b',
    `/albums/album-one?cursor=${'x'.repeat(513)}`,
    '/albums/album-one#fragment',
    '/albums/different-album',
  ])('rejects an unsafe or unrelated album return path: %s', async (from) => {
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname.endsWith('/variants')) return Response.json(emptyPage);
      return Response.json(media);
    });
    renderPage({ pathname: '/inspect/media-one/details', state: { from } });
    expect(await screen.findByRole('heading', { name: 'photo.png' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to album' })).toHaveAttribute(
      'href',
      '/albums/album-one',
    );
  });

  it.each(['/inspect/media-one/details?cursor=anything', '/inspect/media-one/details?unknown=1'])(
    'rejects unsupported media-detail parameters before requesting metadata: %s',
    async (path) => {
      const fetch = installFetch((url) =>
        url.pathname.endsWith('/auth/me') ? Response.json(user) : problem(404, 'NOT_FOUND'),
      );
      renderPage(path);
      expect(
        await screen.findByText(
          'This page link is invalid or expired, or access has changed. Return to the first page.',
        ),
      ).toBeInTheDocument();
      expect(screen.getByRole('link', { name: 'First page' })).toHaveAttribute(
        'href',
        '/inspect/media-one/details',
      );
      await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1));
    },
  );

  it('rejects malformed identifiers before they can reach URL encoding', () => {
    expect(safeID('\ud800')).toBe(false);
    expect(safeID('\udfff')).toBe(false);
    expect(safeID('valid-識別碼')).toBe(true);
  });

  it('preserves a validated album link when media access is revoked', async () => {
    installFetch((url) =>
      url.pathname.endsWith('/auth/me') ? Response.json(user) : problem(404, 'NOT_FOUND'),
    );
    renderPage({
      pathname: '/inspect/media-one/details',
      state: { from: '/albums/album-one?cursor=media-cursor' },
    });
    expect(
      await screen.findByText('This media item is unavailable or you no longer have access.'),
    ).toBeInTheDocument();
    const links = screen.getAllByRole('link', { name: 'Back to album' });
    expect(links).toHaveLength(2);
    expect(links[1]).toHaveAttribute('href', '/albums/album-one?cursor=media-cursor');
    expect(screen.queryByText('private path')).not.toBeInTheDocument();
  });

  it('rechecks media after preview content fails and hides newly revoked details', async () => {
    let revoked = false;
    let mediaReads = 0;
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname.endsWith('/variants'))
        return Response.json({ ...emptyPage, items: [variant] });
      mediaReads += 1;
      return revoked ? problem(404, 'NOT_FOUND') : Response.json(media);
    });
    renderPage('/inspect/media-one/details');
    const image = await screen.findByRole('img', { name: 'Preview of photo.png' });
    revoked = true;
    fireEvent.error(image);
    expect(
      await screen.findByText('This media item is unavailable or you no longer have access.'),
    ).toBeInTheDocument();
    expect(screen.queryByText('photo.png')).not.toBeInTheDocument();
    expect(screen.queryByText('Encoded dimensions')).not.toBeInTheDocument();
    expect(mediaReads).toBe(2);
  });

  it('bounds corrupt-preview rechecks and permits another check after a successful load', async () => {
    let finish!: (response: Response) => void;
    let mediaReads = 0;
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname.endsWith('/variants'))
        return Response.json({ ...emptyPage, items: [variant] });
      mediaReads += 1;
      return mediaReads === 2 || mediaReads === 3
        ? new Promise((resolve) => {
            finish = resolve;
          })
        : Response.json(media);
    });
    const client = renderPage('/inspect/media-one/details');
    fireEvent.error(await screen.findByRole('img', { name: 'Preview of photo.png' }));
    await waitFor(() => expect(mediaReads).toBe(2));
    expect(await screen.findByText('Loading media details…')).toBeInTheDocument();
    await act(async () => finish(Response.json(media)));
    fireEvent.error(await screen.findByRole('img', { name: 'Preview of photo.png' }));
    expect(await screen.findByText('Preview not shown')).toBeInTheDocument();
    expect(mediaReads).toBe(2);

    act(() => {
      void client.invalidateQueries({ queryKey: [user.id, user.scope_id, 'media', media.id] });
    });
    await waitFor(() => expect(mediaReads).toBe(3));
    expect(await screen.findByText('Loading media details…')).toBeInTheDocument();
    await act(async () => finish(Response.json(media)));
    const image = await screen.findByRole('img', { name: 'Preview of photo.png' });
    fireEvent.load(image);
    fireEvent.error(image);
    await waitFor(() => expect(mediaReads).toBe(4));
  });

  it('hides details when the preview endpoint reports lost access', async () => {
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname.endsWith('/variants')) return problem(404, 'NOT_FOUND');
      return Response.json(media);
    });
    renderPage('/inspect/media-one/details');
    expect(
      await screen.findByText('This media item is unavailable or you no longer have access.'),
    ).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'photo.png' })).not.toBeInTheDocument();
    expect(screen.queryByText('Encoded dimensions')).not.toBeInTheDocument();
    for (const link of screen.getAllByRole('link', { name: 'Back to albums' })) {
      expect(link).toHaveAttribute('href', '/albums');
    }
  });

  it('hides previous metadata during refresh and after a transient failure, then recovers', async () => {
    let finish!: (response: Response) => void;
    let mode: 'ready' | 'pending' = 'ready';
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname.endsWith('/variants')) return Response.json(emptyPage);
      return mode === 'pending'
        ? new Promise((resolve) => {
            finish = resolve;
          })
        : Response.json(media);
    });
    const client = renderPage('/inspect/media-one/details');
    expect(await screen.findByRole('heading', { name: 'photo.png' })).toBeInTheDocument();
    mode = 'pending';
    act(() => {
      void client.invalidateQueries({ queryKey: [user.id, user.scope_id, 'media', media.id] });
    });
    await waitFor(() => expect(finish).toBeDefined());
    expect(screen.queryByRole('heading', { name: 'photo.png' })).not.toBeInTheDocument();
    expect(screen.queryByText('Encoded dimensions')).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to albums' })).toHaveAttribute('href', '/albums');
    await act(async () => finish(problem(503, 'INTERNAL_ERROR')));
    expect(
      await screen.findByText('Could not load this page. Check your connection and try again.'),
    ).toBeInTheDocument();
    expect(screen.queryByText('photo.png')).not.toBeInTheDocument();
    expect(screen.queryByText('private path')).not.toBeInTheDocument();
    mode = 'ready';
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }));
    expect(screen.getByRole('main')).toHaveFocus();
    expect(await screen.findByRole('heading', { name: 'photo.png' })).toBeInTheDocument();
  });

  it.each([
    { id: 'account-b', scope_id: 'scope-a' },
    { id: 'account-a', scope_id: 'scope-b' },
  ])('separates metadata by account and scope: $id/$scope_id', async (next) => {
    let current = user;
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(current);
      if (url.pathname.endsWith('/variants')) return Response.json(emptyPage);
      return Response.json(current === user ? media : { ...media, file_name: 'new-account.png' });
    });
    const client = renderPage('/inspect/media-one/details');
    expect(await screen.findByRole('heading', { name: 'photo.png' })).toBeInTheDocument();
    current = { ...user, ...next };
    act(() => client.setQueryData(['auth', 'me'], current));
    expect(await screen.findByRole('heading', { name: 'new-account.png' })).toBeInTheDocument();
    expect(screen.queryByText('photo.png')).not.toBeInTheDocument();
  });

  it('does not retain a preview denial after the account scope changes and returns', async () => {
    let current = user;
    let denied = true;
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(current);
      if (url.pathname.endsWith('/variants'))
        return denied ? problem(404, 'NOT_FOUND') : Response.json(emptyPage);
      return Response.json(
        current.scope_id === 'scope-b' ? { ...media, file_name: 'scope-b.png' } : media,
      );
    });
    const client = renderPage('/inspect/media-one/details');
    expect(
      await screen.findByText('This media item is unavailable or you no longer have access.'),
    ).toBeInTheDocument();
    denied = false;
    current = { ...user, scope_id: 'scope-b' };
    act(() => client.setQueryData(['auth', 'me'], current));
    expect(await screen.findByRole('heading', { name: 'scope-b.png' })).toBeInTheDocument();
    current = user;
    act(() => client.setQueryData(['auth', 'me'], current));
    await waitFor(() => {
      expect(screen.getByRole('heading', { name: 'photo.png' })).toBeInTheDocument();
      expect(
        screen.queryByText('This media item is unavailable or you no longer have access.'),
      ).not.toBeInTheDocument();
    });
  });

  it('revalidates the session when preview access returns 401', async () => {
    let revoked = false;
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me'))
        return revoked ? problem(401, 'UNAUTHENTICATED') : Response.json(user);
      if (url.pathname.endsWith('/variants')) {
        revoked = true;
        return problem(401, 'UNAUTHENTICATED');
      }
      return Response.json(media);
    });
    renderPage('/inspect/media-one/details');
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
    expect(screen.queryByText('photo.png')).not.toBeInTheDocument();
  });

  it('revalidates the session when metadata access returns 401', async () => {
    let revoked = false;
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me'))
        return revoked ? problem(401, 'UNAUTHENTICATED') : Response.json(user);
      if (url.pathname.endsWith('/variants')) return Response.json(emptyPage);
      if (revoked) return problem(401, 'UNAUTHENTICATED');
      return Response.json(media);
    });
    const client = renderPage('/inspect/media-one/details');
    expect(await screen.findByRole('heading', { name: 'photo.png' })).toBeInTheDocument();
    revoked = true;
    await act(async () => {
      await client.invalidateQueries({ queryKey: [user.id, user.scope_id, 'media', media.id] });
    });
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
    expect(screen.queryByText('photo.png')).not.toBeInTheDocument();
  });
});
