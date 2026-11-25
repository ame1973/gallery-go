import { onlineManager, QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from '../../app/App';
import type { components } from '../../lib/api/schema';

type User = components['schemas']['User'];
type Source = components['schemas']['Source'];
const admin: User = {
  id: 'admin-a',
  username: 'owner',
  role: 'admin',
  active: true,
  scope_id: 'scope-a',
  version: 1,
  download_original: false,
};
const source: Source = {
  id: 'b-source',
  library_id: 'library-b',
  root_name: 'B private source',
  mapping: { album_depth: 2, person_depth: 1 },
  active: true,
  configured: true,
  state: 'healthy',
  minimum_files: 0,
  poll_interval_seconds: 60,
  full_scan_interval_seconds: 3600,
  scan_generation: 1,
  version: 1,
  config_version: 1,
  last_checked_at: null,
  last_healthy_at: null,
  last_full_scan_at: null,
  last_error_code: null,
  created_at: '2026-11-26T00:00:00Z',
  updated_at: '2026-11-26T00:00:00Z',
};
const pages = [
  { page: 'list', route: '/admin/sources', api: '/api/v2/admin/sources' },
  {
    page: 'detail',
    route: '/admin/sources/b-source/details',
    api: '/api/v2/admin/sources/b-source',
  },
] as const;
const userHeader = 'X-GalleryGo-Expected-User';
const scopeHeader = 'X-GalleryGo-Expected-Scope';
type Echo = 'valid' | 'missing' | 'wrong-user' | 'wrong-scope';

function response(data: unknown, echo: Echo, actor = admin) {
  if (echo === 'missing') return Response.json(data);
  return Response.json(data, {
    headers: {
      [userHeader]: echo === 'wrong-user' ? 'admin-b' : actor.id,
      [scopeHeader]: echo === 'wrong-scope' ? 'scope-b' : actor.scope_id,
    },
  });
}

function renderPage(route: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[route]}>
        <App />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return client;
}

function installHandler(handler: (url: URL, request: Request) => Response | Promise<Response>) {
  const fetch = vi.fn(async (request: Request) => handler(new URL(request.url), request));
  vi.stubGlobal('fetch', fetch);
  return fetch;
}

function installFetch(echo: Echo) {
  return installHandler((url) => {
    if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
    return response(
      url.pathname === '/api/v2/admin/sources'
        ? { items: [source], next_cursor: null, has_more: false }
        : source,
      echo,
    );
  });
}

beforeEach(() => localStorage.clear());
afterEach(() => {
  onlineManager.setOnline(true);
  vi.unstubAllGlobals();
});

