import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { App } from '../../app/App';
import type { components } from '../../lib/api/schema';

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
  id: 'album-b',
  title: 'Private album B',
  library_id: 'library-b',
  cover_media_id: null,
  media_count: 1,
  rating_avg: 5,
  view_count: 0,
  indexed_at: '2026-11-26T00:00:00Z',
  version: 1,
};
const media: components['schemas']['Media'] = {
  id: 'media-b',
  album_id: 'album-b',
  kind: 'image',
  codec: null,
  file_name: 'Private photo B.png',
  width: 1,
  height: 1,
  duration_ms: null,
  animated: false,
  frame_count: 1,
  loop_count: 1,
  orientation: null,
  capture_at: null,
  revision: 'revision-b',
  offline_capable: false,
};
const headers = {
  'X-GalleryGo-Expected-User': user.id,
  'X-GalleryGo-Expected-Scope': user.scope_id,
};
const emptyPage = { items: [], has_more: false, next_cursor: null };
const routes = [
  { name: 'album list', page: '/albums', endpoint: '/api/v2/albums' },
  { name: 'album details', page: '/albums/album-b', endpoint: '/api/v2/albums/album-b' },
  { name: 'album media', page: '/albums/album-b', endpoint: '/api/v2/albums/album-b/media' },
  { name: 'personal rating', page: '/albums/album-b', endpoint: '/api/v2/albums/album-b/rating' },
] as const;

type Mode = 'valid' | 'missing' | 'wrong-user' | 'wrong-scope';

function response(body: unknown, mode: Mode, extra: Record<string, string> = {}) {
  const echo: Record<string, string> = { ...extra, ...(mode === 'missing' ? {} : headers) };
  if (mode === 'wrong-user') echo['X-GalleryGo-Expected-User'] = 'account-b';
  if (mode === 'wrong-scope') echo['X-GalleryGo-Expected-Scope'] = 'scope-b';
  return Response.json(body, { headers: echo });
}

function renderPage(path: string) {
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

function installFetch(target: string, mode: Mode | (() => Mode)) {
  const fetch = vi.fn(async (request: Request) => {
    const path = new URL(request.url).pathname;
    if (path.endsWith('/auth/me')) return Response.json(user);
    if (path.endsWith('/auth/csrf')) return Response.json({ token: 'test-csrf' });
    const selected = path === target ? (typeof mode === 'function' ? mode() : mode) : 'valid';
    if (path === '/api/v2/albums') return response({ ...emptyPage, items: [album] }, selected);
    if (path === '/api/v2/albums/album-b') return response(album, selected);
    if (path === '/api/v2/albums/album-b/media') {
      return response({ ...emptyPage, items: [media] }, selected);
    }
    if (path === '/api/v2/albums/album-b/rating') {
      return response({ album_id: album.id, rating: 5, version: 1, updated_at: null }, selected, {
        ETag: '"rating-test"',
      });
    }
    return response(emptyPage, 'valid');
  });
  vi.stubGlobal('fetch', fetch);
  return fetch;
}

afterEach(() => vi.unstubAllGlobals());

describe('album read identity', () => {
  for (const { name, page, endpoint } of routes) {
    it.each(['missing', 'wrong-user', 'wrong-scope'] as const)(
      `does not display ${name} when its response echo is %s`,
      async (mode) => {
        installFetch(endpoint, mode);
        renderPage(page);
        expect(await screen.findByRole('alert')).toBeInTheDocument();
        expect(screen.queryByText('Private album B')).not.toBeInTheDocument();
        expect(screen.queryByText('Private photo B.png')).not.toBeInTheDocument();
        expect(screen.queryByRole('radio', { name: '5 stars' })).not.toBeInTheDocument();
      },
    );
    it(`sends the expected account and scope on ${name}`, async () => {
      const fetch = installFetch(endpoint, 'valid');
      renderPage(page);
      if (name === 'album list') {
        expect(await screen.findByRole('link', { name: /Private album B/ })).toBeInTheDocument();
      } else if (name === 'album media') {
        expect(await screen.findByText('Private photo B.png')).toBeInTheDocument();
      } else if (name === 'personal rating') {
        expect(await screen.findByRole('radio', { name: '5 stars' })).toBeChecked();
      } else {
        expect(await screen.findByRole('heading', { name: 'Private album B' })).toBeInTheDocument();
      }
      const request = fetch.mock.calls
        .map(([item]) => item)
        .find((item) => new URL(item.url).pathname === endpoint);
      expect(request?.headers.get('X-GalleryGo-Expected-User')).toBe(user.id);
      expect(request?.headers.get('X-GalleryGo-Expected-Scope')).toBe(user.scope_id);
    });

    it(`hides stale ${name} and revalidates the session after a changed echo`, async () => {
      let mode: Mode = 'valid';
      const fetch = installFetch(endpoint, () => mode);
      const client = renderPage(page);
      if (name === 'album list') {
        expect(await screen.findByRole('link', { name: /Private album B/ })).toBeInTheDocument();
      } else {
        expect(await screen.findByRole('radio', { name: '5 stars' })).toBeChecked();
        expect(await screen.findByText('Private photo B.png')).toBeInTheDocument();
      }
      mode = 'wrong-scope';
      const key =
        name === 'album list'
          ? 'albums'
          : name === 'album details'
            ? 'album'
            : name === 'album media'
              ? 'album-media'
              : 'album-rating';
      await act(async () => {
        await client.invalidateQueries({ queryKey: [user.id, user.scope_id, key] });
      });
      expect(await screen.findByRole('alert')).toBeInTheDocument();
      expect(screen.queryByText('Private album B')).not.toBeInTheDocument();
      expect(screen.queryByText('Private photo B.png')).not.toBeInTheDocument();
      expect(screen.queryByRole('radio', { name: '5 stars' })).not.toBeInTheDocument();
      await waitFor(() =>
        expect(
          fetch.mock.calls.filter(([request]) => new URL(request.url).pathname.endsWith('/auth/me'))
            .length,
        ).toBeGreaterThan(1),
      );
      mode = 'valid';
      await userEvent.click(screen.getByRole('button', { name: 'Try again' }));
      if (name === 'album list') {
        expect(await screen.findByRole('link', { name: /Private album B/ })).toBeInTheDocument();
      } else {
        expect(await screen.findByRole('radio', { name: '5 stars' })).toBeChecked();
        expect(await screen.findByText('Private photo B.png')).toBeInTheDocument();
      }
    });
  }

  it('hides the editor when the shared-average refresh has the wrong scope', async () => {
    let mode: Mode = 'valid';
    const fetch = installFetch('/api/v2/albums/album-b', () => mode);
    renderPage('/albums/album-b');
    expect(await screen.findByRole('radio', { name: '5 stars' })).toBeChecked();
    mode = 'wrong-scope';
    await userEvent.click(screen.getByRole('radio', { name: '4 stars' }));
    await userEvent.click(screen.getByRole('button', { name: 'Save rating' }));
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(screen.queryByText('Private album B')).not.toBeInTheDocument();
    expect(screen.queryByRole('radio', { name: '5 stars' })).not.toBeInTheDocument();
    const mutation = fetch.mock.calls.map(([request]) => request).find((r) => r.method === 'PUT');
    expect(mutation).toBeDefined();
    expect(mutation?.headers.get('X-GalleryGo-Expected-User')).toBeNull();
    expect(mutation?.headers.get('X-GalleryGo-Expected-Scope')).toBeNull();
  });
});
