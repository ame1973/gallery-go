import { onlineManager, QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from '../../app/App';
import type { User } from '../auth/api';

const admin: User = {
  id: 'admin-a',
  username: 'owner',
  role: 'admin',
  active: true,
  scope_id: 'scope-a',
  version: 1,
  download_original: false,
};
const group = {
  id: 'group-a',
  name: 'Family 相簿',
  version: 2,
  created_at: '2026-11-26T00:00:00Z',
};
const page = { items: [group], next_cursor: null, has_more: false };
const userHeader = 'X-GalleryGo-Expected-User';
const scopeHeader = 'X-GalleryGo-Expected-Scope';

function bound(body: unknown, actor = admin) {
  return Response.json(body, {
    headers: { [userHeader]: actor.id, [scopeHeader]: actor.scope_id },
  });
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

function installFetch(handler: (url: URL, request: Request) => Response | Promise<Response>) {
  const fetch = vi.fn(async (request: Request) => handler(new URL(request.url), request));
  vi.stubGlobal('fetch', fetch);
  return fetch;
}

beforeEach(() => localStorage.clear());
afterEach(() => {
  onlineManager.setOnline(true);
  vi.unstubAllGlobals();
});

describe('read-only administrative groups', () => {
  it('offers group navigation to administrators', async () => {
    installFetch((url) => (url.pathname.endsWith('/auth/me') ? Response.json(admin) : bound(page)));
    renderPage('/settings/security');
    expect(await screen.findByRole('link', { name: 'Groups' })).toHaveAttribute(
      'href',
      '/admin/groups',
    );
  });

  it('opens a group from an identity-bound list', async () => {
    const fetch = installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      return bound(url.pathname === '/api/v2/admin/groups' ? page : group);
    });
    renderPage('/admin/groups');
    await userEvent.click(await screen.findByRole('link', { name: 'View group Family 相簿' }));
    expect(await screen.findByRole('heading', { name: 'Family 相簿' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to groups' })).toHaveAttribute(
      'href',
      '/admin/groups',
    );
    const requests = fetch.mock.calls
      .map(([request]) => request)
      .filter((request) => new URL(request.url).pathname.startsWith('/api/v2/admin/groups'));
    expect(requests).toHaveLength(2);
    for (const request of requests) {
      expect(request.method).toBe('GET');
      expect(request.headers.get(userHeader)).toBe(admin.id);
      expect(request.headers.get(scopeHeader)).toBe(admin.scope_id);
    }
  });

  it.each(['/admin/groups', '/admin/groups/group-a/details'])(
    'does not fetch %s for a regular user',
    async (path) => {
      const reader = { ...admin, role: 'user' as const };
      const fetch = installFetch((url) =>
        url.pathname.endsWith('/auth/me') ? Response.json(reader) : bound(page),
      );
      renderPage(path);
      expect(
        await screen.findByText('Administrator access is required to view groups.'),
      ).toBeInTheDocument();
      expect(screen.queryByRole('link', { name: 'Groups' })).not.toBeInTheDocument();
      expect(fetch.mock.calls.map(([request]) => new URL(request.url).pathname)).toEqual([
        '/api/v2/auth/me',
      ]);
    },
  );

  it('paginates in stable pages and preserves the validated return location', async () => {
    const second = { ...group, id: 'group-b', name: 'Friends' };
    const fetch = installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      if (url.pathname.endsWith('/group-b')) return bound(second);
      if (url.searchParams.has('cursor')) return bound({ ...page, items: [second] });
      return bound({ ...page, has_more: true, next_cursor: 'next-page' });
    });
    renderPage('/admin/groups');
    await screen.findByRole('link', { name: 'View group Family 相簿' });
    await userEvent.click(screen.getByRole('link', { name: 'Next page' }));
    const link = await screen.findByRole('link', { name: 'View group Friends' });
    expect(screen.getByRole('main')).toHaveFocus();
    await userEvent.click(link);
    expect(await screen.findByRole('heading', { name: 'Friends' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to groups' })).toHaveAttribute(
      'href',
      '/admin/groups?cursor=next-page',
    );
    const listRequests = fetch.mock.calls
      .map(([request]) => new URL(request.url))
      .filter((url) => url.pathname === '/api/v2/admin/groups');
    expect(listRequests).toHaveLength(2);
    expect(listRequests.every((url) => url.searchParams.get('limit') === '30')).toBe(true);
    expect(listRequests[1]?.searchParams.get('cursor')).toBe('next-page');
  });

  it.each([
    '/admin/groups?cursor=',
    '/admin/groups?cursor=a&cursor=b',
    '/admin/groups?sort=name',
    `/admin/groups?cursor=${'a'.repeat(513)}`,
    '/admin/groups/bad%2Fid/details',
    '/admin/groups/group-a/details?unexpected=1',
  ])('rejects invalid navigation before requesting %s', async (path) => {
    const fetch = installFetch((url) =>
      url.pathname.endsWith('/auth/me') ? Response.json(admin) : bound(page),
    );
    renderPage(path);
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(fetch.mock.calls.map(([request]) => new URL(request.url).pathname)).toEqual([
      '/api/v2/auth/me',
    ]);
  });

  it.each(['/admin/groups', '/admin/groups/group-a/details'])(
    'requires authentication for %s',
    async (path) => {
      const fetch = installFetch(() => new Response(null, { status: 401 }));
      renderPage(path);
      expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
      expect(fetch.mock.calls.map(([request]) => new URL(request.url).pathname)).toEqual([
        '/api/v2/auth/me',
      ]);
    },
  );

  for (const path of ['/admin/groups', '/admin/groups/group-a/details']) {
    const body = path === '/admin/groups' ? page : group;
    it.each(['missing', 'wrong-user', 'wrong-scope'])('hides %s echo on ' + path, async (mode) => {
      installFetch((url) => {
        if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
        if (mode === 'missing') return Response.json(body);
        return bound(
          body,
          mode === 'wrong-user' ? { ...admin, id: 'admin-b' } : { ...admin, scope_id: 'scope-b' },
        );
      });
      renderPage(path);
      expect(await screen.findByRole('alert')).toBeInTheDocument();
      expect(screen.queryByText(group.name)).not.toBeInTheDocument();
    });

    it('hides old data while refreshing and after an error on ' + path, async () => {
      let held = false;
      let release!: (response: Response) => void;
      installFetch((url) => {
        if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
        return held
          ? new Promise<Response>((resolve) => {
              release = resolve;
            })
          : bound(body);
      });
      renderPage(path);
      expect(await screen.findByText(group.name)).toBeInTheDocument();
      held = true;
      await userEvent.click(screen.getByRole('button', { name: 'Refresh' }));
      await waitFor(() => expect(release).toBeDefined());
      expect(screen.queryByText(group.name)).not.toBeInTheDocument();
      await act(async () =>
        release(
          Response.json({ code: 'INTERNAL_ERROR', detail: '/private/secret' }, { status: 503 }),
        ),
      );
      const alert = await screen.findByRole('alert');
      expect(screen.queryByText(group.name)).not.toBeInTheDocument();
      expect(screen.queryByText('/private/secret')).not.toBeInTheDocument();
      held = false;
      await userEvent.click(within(alert).getByRole('button', { name: 'Refresh' }));
      expect(await screen.findByText(group.name)).toBeInTheDocument();
    });

    it.each([401, 403, 409])(
      'revalidates identity after bodyless %i on ' + path,
      async (status) => {
        let current: User | null = admin;
        let failing = false;
        let authReads = 0;
        installFetch((url) => {
          if (url.pathname.endsWith('/auth/me')) {
            authReads++;
            return current ? Response.json(current) : new Response(null, { status: 401 });
          }
          if (!failing) return bound(body);
          if (status === 401) current = null;
          if (status === 403) current = { ...admin, role: 'user' };
          if (status === 409) current = { ...admin, scope_id: 'scope-next', version: 2 };
          return new Response(null, { status });
        });
        renderPage(path);
        expect(await screen.findByText(group.name)).toBeInTheDocument();
        failing = true;
        await userEvent.click(screen.getByRole('button', { name: 'Refresh' }));
        await waitFor(() => expect(authReads).toBeGreaterThan(1));
        expect(screen.queryByText(group.name)).not.toBeInTheDocument();
      },
    );

    it('hides stale data when a reconnect read fails on ' + path, async () => {
      let reads = 0;
      let release!: (response: Response) => void;
      installFetch((url) => {
        if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
        reads++;
        return reads === 1
          ? bound(body)
          : new Promise<Response>((resolve) => {
              release = resolve;
            });
      });
      renderPage(path);
      expect(await screen.findByText(group.name)).toBeInTheDocument();
      act(() => {
        onlineManager.setOnline(false);
        onlineManager.setOnline(true);
      });
      await waitFor(() => expect(release).toBeDefined());
      expect(screen.queryByText(group.name)).not.toBeInTheDocument();
      await act(async () => release(new Response(null, { status: 503 })));
      expect(await screen.findByRole('alert')).toBeInTheDocument();
      expect(screen.queryByText(group.name)).not.toBeInTheDocument();
    });

    it.each([
      { ...admin, scope_id: 'scope-next', version: 2 },
      { ...admin, id: 'admin-b', scope_id: 'scope-b' },
    ])('discards a held response after identity changes on ' + path, async (next) => {
      const fresh = { ...group, name: 'Current group' };
      const freshBody = path === '/admin/groups' ? { ...page, items: [fresh] } : fresh;
      let releaseOld!: (response: Response) => void;
      let releaseNew!: (response: Response) => void;
      installFetch((url, request) => {
        if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
        return new Promise<Response>((resolve) => {
          if (request.headers.get(scopeHeader) === admin.scope_id) releaseOld = resolve;
          else releaseNew = resolve;
        });
      });
      const client = renderPage(path);
      await waitFor(() => expect(releaseOld).toBeDefined());
      act(() => client.setQueryData(['auth', 'me'], next));
      await waitFor(() => expect(releaseNew).toBeDefined());
      await act(async () => releaseOld(bound(body)));
      expect(screen.queryByText(group.name)).not.toBeInTheDocument();
      await act(async () => releaseNew(bound(freshBody, next)));
      expect(await screen.findByText(fresh.name)).toBeInTheDocument();
    });
  }

  it.each([
    { ...page, items: [null] },
    { ...page, items: [group, group] },
    { ...page, items: [{ ...group, id: '../outside' }] },
    { ...page, items: [{ ...group, name: 'bad\nname' }] },
    { ...page, items: [{ ...group, name: '😀'.repeat(101) }] },
    { ...page, items: [{ ...group, created_at: 'not-a-date' }] },
    { ...page, items: [{ ...group, version: 0 }] },
    { ...page, has_more: true },
    { ...page, next_cursor: 'unexpected' },
  ])('rejects malformed group pages without showing rows', async (body) => {
    installFetch((url) => (url.pathname.endsWith('/auth/me') ? Response.json(admin) : bound(body)));
    renderPage('/admin/groups');
    expect(await screen.findByText(/Could not load groups/)).toBeInTheDocument();
    expect(screen.queryByText(group.name)).not.toBeInTheDocument();
  });

  it('accepts a group name at the Unicode character and byte limits', async () => {
    const name = '😀'.repeat(100);
    installFetch((url) =>
      url.pathname.endsWith('/auth/me')
        ? Response.json(admin)
        : bound({ ...page, items: [{ ...group, name }] }),
    );
    renderPage('/admin/groups');
    expect(await screen.findByText(name)).toBeInTheDocument();
  });

  it('shows a bounded empty page without inventing a next link', async () => {
    installFetch((url) =>
      url.pathname.endsWith('/auth/me') ? Response.json(admin) : bound({ ...page, items: [] }),
    );
    renderPage('/admin/groups');
    expect(await screen.findByText('No groups are available on this page.')).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Next page' })).not.toBeInTheDocument();
  });

  it('shows a safe missing-group message for a bodyless 404', async () => {
    installFetch((url) =>
      url.pathname.endsWith('/auth/me')
        ? Response.json(admin)
        : new Response(null, { status: 404 }),
    );
    renderPage('/admin/groups/group-a/details');
    expect(await screen.findByText('This group is unavailable.')).toBeInTheDocument();
    expect(screen.queryByText(group.name)).not.toBeInTheDocument();
  });

  it.each(['/admin/groups', '/admin/groups/group-a/details'])(
    'does not request %s with an invalid actor',
    async (path) => {
      const fetch = installFetch((url) =>
        url.pathname.endsWith('/auth/me')
          ? Response.json({ ...admin, scope_id: 'invalid scope' })
          : bound(page),
      );
      renderPage(path);
      expect(await screen.findByRole('alert')).toBeInTheDocument();
      expect(fetch.mock.calls.map(([request]) => new URL(request.url).pathname)).toEqual([
        '/api/v2/auth/me',
      ]);
    },
  );

  it('rejects a detail response for a different group', async () => {
    installFetch((url) =>
      url.pathname.endsWith('/auth/me') ? Response.json(admin) : bound({ ...group, id: 'other' }),
    );
    renderPage('/admin/groups/group-a/details');
    expect(await screen.findByText(/Could not load groups/)).toBeInTheDocument();
    expect(screen.queryByText(group.name)).not.toBeInTheDocument();
  });

  it.each([
    ['zh-TW', '群組'],
    ['zh-CN', '群组'],
  ] as const)('renders the %s locale', async (locale, title) => {
    localStorage.setItem('gallerygo:locale', locale);
    installFetch((url) => (url.pathname.endsWith('/auth/me') ? Response.json(admin) : bound(page)));
    renderPage('/admin/groups');
    expect(await screen.findByRole('heading', { name: title })).toBeInTheDocument();
    expect(await screen.findByText(group.name)).toBeInTheDocument();
  });
});
