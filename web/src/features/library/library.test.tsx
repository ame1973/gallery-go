import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { App } from '../../app/App';
import type { components } from '../../lib/api/schema';
import { choosePreview } from './api';

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
  codec: null,
  file_name: 'photo.png',
  width: 1,
  height: 1,
  duration_ms: null,
  animated: false,
  frame_count: 1,
  loop_count: 1,
  orientation: null,
  capture_at: null,
  revision: 'revision',
  offline_capable: false,
};
const variant: components['schemas']['Variant'] = {
  id: 'variant-one',
  revision_id: 'revision',
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

function renderPage(path: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const view = render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <App />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { client, ...view };
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

afterEach(() => vi.unstubAllGlobals());

describe('album browsing', () => {
  it('requires the actual session before requesting gallery data', async () => {
    const fetch = installFetch(() => problem(401, 'UNAUTHENTICATED'));
    renderPage('/albums');
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
    expect(fetch.mock.calls.map(([request]) => new URL(request.url).pathname)).toEqual([
      '/api/v2/auth/me',
    ]);
  });

  it('renders an empty page in all three locales without exposing server diagnostics', async () => {
    installFetch((url) =>
      url.pathname.endsWith('/auth/me')
        ? Response.json(user)
        : Response.json({ items: [], next_cursor: null, has_more: false }),
    );
    renderPage('/albums');
    expect(await screen.findByText('No albums are available on this page.')).toBeInTheDocument();
    expect(
      screen.getByText('Ask your administrator to check the sources and access permissions.'),
    ).toBeInTheDocument();
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Language' }), 'zh-TW');
    expect(screen.getByRole('heading', { name: '相簿' })).toBeInTheDocument();
    expect(screen.getByText('此頁面目前沒有可用的相簿。')).toBeInTheDocument();
    await userEvent.selectOptions(screen.getByRole('combobox', { name: '語言' }), 'zh-CN');
    expect(screen.getByRole('heading', { name: '相册' })).toBeInTheDocument();
    expect(screen.queryByText('private path')).not.toBeInTheDocument();
  });

  it('uses bounded scoped pages, supported sorts, and only same-origin preview paths', async () => {
    const calls: URL[] = [];
    installFetch((url) => {
      calls.push(url);
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname.endsWith('/variants'))
        return Response.json({ items: [variant], next_cursor: null, has_more: false });
      if (url.pathname.endsWith('/albums')) {
        return Response.json(
          url.searchParams.has('cursor')
            ? {
                items: [{ ...album, id: 'album-two', title: 'Second album', cover_media_id: null }],
                next_cursor: null,
                has_more: false,
              }
            : { items: [album], next_cursor: 'opaque-page', has_more: true },
        );
      }
      return problem(404, 'NOT_FOUND');
    });
    renderPage('/albums');
    const card = await screen.findByRole('link', { name: /A family trip/ });
    await waitFor(() =>
      expect(card.querySelector('img')).toHaveAttribute(
        'src',
        '/api/v2/media/media-one/variants/variant-one/content',
      ),
    );
    expect(calls.some((url) => url.origin === 'https://untrusted.example')).toBe(false);
    expect(calls.find((url) => url.pathname.endsWith('/albums'))?.searchParams.get('limit')).toBe(
      '30',
    );
    await userEvent.click(screen.getByRole('link', { name: 'Next page' }));
    expect(await screen.findByRole('link', { name: /Second album/ })).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /A family trip/ })).not.toBeInTheDocument();
    expect(calls.find((url) => url.searchParams.get('cursor') === 'opaque-page')).toBeDefined();
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Sort albums' }), 'popular');
    expect(await screen.findByRole('link', { name: /A family trip/ })).toBeInTheDocument();
    const last = calls.filter((url) => url.pathname.endsWith('/albums')).at(-1)!;
    expect(last.searchParams.get('sort')).toBe('popular');
    expect(last.searchParams.has('cursor')).toBe(false);
  });

  it('hides previously authorized album information if media access is revoked', async () => {
    let finishMedia!: (response: Response) => void;
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname.endsWith('/albums/album-one')) return Response.json(album);
      if (url.pathname.endsWith('/albums/album-one/media'))
        return new Promise((resolve) => {
          finishMedia = resolve;
        });
      return problem(404, 'NOT_FOUND');
    });
    renderPage('/albums/album-one');
    expect(await screen.findByRole('heading', { name: 'A family trip' })).toBeInTheDocument();
    finishMedia(problem(404, 'NOT_FOUND'));
    expect(
      await screen.findByText('This album is unavailable or you no longer have access.'),
    ).toBeInTheDocument();
    expect(screen.queryByText('A family trip')).not.toBeInTheDocument();
    expect(screen.queryByText('private path')).not.toBeInTheDocument();
  });

  it('renders authorized media and makes invalid continuation recoverable', async () => {
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname.endsWith('/albums/album-one')) return Response.json(album);
      if (url.pathname.endsWith('/albums/album-one/media'))
        return Response.json({ items: [media], next_cursor: null, has_more: false });
      if (url.pathname.endsWith('/variants'))
        return Response.json({ items: [variant], next_cursor: null, has_more: false });
      return problem(404, 'NOT_FOUND');
    });
    renderPage('/albums/album-one');
    expect(await screen.findByText('photo.png')).toBeInTheDocument();
    expect(screen.getByText('Image')).toBeInTheDocument();
    expect(
      screen.getByText('Offline downloads and the full media viewer are still under development.'),
    ).toBeInTheDocument();
  });

  it.each([
    '/albums?cursor=',
    '/albums?sort=',
    '/albums?sort=unknown',
    '/albums?cursor=one&cursor=two',
    '/albums?favorite=true',
  ])('rejects unsupported or ambiguous page parameters: %s', async (path) => {
    const fetch = installFetch((url) =>
      url.pathname.endsWith('/auth/me') ? Response.json(user) : problem(400, 'INVALID_CURSOR'),
    );
    renderPage(path);
    expect(
      await screen.findByText(
        'This page link is invalid or expired, or access has changed. Return to the first page.',
      ),
    ).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'First page' })).toHaveAttribute('href', '/albums');
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1));
  });

  it('does not silently ignore an unsupported media filter', async () => {
    const fetch = installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname.endsWith('/albums/album-one')) return Response.json(album);
      return problem(404, 'NOT_FOUND');
    });
    renderPage('/albums/album-one?kind=video');
    expect(
      await screen.findByText(
        'This page link is invalid or expired, or access has changed. Return to the first page.',
      ),
    ).toBeInTheDocument();
    expect(
      fetch.mock.calls.some(([request]) => new URL(request.url).pathname.endsWith('/media')),
    ).toBe(false);
  });

  it.each([
    { id: 'account-b', scope_id: 'scope-b' },
    { id: 'account-a', scope_id: 'scope-b' },
  ])('drops prior data when the account or scope changes: $id/$scope_id', async (next) => {
    let current = user;
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(current);
      if (url.pathname.endsWith('/variants'))
        return Response.json({ items: [variant], next_cursor: null, has_more: false });
      if (url.pathname.endsWith('/albums')) {
        const item =
          current.scope_id === user.scope_id
            ? album
            : { ...album, id: 'second-account', title: 'Another account', cover_media_id: null };
        return Response.json({ items: [item], next_cursor: null, has_more: false });
      }
      return problem(404, 'NOT_FOUND');
    });
    const { client } = renderPage('/albums');
    expect(await screen.findByRole('link', { name: /A family trip/ })).toBeInTheDocument();
    current = { ...user, ...next };
    act(() => client.setQueryData(['auth', 'me'], current));
    expect(await screen.findByRole('link', { name: /Another account/ })).toBeInTheDocument();
    expect(screen.queryByText('A family trip')).not.toBeInTheDocument();
  });

  it('hides stale gallery results and rechecks a revoked session', async () => {
    let revoked = false;
    installFetch((url) => {
      if (revoked) return problem(401, 'UNAUTHENTICATED');
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname.endsWith('/variants'))
        return Response.json({ items: [variant], next_cursor: null, has_more: false });
      return Response.json({ items: [album], next_cursor: null, has_more: false });
    });
    const { client } = renderPage('/albums');
    expect(await screen.findByRole('link', { name: /A family trip/ })).toBeInTheDocument();
    revoked = true;
    await act(async () => {
      await client.invalidateQueries({ queryKey: [user.id, user.scope_id, 'albums'] });
    });
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
    expect(screen.queryByText('A family trip')).not.toBeInTheDocument();
  });

  it('loads preview metadata only as a card approaches the viewport', async () => {
    let reveal: (() => void) | undefined;
    class MockObserver {
      constructor(callback: IntersectionObserverCallback) {
        reveal = () =>
          callback(
            [{ isIntersecting: true } as IntersectionObserverEntry],
            this as unknown as IntersectionObserver,
          );
      }
      observe() {}
      disconnect() {}
    }
    vi.stubGlobal('IntersectionObserver', MockObserver);
    const fetch = installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname.endsWith('/variants'))
        return Response.json({ items: [variant], next_cursor: null, has_more: false });
      return Response.json({ items: [album], next_cursor: null, has_more: false });
    });
    renderPage('/albums');
    const card = await screen.findByRole('link', { name: /A family trip/ });
    await waitFor(() => expect(reveal).toBeDefined());
    expect(
      fetch.mock.calls.some(([request]) => new URL(request.url).pathname.endsWith('/variants')),
    ).toBe(false);
    act(() => reveal!());
    await waitFor(() =>
      expect(card.querySelector('img')).toHaveAttribute(
        'src',
        '/api/v2/media/media-one/variants/variant-one/content',
      ),
    );
    expect(fetch.mock.calls.every(([request]) => !request.url.includes('/original'))).toBe(true);
  });

  it('recovers from a server-rejected scoped cursor', async () => {
    installFetch((url) =>
      url.pathname.endsWith('/auth/me') ? Response.json(user) : problem(400, 'INVALID_CURSOR'),
    );
    renderPage('/albums?cursor=old');
    expect(
      await screen.findByText(
        'This page link is invalid or expired, or access has changed. Return to the first page.',
      ),
    ).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'First page' })).toHaveAttribute('href', '/albums');
  });

  it('never chooses oversized or non-image variant content as a thumbnail', () => {
    expect(
      choosePreview([
        { ...variant, mime: 'video/mp4' },
        { ...variant, width: 10000 },
        { ...variant, id: '..' },
        { ...variant, id: '../private' },
      ]),
    ).toBeUndefined();
    expect(choosePreview([{ ...variant, preset_key: 'other' }, variant])).toEqual(variant);
  });
});
