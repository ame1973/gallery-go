import { onlineManager, QueryClient, QueryClientProvider } from '@tanstack/react-query';
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
  download_original: true,
};
const media: components['schemas']['Media'] = {
  id: 'media-b',
  album_id: 'album-b',
  kind: 'image',
  codec: 'png',
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
const album: components['schemas']['Album'] = {
  id: 'album-b',
  title: 'Album B',
  library_id: 'library-b',
  cover_media_id: media.id,
  media_count: 1,
  rating_avg: null,
  view_count: 0,
  indexed_at: '2026-11-26T00:00:00Z',
  version: 1,
};
const variant: components['schemas']['Variant'] = {
  id: 'variant-b',
  revision_id: 'revision-b',
  preset_key: 'thumb_320',
  preset_version: 1,
  mime: 'image/png',
  size_bytes: 64,
  sha256: 'a'.repeat(64),
  width: 1,
  height: 1,
  duration_ms: null,
  content_url: '/ignored',
};
const headers = {
  'X-GalleryGo-Expected-User': user.id,
  'X-GalleryGo-Expected-Scope': user.scope_id,
};
const routes = [
  { name: 'media details', endpoint: '/api/v2/media/media-b' },
  { name: 'variant list', endpoint: '/api/v2/media/media-b/variants' },
] as const;

type Mode = 'valid' | 'missing' | 'wrong-user' | 'wrong-scope' | 'invalid-context';

function response(body: unknown, mode: Mode) {
  if (mode === 'invalid-context') {
    return Response.json({ code: 'INVALID_READ_CONTEXT' }, { status: 400 });
  }
  const echo: Record<string, string> = mode === 'missing' ? {} : { ...headers };
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
    const selected = path === target ? (typeof mode === 'function' ? mode() : mode) : 'valid';
    if (path === '/api/v2/albums') {
      return response({ items: [album], has_more: false, next_cursor: null }, selected);
    }
    if (path === '/api/v2/media/media-b') return response(media, selected);
    if (path === '/api/v2/media/media-b/variants') {
      return response({ items: [variant], has_more: false, next_cursor: null }, selected);
    }
    return response({ items: [], has_more: false, next_cursor: null }, 'valid');
  });
  vi.stubGlobal('fetch', fetch);
  return fetch;
}

afterEach(() => {
  onlineManager.setOnline(true);
  vi.unstubAllGlobals();
});

describe('media read identity', () => {
  for (const { name, endpoint } of routes) {
    it.each(['missing', 'wrong-user', 'wrong-scope', 'invalid-context'] as const)(
      `hides media details when the ${name} echo is %s`,
      async (mode) => {
        installFetch(endpoint, mode);
        renderPage('/inspect/media-b/details');
        expect(await screen.findByRole('alert')).toBeInTheDocument();
        expect(screen.queryByText('Private photo B.png')).not.toBeInTheDocument();
        expect(screen.queryByRole('img', { name: /Private photo B/ })).not.toBeInTheDocument();
        expect(screen.queryByRole('link', { name: 'Download original' })).not.toBeInTheDocument();
      },
    );
    it(`sends the expected account and scope on ${name}`, async () => {
      const fetch = installFetch(endpoint, 'valid');
      renderPage('/inspect/media-b/details');
      expect(
        await screen.findByRole('img', { name: 'Preview of Private photo B.png' }),
      ).toBeInTheDocument();
      const request = fetch.mock.calls
        .map(([item]) => item)
        .find((item) => new URL(item.url).pathname === endpoint);
      expect(request?.headers.get('X-GalleryGo-Expected-User')).toBe(user.id);
      expect(request?.headers.get('X-GalleryGo-Expected-Scope')).toBe(user.scope_id);
    });

    it(`hides stale data and revalidates the session when ${name} changes scope`, async () => {
      let mode: Mode = 'valid';
      const fetch = installFetch(endpoint, () => mode);
      const client = renderPage('/inspect/media-b/details');
      expect(
        await screen.findByRole('img', { name: 'Preview of Private photo B.png' }),
      ).toBeInTheDocument();
      mode = 'wrong-scope';
      const key = name === 'media details' ? 'media' : 'preview';
      await act(async () => {
        await client.invalidateQueries({ queryKey: [user.id, user.scope_id, key, media.id] });
      });
      expect(await screen.findByRole('alert')).toBeInTheDocument();
      expect(screen.queryByText('Private photo B.png')).not.toBeInTheDocument();
      expect(screen.queryByRole('img', { name: /Private photo B/ })).not.toBeInTheDocument();
      expect(screen.queryByRole('link', { name: 'Download original' })).not.toBeInTheDocument();
      await waitFor(() =>
        expect(
          fetch.mock.calls.filter(([request]) => new URL(request.url).pathname.endsWith('/auth/me'))
            .length,
        ).toBeGreaterThan(1),
      );
      mode = 'valid';
      await userEvent.click(screen.getByRole('button', { name: 'Try again' }));
      expect(
        await screen.findByRole('img', { name: 'Preview of Private photo B.png' }),
      ).toBeInTheDocument();
    });
  }

  it.each(routes)(
    'hides stale preview while $name revalidates after reconnecting',
    async ({ name, endpoint }) => {
      let reconnecting = false;
      let finish!: (value: Response) => void;
      vi.stubGlobal(
        'fetch',
        vi.fn(async (request: Request) => {
          const path = new URL(request.url).pathname;
          if (path.endsWith('/auth/me')) return Response.json(user);
          if (reconnecting && path === endpoint) {
            return new Promise<Response>((resolve) => {
              finish = resolve;
            });
          }
          if (path === '/api/v2/media/media-b') return response(media, 'valid');
          return response({ items: [variant], has_more: false, next_cursor: null }, 'valid');
        }),
      );
      renderPage('/inspect/media-b/details');
      expect(
        await screen.findByRole('img', { name: 'Preview of Private photo B.png' }),
      ).toBeInTheDocument();
      reconnecting = true;
      act(() => {
        onlineManager.setOnline(false);
        onlineManager.setOnline(true);
      });
      await waitFor(() => expect(finish).toBeDefined());
      expect(screen.queryByRole('img', { name: /Private photo B/ })).not.toBeInTheDocument();
      await act(async () => finish(Response.json({ code: 'INTERNAL_ERROR' }, { status: 503 })));
      if (name === 'media details') {
        expect(await screen.findByRole('alert')).toBeInTheDocument();
        expect(screen.queryByText('Private photo B.png')).not.toBeInTheDocument();
      } else {
        expect(await screen.findByText('Preview not shown')).toBeInTheDocument();
        expect(screen.queryByRole('img', { name: /Private photo B/ })).not.toBeInTheDocument();
      }
    },
  );

  it('does not display an album card preview with an unverified response', async () => {
    const fetch = installFetch('/api/v2/media/media-b/variants', 'wrong-user');
    renderPage('/albums');
    const card = await screen.findByRole('link', { name: /Album B/ });
    expect(await screen.findByText('Preview not shown')).toBeInTheDocument();
    expect(card.querySelector('img')).toBeNull();
    await waitFor(() =>
      expect(
        fetch.mock.calls.filter(([request]) => new URL(request.url).pathname.endsWith('/auth/me'))
          .length,
      ).toBeGreaterThan(1),
    );
  });
});
