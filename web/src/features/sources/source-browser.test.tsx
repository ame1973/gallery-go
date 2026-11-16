import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, screen, waitFor, within } from '@testing-library/react';
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
const reader: User = { ...admin, id: 'reader', role: 'user' };
const source: Source = {
  id: 'source.with.dot',
  library_id: 'library-one',
  root_name: 'Family archive',
  mapping: { album_depth: 2, person_depth: 1 },
  active: false,
  configured: false,
  state: 'error',
  minimum_files: 5,
  poll_interval_seconds: 60,
  full_scan_interval_seconds: 3600,
  scan_generation: 0,
  version: 2,
  config_version: 3,
  last_checked_at: '2026-11-16T12:00:00Z',
  last_healthy_at: null,
  last_full_scan_at: null,
  last_error_code: 'SOURCE_UNAVAILABLE',
  created_at: '2026-11-16T12:00:00Z',
  updated_at: '2026-11-16T12:00:00Z',
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

function installFetch(handler: (url: URL, request: Request) => Response | Promise<Response>) {
  const fetch = vi.fn(async (request: Request) => handler(new URL(request.url), request));
  vi.stubGlobal('fetch', fetch);
  return fetch;
}

function problem(status: number, code: string) {
  return Response.json(
    { code, status, type: 'urn:test', title: 'Test', request_id: 'id', detail: '/private/secret' },
    { status },
  );
}

beforeEach(() => localStorage.clear());
afterEach(() => vi.unstubAllGlobals());

describe('administrative source browser', () => {
  it('offers source navigation to an authenticated administrator', async () => {
    installFetch((url) =>
      url.pathname.endsWith('/auth/me') ? Response.json(admin) : Response.json(emptyPage),
    );
    renderPage('/settings/security');
    const navigation = await screen.findByRole('navigation', { name: 'Gallery navigation' });
    expect(navigation).toHaveTextContent('Sources');
    expect(screen.getByRole('link', { name: 'Sources' })).toHaveAttribute('href', '/admin/sources');
  });

  it('does not offer administrative source navigation to a regular user', async () => {
    installFetch((url) =>
      url.pathname.endsWith('/auth/me') ? Response.json(reader) : Response.json(emptyPage),
    );
    renderPage('/settings/security');
    await screen.findByRole('navigation', { name: 'Gallery navigation' });
    expect(screen.queryByRole('link', { name: 'Sources' })).not.toBeInTheDocument();
  });

  it.each(['/admin/sources', '/admin/sources/source.with.dot/details'])(
    'requires a live session before loading %s',
    async (path) => {
      const fetch = installFetch(() => problem(401, 'UNAUTHENTICATED'));
      renderPage(path);
      expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
      expect(fetch.mock.calls.map(([request]) => new URL(request.url).pathname)).toEqual([
        '/api/v2/auth/me',
      ]);
    },
  );

  it.each(['/admin/sources', '/admin/sources/source.with.dot/details'])(
    'does not request %s for an ordinary account',
    async (path) => {
      const fetch = installFetch((url) =>
        url.pathname.endsWith('/auth/me') ? Response.json(reader) : problem(403, 'ADMIN_REQUIRED'),
      );
      renderPage(path);
      expect(
        await screen.findByText('Administrator access is required to view sources.'),
      ).toBeInTheDocument();
      expect(fetch.mock.calls.map(([request]) => new URL(request.url).pathname)).toEqual([
        '/api/v2/auth/me',
      ]);
    },
  );

  it('uses bounded server pages and retains the local return context', async () => {
    const requests: URL[] = [];
    const fetch = installFetch((url) => {
      requests.push(url);
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      if (url.pathname === '/api/v2/admin/sources') {
        return Response.json(
          url.searchParams.has('cursor')
            ? { ...emptyPage, items: [source] }
            : {
                items: [{ ...source, id: 'first', root_name: 'First' }],
                next_cursor: 'next',
                has_more: true,
              },
        );
      }
      if (url.pathname === '/api/v2/admin/sources/source.with.dot') return Response.json(source);
      return problem(404, 'NOT_FOUND');
    });
    renderPage('/admin/sources');
    expect(await screen.findByRole('link', { name: 'View source First' })).toBeInTheDocument();
    await userEvent.click(screen.getByRole('link', { name: 'Next page' }));
    expect(
      await screen.findByRole('link', { name: 'View source Family archive' }),
    ).toBeInTheDocument();
    expect(screen.getByRole('main')).toHaveFocus();
    await userEvent.click(screen.getByRole('link', { name: 'View source Family archive' }));
    expect(await screen.findByRole('heading', { name: 'Family archive' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to sources' })).toHaveAttribute(
      'href',
      '/admin/sources?cursor=next',
    );
    expect(requests.filter((url) => url.pathname === '/api/v2/admin/sources')).toHaveLength(2);
    expect(
      requests
        .filter((url) => url.pathname === '/api/v2/admin/sources')
        .every((url) => url.searchParams.get('limit') === '30'),
    ).toBe(true);
    expect(fetch.mock.calls.every(([request]) => request.method === 'GET')).toBe(true);
  });

  it('distinguishes configuration, reachability, inventory and scheduling', async () => {
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      if (url.pathname === '/api/v2/admin/sources/source.with.dot') {
        return Response.json({ ...source, root_path: '/private/root', marker: '.private-marker' });
      }
      return problem(404, 'NOT_FOUND');
    });
    renderPage('/admin/sources/source.with.dot/details');
    expect(await screen.findByRole('heading', { name: 'Family archive' })).toBeInTheDocument();
    expect(screen.getByText('Inactive')).toBeInTheDocument();
    expect(screen.getByText('Missing or changed on this server')).toBeInTheDocument();
    expect(screen.getByText('Error')).toBeInTheDocument();
    expect(screen.getByText('Published inventory generation')).toBeInTheDocument();
    expect(screen.getByText('3,600 seconds')).toBeInTheDocument();
    expect(screen.getByText('SOURCE_UNAVAILABLE')).toBeInTheDocument();
    expect(screen.getByText(/does not prove a completed inventory/)).toBeInTheDocument();
    expect(screen.queryByText('/private/root')).not.toBeInTheDocument();
    expect(screen.queryByText('.private-marker')).not.toBeInTheDocument();
  });

  it('does not display unknown diagnostic data', async () => {
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      return Response.json({ ...source, last_error_code: '/private/unexpected' });
    });
    renderPage('/admin/sources/source.with.dot/details');
    expect(await screen.findByText('Another source diagnostic was recorded.')).toBeInTheDocument();
    expect(screen.queryByText('/private/unexpected')).not.toBeInTheDocument();
  });

  it.each([
    '/admin/sources?cursor=',
    '/admin/sources?cursor=a&cursor=b',
    '/admin/sources?unknown=value',
    '/admin/sources/source.with.dot/details?unexpected=1',
    '/admin/sources/bad%2Fid/details',
  ])('rejects an invalid source page before a request: %s', async (path) => {
    const fetch = installFetch((url) =>
      url.pathname.endsWith('/auth/me') ? Response.json(admin) : Response.json(emptyPage),
    );
    renderPage(path);
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(fetch.mock.calls.map(([request]) => new URL(request.url).pathname)).toEqual([
      '/api/v2/auth/me',
    ]);
  });

  it.each([
    { items: [null], next_cursor: null, has_more: false },
    { items: [source, source], next_cursor: null, has_more: false },
    { items: [source], next_cursor: null, has_more: true },
    { items: [{ ...source, id: '../outside' }], next_cursor: null, has_more: false },
  ])('rejects malformed source pages without creating links', async (page) => {
    installFetch((url) =>
      url.pathname.endsWith('/auth/me') ? Response.json(admin) : Response.json(page),
    );
    renderPage('/admin/sources');
    expect(await screen.findByText(/Could not load sources/)).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /View source/ })).not.toBeInTheDocument();
  });

  it.each(['https://example.com', '//example.com', '/admin/sources?cursor=a&cursor=b', '/albums'])(
    'rejects an unsafe return context: %s',
    async (from) => {
      installFetch((url) =>
        url.pathname.endsWith('/auth/me') ? Response.json(admin) : Response.json(source),
      );
      renderPage({ pathname: '/admin/sources/source.with.dot/details', state: { from } });
      expect(await screen.findByRole('heading', { name: 'Family archive' })).toBeInTheDocument();
      expect(screen.getByRole('link', { name: 'Back to sources' })).toHaveAttribute(
        'href',
        '/admin/sources',
      );
    },
  );

  it('hides stale rows during a failed refresh and recovers from a fresh read', async () => {
    let held = false;
    let release!: (response: Response) => void;
    const fetch = installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      if (url.pathname === '/api/v2/admin/sources') {
        return held
          ? new Promise<Response>((resolve) => {
              release = resolve;
            })
          : Response.json({ ...emptyPage, items: [source] });
      }
      return problem(404, 'NOT_FOUND');
    });
    renderPage('/admin/sources');
    expect(
      await screen.findByRole('link', { name: 'View source Family archive' }),
    ).toBeInTheDocument();
    held = true;
    await userEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    await waitFor(() => expect(release).toBeDefined());
    expect(screen.queryByText('Family archive')).not.toBeInTheDocument();
    await act(async () => release(problem(503, 'INTERNAL_ERROR')));
    const alert = await screen.findByRole('alert');
    expect(within(alert).getByText(/Could not load sources/)).toBeInTheDocument();
    expect(screen.queryByText('/private/secret')).not.toBeInTheDocument();
    expect(screen.queryByText('Family archive')).not.toBeInTheDocument();
    held = false;
    await userEvent.click(within(alert).getByRole('button', { name: 'Refresh' }));
    expect(
      await screen.findByRole('link', { name: 'View source Family archive' }),
    ).toBeInTheDocument();
    expect(fetch.mock.calls.filter(([request]) => request.method !== 'GET')).toHaveLength(0);
  });

  it('hides old data and revalidates identity after a bodyless 401', async () => {
    let denied = false;
    let release!: (response: Response) => void;
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) {
        return denied
          ? new Promise<Response>((resolve) => {
              release = resolve;
            })
          : Response.json(admin);
      }
      if (url.pathname === '/api/v2/admin/sources') {
        return denied
          ? new Response(null, { status: 401 })
          : Response.json({ ...emptyPage, items: [source] });
      }
      return problem(404, 'NOT_FOUND');
    });
    renderPage('/admin/sources');
    expect(
      await screen.findByRole('link', { name: 'View source Family archive' }),
    ).toBeInTheDocument();
    denied = true;
    await userEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    await waitFor(() => expect(release).toBeDefined());
    expect(screen.queryByText('Family archive')).not.toBeInTheDocument();
    await act(async () => release(problem(401, 'UNAUTHENTICATED')));
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
  });

  it('rejects a detail response for a different source', async () => {
    installFetch((url) =>
      url.pathname.endsWith('/auth/me')
        ? Response.json(admin)
        : Response.json({ ...source, id: 'other-source', root_name: 'Other source' }),
    );
    renderPage('/admin/sources/source.with.dot/details');
    expect(await screen.findByText(/Could not load sources/)).toBeInTheDocument();
    expect(screen.queryByText('Other source')).not.toBeInTheDocument();
  });

  it('hides source details when a fresh administrator read is forbidden', async () => {
    let denied = false;
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      return denied ? new Response(null, { status: 403 }) : Response.json(source);
    });
    renderPage('/admin/sources/source.with.dot/details');
    expect(await screen.findByRole('heading', { name: 'Family archive' })).toBeInTheDocument();
    denied = true;
    await userEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    expect(
      await screen.findByText('Administrator access is required to view sources.'),
    ).toBeInTheDocument();
    expect(screen.queryByText('Family archive')).not.toBeInTheDocument();
    expect(screen.queryByText('SOURCE_UNAVAILABLE')).not.toBeInTheDocument();
  });

  it.each([
    { change: 'account', next: { ...admin, id: 'admin-b', scope_id: 'scope-b' } },
    { change: 'scope', next: { ...admin, scope_id: 'scope-b' } },
  ])('discards prior data when the $change changes', async ({ next }) => {
    let current = admin;
    let release!: (response: Response) => void;
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(current);
      if (url.pathname === '/api/v2/admin/sources') {
        return current.scope_id === 'scope-b'
          ? new Promise<Response>((resolve) => {
              release = resolve;
            })
          : Response.json({ ...emptyPage, items: [source] });
      }
      return problem(404, 'NOT_FOUND');
    });
    const client = renderPage('/admin/sources');
    expect(
      await screen.findByRole('link', { name: 'View source Family archive' }),
    ).toBeInTheDocument();
    current = next;
    act(() => client.setQueryData(['auth', 'me'], current));
    await waitFor(() => expect(release).toBeDefined());
    expect(screen.queryByText('Family archive')).not.toBeInTheDocument();
    await act(async () => release(Response.json(emptyPage)));
    expect(await screen.findByText('No sources are registered.')).toBeInTheDocument();
  });
});