describe('source browser read identity', () => {
  for (const { page, route, api } of pages) {
    it.each(['missing', 'wrong-user', 'wrong-scope'] as const)(
      `does not display ${page} data when its response echo is %s`,
      async (echo) => {
        installFetch(echo);
        renderPage(route);
        expect(await screen.findByRole('alert')).toBeInTheDocument();
        expect(screen.queryByText('B private source')).not.toBeInTheDocument();
      },
    );
    it(`sends the expected actor on the ${page} request`, async () => {
      const fetch = installFetch('valid');
      renderPage(route);
      expect(await screen.findByText('B private source')).toBeInTheDocument();
      const request = fetch.mock.calls
        .map(([item]) => item)
        .find((item) => new URL(item.url).pathname === api);
      expect(request?.headers.get(userHeader)).toBe(admin.id);
      expect(request?.headers.get(scopeHeader)).toBe(admin.scope_id);
    });

    it.each([
      { reason: 'inactive administrator', actor: { ...admin, active: false } },
      { reason: 'invalid scope', actor: { ...admin, scope_id: 'invalid scope' } },
    ])(`does not request ${page} for $reason`, async ({ actor }) => {
      const fetch = installHandler((url) =>
        url.pathname.endsWith('/auth/me') ? Response.json(actor) : response(source, 'valid'),
      );
      renderPage(route);
      expect(await screen.findByRole('alert')).toBeInTheDocument();
      expect(fetch.mock.calls.map(([request]) => new URL(request.url).pathname)).toEqual([
        '/api/v2/auth/me',
      ]);
    });

    it(`hides ${page} data while a refresh is pending and rejects missing echoes`, async () => {
      let held = false;
      let release!: (value: Response) => void;
      const body =
        page === 'list' ? { items: [source], next_cursor: null, has_more: false } : source;
      installHandler((url) => {
        if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
        return held
          ? new Promise<Response>((resolve) => {
              release = resolve;
            })
          : response(body, 'valid');
      });
      renderPage(route);
      expect(await screen.findByText('B private source')).toBeInTheDocument();
      held = true;
      await userEvent.click(screen.getByRole('button', { name: 'Refresh' }));
      await waitFor(() => expect(release).toBeDefined());
      expect(screen.queryByText('B private source')).not.toBeInTheDocument();
      await act(async () => release(response(body, 'missing')));
      expect(await screen.findByRole('alert')).toBeInTheDocument();
      expect(screen.queryByText('B private source')).not.toBeInTheDocument();
    });

    it(`hides stale ${page} data on a reconnect failure`, async () => {
      let reads = 0;
      let release!: (value: Response) => void;
      const body =
        page === 'list' ? { items: [source], next_cursor: null, has_more: false } : source;
      installHandler((url) => {
        if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
        reads++;
        return reads === 1
          ? response(body, 'valid')
          : new Promise<Response>((resolve) => {
              release = resolve;
            });
      });
      renderPage(route);
      expect(await screen.findByText('B private source')).toBeInTheDocument();
      act(() => {
        onlineManager.setOnline(false);
        onlineManager.setOnline(true);
      });
      await waitFor(() => expect(release).toBeDefined());
      expect(screen.queryByText('B private source')).not.toBeInTheDocument();
      await act(async () => release(new Response(null, { status: 503 })));
      expect(await screen.findByRole('alert')).toBeInTheDocument();
      expect(screen.queryByText('B private source')).not.toBeInTheDocument();
    });

    it.each([401, 403, 409])(
      `revalidates identity after bodyless %i on ${page}`,
      async (status) => {
        let current: User | null = admin;
        let failing = false;
        let authReads = 0;
        const body =
          page === 'list' ? { items: [source], next_cursor: null, has_more: false } : source;
        installHandler((url) => {
          if (url.pathname.endsWith('/auth/me')) {
            authReads++;
            return current ? Response.json(current) : new Response(null, { status: 401 });
          }
          if (!failing) return response(body, 'valid');
          if (status === 401) current = null;
          if (status === 403) current = { ...admin, role: 'user' };
          if (status === 409) current = { ...admin, scope_id: 'scope-next', version: 2 };
          return new Response(null, { status });
        });
        renderPage(route);
        expect(await screen.findByText('B private source')).toBeInTheDocument();
        failing = true;
        await userEvent.click(screen.getByRole('button', { name: 'Refresh' }));
        await waitFor(() => expect(authReads).toBeGreaterThan(1));
        expect(screen.queryByText('B private source')).not.toBeInTheDocument();
      },
    );

    it(`discards a held ${page} response after a scope change`, async () => {
      const next = { ...admin, scope_id: 'scope-next', version: 2 };
      let releaseOld!: (value: Response) => void;
      let releaseNew!: (value: Response) => void;
      const oldBody =
        page === 'list' ? { items: [source], next_cursor: null, has_more: false } : source;
      const latest = { ...source, root_name: 'Current source' };
      const newBody =
        page === 'list' ? { items: [latest], next_cursor: null, has_more: false } : latest;
      installHandler((url, request) => {
        if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
        if (request.headers.get(scopeHeader) === admin.scope_id)
          return new Promise<Response>((resolve) => {
            releaseOld = resolve;
          });
        expect(request.headers.get(scopeHeader)).toBe(next.scope_id);
        return new Promise<Response>((resolve) => {
          releaseNew = resolve;
        });
      });
      const client = renderPage(route);
      await waitFor(() => expect(releaseOld).toBeDefined());
      act(() => client.setQueryData(['auth', 'me'], next));
      await waitFor(() => expect(releaseNew).toBeDefined());
      await act(async () => releaseOld(response(oldBody, 'valid', admin)));
      expect(screen.queryByText('B private source')).not.toBeInTheDocument();
      await act(async () => releaseNew(response(newBody, 'valid', next)));
      expect(await screen.findByText('Current source')).toBeInTheDocument();
    });
  }
});
