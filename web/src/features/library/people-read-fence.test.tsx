import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
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
const person: components['schemas']['Person'] = {
  id: 'person-b',
  name: 'Private person B',
  source_id: 'source-b',
  album_count: 1,
  cover_media_id: null,
  version: 1,
};
const album: components['schemas']['Album'] = {
  id: 'album-b',
  title: 'Private album B',
  library_id: 'library-b',
  cover_media_id: null,
  media_count: 0,
  rating_avg: null,
  view_count: 0,
  indexed_at: '2026-11-26T00:00:00Z',
  version: 1,
};
const headers = {
  'X-GalleryGo-Expected-User': user.id,
  'X-GalleryGo-Expected-Scope': user.scope_id,
};
const routes = [
  { name: 'people list', page: '/people', endpoint: '/api/v2/people', value: 'Private person B' },
  {
    name: 'person details',
    page: '/people/person-b',
    endpoint: '/api/v2/people/person-b',
    value: 'Private person B',
  },
  {
    name: 'person albums',
    page: '/people/person-b',
    endpoint: '/api/v2/people/person-b/albums',
    value: 'Private album B',
  },
] as const;

type Mode = 'valid' | 'missing' | 'wrong-user' | 'wrong-scope';

function response(body: unknown, mode: Mode) {
  const echo: Record<string, string> = { ...headers };
  if (mode === 'missing') return Response.json(body);
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
}

function installFetch(target: string, mode: Mode) {
  const fetch = vi.fn(async (request: Request) => {
    const path = new URL(request.url).pathname;
    if (path.endsWith('/auth/me')) return Response.json(user);
    const data =
      path === '/api/v2/people'
        ? { items: [person], has_more: false, next_cursor: null }
        : path.endsWith('/albums')
          ? { items: [album], has_more: false, next_cursor: null }
          : { ...person, name: path === target ? person.name : 'Current person' };
    return response(data, path === target ? mode : 'valid');
  });
  vi.stubGlobal('fetch', fetch);
  return fetch;
}

afterEach(() => vi.unstubAllGlobals());

describe('people read identity', () => {
  for (const { name, page, endpoint, value } of routes) {
    it.each(['missing', 'wrong-user', 'wrong-scope'] as const)(
      `does not display ${name} when its response echo is %s`,
      async (mode) => {
        installFetch(endpoint, mode);
        renderPage(page);
        expect(await screen.findByRole('alert')).toBeInTheDocument();
        expect(screen.queryByText(value)).not.toBeInTheDocument();
      },
    );
    it(`sends the expected account and scope on ${name}`, async () => {
      const fetch = installFetch(endpoint, 'valid');
      renderPage(page);
      expect(await screen.findByText(value)).toBeInTheDocument();
      const request = fetch.mock.calls
        .map(([item]) => item)
        .find((item) => new URL(item.url).pathname === endpoint);
      expect(request?.headers.get('X-GalleryGo-Expected-User')).toBe(user.id);
      expect(request?.headers.get('X-GalleryGo-Expected-Scope')).toBe(user.scope_id);
    });
  }
});
