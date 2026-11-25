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
const reader: User = {
  ...admin,
  id: 'reader',
  username: 'reader',
  role: 'user',
  scope_id: 'scope-reader',
};
const second: User = {
  ...admin,
  id: 'second.user',
  username: 'second',
  role: 'user',
  scope_id: 'scope-second',
  active: false,
  version: 2,
  download_original: true,
};
const otherAdmin: User = { ...admin, id: 'admin-b', username: 'otheradmin', scope_id: 'scope-b' };
const emptyPage = { items: [], has_more: false, next_cursor: null };
const expectedUser = 'X-GalleryGo-Expected-User';
const expectedScope = 'X-GalleryGo-Expected-Scope';

function bound(data: unknown, actor = admin) {
  return Response.json(data, {
    headers: { [expectedUser]: actor.id, [expectedScope]: actor.scope_id },
  });
}

function problem(status: number, code: string) {
  return Response.json(
    { status, code, type: 'urn:test', title: 'Test', request_id: 'id', detail: '/private/secret' },
    { status },
  );
}

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

beforeEach(() => localStorage.clear());
afterEach(() => {
  onlineManager.setOnline(true);
  vi.unstubAllGlobals();
});

describe('read-only administrative accounts', () => {
  it('shows account navigation only to an administrator', async () => {
    installFetch((url) =>
      url.pathname.endsWith('/auth/me') ? Response.json(admin) : bound(emptyPage),
    );
    renderPage('/settings/security');
    expect(await screen.findByRole('link', { name: 'Accounts' })).toHaveAttribute(
      'href',
      '/admin/users',
    );
  });

  it.each(['/admin/users', '/admin/users/second.user/details'])(
    'does not load %s without an admin',
    async (path) => {
      const fetch = installFetch((url) =>
        url.pathname.endsWith('/auth/me') ? Response.json(reader) : problem(403, 'ADMIN_REQUIRED'),
      );
      renderPage(path);
      expect(
        await screen.findByText('Administrator access is required to view accounts.'),
      ).toBeInTheDocument();
      expect(screen.queryByRole('link', { name: 'Accounts' })).not.toBeInTheDocument();
      expect(fetch.mock.calls.map(([request]) => new URL(request.url).pathname)).toEqual([
        '/api/v2/auth/me',
      ]);
    },
  );

  it.each(['/admin/users', '/admin/users/second.user/details'])(
    'requires a session before loading %s',
    async (path) => {
      const fetch = installFetch(() => new Response(null, { status: 401 }));
      renderPage(path);
      expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
      expect(fetch.mock.calls.map(([request]) => new URL(request.url).pathname)).toEqual([
        '/api/v2/auth/me',
      ]);
    },
  );

  it('paginates with bound headers and preserves a validated return path', async () => {
    const requests: Request[] = [];
    installFetch((url, request) => {
      requests.push(request);
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      if (url.pathname === '/api/v2/admin/users') {
        return url.searchParams.has('cursor')
          ? bound({ ...emptyPage, items: [second] })
          : bound({ items: [admin], next_cursor: 'next-cursor', has_more: true });
      }
      if (url.pathname === '/api/v2/admin/users/second.user') return bound(second);
      return problem(404, 'NOT_FOUND');
    });
    renderPage('/admin/users');
    expect(await screen.findByRole('link', { name: 'View account owner' })).toBeInTheDocument();
    await userEvent.click(screen.getByRole('link', { name: 'Next page' }));
    expect(await screen.findByRole('link', { name: 'View account second' })).toBeInTheDocument();
    expect(screen.getByRole('main')).toHaveFocus();
    const detailsLink = screen.getByRole('link', { name: 'View account second' });
    detailsLink.focus();
    await userEvent.keyboard('{Enter}');
    expect(await screen.findByRole('heading', { name: 'second' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to accounts' })).toHaveAttribute(
      'href',
      '/admin/users?cursor=next-cursor',
    );
    expect(screen.getByText('Inactive')).toBeInTheDocument();
    expect(screen.getByText('Allowed')).toBeInTheDocument();
    const accountRequests = requests.filter((request) =>
      new URL(request.url).pathname.startsWith('/api/v2/admin/users'),
    );
    expect(accountRequests).toHaveLength(3);
    for (const request of accountRequests) {
      expect(request.method).toBe('GET');
      expect(request.headers.get(expectedUser)).toBe(admin.id);
      expect(request.headers.get(expectedScope)).toBe(admin.scope_id);
    }
    expect(
      accountRequests
        .filter((request) => new URL(request.url).pathname === '/api/v2/admin/users')
        .every((request) => new URL(request.url).searchParams.get('limit') === '30'),
    ).toBe(true);
  });

  it.each([
    '/admin/users?cursor=',
    '/admin/users?cursor=a&cursor=b',
    '/admin/users?sort=name',
    '/admin/users/bad%2Fid/details',
    '/admin/users/second.user/details?unexpected=1',
  ])('rejects an invalid page before requesting it: %s', async (path) => {
    const fetch = installFetch((url) =>
      url.pathname.endsWith('/auth/me') ? Response.json(admin) : bound(emptyPage),
    );
    renderPage(path);
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(fetch.mock.calls.map(([request]) => new URL(request.url).pathname)).toEqual([
      '/api/v2/auth/me',
    ]);
  });

  it.each(['https://example.com', '//example.com', '/admin/users?cursor=a&cursor=b', '/albums'])(
    'rejects unsafe back navigation: %s',
    async (from) => {
      installFetch((url) =>
        url.pathname.endsWith('/auth/me') ? Response.json(admin) : bound(second),
      );
      renderPage({ pathname: '/admin/users/second.user/details', state: { from } });
      expect(await screen.findByRole('heading', { name: 'second' })).toBeInTheDocument();
      expect(screen.getByRole('link', { name: 'Back to accounts' })).toHaveAttribute(
        'href',
        '/admin/users',
      );
    },
  );

  it.each([
    { items: [null], has_more: false, next_cursor: null },
    { items: [second, second], has_more: false, next_cursor: null },
    { items: [second], has_more: true, next_cursor: null },
    { items: [{ ...second, id: '../outside' }], has_more: false, next_cursor: null },
    { items: [{ ...second, username: '/private/secret' }], has_more: false, next_cursor: null },
  ])('rejects malformed account pages without rendering rows', async (page) => {
    installFetch((url) => (url.pathname.endsWith('/auth/me') ? Response.json(admin) : bound(page)));
    renderPage('/admin/users');
    expect(await screen.findByText(/Could not load accounts/)).toBeInTheDocument();
    expect(screen.queryByText('/private/secret')).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /View account/ })).not.toBeInTheDocument();
  });

  it.each(['missing', 'wrong-account', 'wrong-scope'])(
    'hides data when the response echo is %s',
    async (mode) => {
      installFetch((url) => {
        if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
        if (mode === 'missing') return Response.json({ ...emptyPage, items: [second] });
        return bound(
          { ...emptyPage, items: [second] },
          mode === 'wrong-account' ? otherAdmin : { ...admin, scope_id: 'scope-changed' },
        );
      });
      renderPage('/admin/users');
      expect(await screen.findByText(/could not be verified/)).toBeInTheDocument();
      expect(screen.queryByText('second')).not.toBeInTheDocument();
      expect(screen.queryByRole('link', { name: /View account/ })).not.toBeInTheDocument();
    },
  );

  it('hides old rows during a failed refresh and recovers only from a fresh bound response', async () => {
    let held = false;
    let release!: (response: Response) => void;
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      return held
        ? new Promise<Response>((resolve) => {
            release = resolve;
          })
        : bound({ ...emptyPage, items: [second] });
    });
    renderPage('/admin/users');
    expect(await screen.findByRole('link', { name: 'View account second' })).toBeInTheDocument();
    held = true;
    await userEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    await waitFor(() => expect(release).toBeDefined());
    expect(screen.queryByText('second')).not.toBeInTheDocument();
    await act(async () => release(problem(503, 'INTERNAL_ERROR')));
    const alert = await screen.findByRole('alert');
    expect(within(alert).getByText(/Could not load accounts/)).toBeInTheDocument();
    expect(screen.queryByText('/private/secret')).not.toBeInTheDocument();
    expect(screen.queryByText('second')).not.toBeInTheDocument();
    held = false;
    await userEvent.click(within(alert).getByRole('button', { name: 'Refresh' }));
    expect(await screen.findByRole('link', { name: 'View account second' })).toBeInTheDocument();
  });

  it('hides the prior account when the authenticated actor changes during a held read', async () => {
    let current = admin;
    let releaseOld!: (response: Response) => void;
    let releaseNew!: (response: Response) => void;
    installFetch((url, request) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(current);
      if (request.headers.get(expectedUser) === admin.id)
        return new Promise<Response>((resolve) => {
          releaseOld = resolve;
        });
      return new Promise<Response>((resolve) => {
        releaseNew = resolve;
      });
    });
    const client = renderPage('/admin/users');
    await waitFor(() => expect(releaseOld).toBeDefined());
    current = otherAdmin;
    act(() => client.setQueryData(['auth', 'me'], current));
    await waitFor(() => expect(releaseNew).toBeDefined());
    await act(async () => releaseOld(bound({ ...emptyPage, items: [second] }, admin)));
    expect(screen.queryByText('second')).not.toBeInTheDocument();
    await act(async () => releaseNew(bound(emptyPage, otherAdmin)));
    expect(await screen.findByText('No accounts are available on this page.')).toBeInTheDocument();
  });

  it.each(['missing', 'wrong-account', 'wrong-scope'])(
    'hides detail data when the response echo is %s',
    async (mode) => {
      installFetch((url) => {
        if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
        if (mode === 'missing') return Response.json(second);
        return bound(second, mode === 'wrong-account' ? otherAdmin : { ...admin, scope_id: 'new' });
      });
      renderPage('/admin/users/second.user/details');
      expect(await screen.findByText(/could not be verified/)).toBeInTheDocument();
      expect(screen.queryByRole('heading', { name: 'second' })).not.toBeInTheDocument();
    },
  );

  it.each([401, 403, 409])(
    'revalidates the session after a bodyless %i response',
    async (status) => {
      let current: User | null = admin;
      let failing = false;
      let authReads = 0;
      installFetch((url) => {
        if (url.pathname.endsWith('/auth/me')) {
          authReads++;
          return current ? Response.json(current) : new Response(null, { status: 401 });
        }
        if (!failing) return bound({ ...emptyPage, items: [second] });
        if (status === 401) current = null;
        if (status === 403) current = reader;
        if (status === 409) current = { ...admin, scope_id: 'new-scope' };
        return new Response(null, { status });
      });
      renderPage('/admin/users');
      expect(await screen.findByRole('link', { name: 'View account second' })).toBeInTheDocument();
      failing = true;
      await userEvent.click(screen.getByRole('button', { name: 'Refresh' }));
      await waitFor(() => expect(authReads).toBeGreaterThan(1));
      expect(screen.queryByRole('link', { name: 'View account second' })).not.toBeInTheDocument();
      if (status === 401)
        expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
      if (status === 403)
        expect(await screen.findByText(/Administrator access is required/)).toBeInTheDocument();
      if (status === 409) expect(current?.scope_id).toBe('new-scope');
    },
  );

  it('rejects a held response after a same-account scope change', async () => {
    const nextActor = { ...admin, scope_id: 'new-scope' };
    let releaseOld!: (response: Response) => void;
    let releaseNew!: (response: Response) => void;
    installFetch((url, request) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      if (request.headers.get(expectedScope) === admin.scope_id)
        return new Promise<Response>((resolve) => {
          releaseOld = resolve;
        });
      expect(request.headers.get(expectedUser)).toBe(admin.id);
      expect(request.headers.get(expectedScope)).toBe(nextActor.scope_id);
      return new Promise<Response>((resolve) => {
        releaseNew = resolve;
      });
    });
    const client = renderPage('/admin/users');
    await waitFor(() => expect(releaseOld).toBeDefined());
    act(() => client.setQueryData(['auth', 'me'], nextActor));
    await waitFor(() => expect(releaseNew).toBeDefined());
    await act(async () => releaseOld(bound({ ...emptyPage, items: [second] })));
    expect(screen.queryByText('second')).not.toBeInTheDocument();
    await act(async () => releaseNew(bound(emptyPage, nextActor)));
    expect(await screen.findByText('No accounts are available on this page.')).toBeInTheDocument();
  });

  it('hides old rows during a reconnecting request and its failure', async () => {
    let reads = 0;
    let release!: (response: Response) => void;
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      reads++;
      if (reads === 1) return bound({ ...emptyPage, items: [second] });
      return new Promise<Response>((resolve) => {
        release = resolve;
      });
    });
    renderPage('/admin/users');
    expect(await screen.findByRole('link', { name: 'View account second' })).toBeInTheDocument();
    act(() => {
      onlineManager.setOnline(false);
      onlineManager.setOnline(true);
    });
    await waitFor(() => expect(release).toBeDefined());
    expect(screen.queryByText('second')).not.toBeInTheDocument();
    await act(async () => release(problem(503, 'INTERNAL_ERROR')));
    expect(await screen.findByText(/Could not load accounts/)).toBeInTheDocument();
    expect(screen.queryByText('second')).not.toBeInTheDocument();
  });

  it('does not show a detail response for a different account', async () => {
    installFetch((url) =>
      url.pathname.endsWith('/auth/me')
        ? Response.json(admin)
        : bound({ ...second, id: 'different-id', username: 'unexpected' }),
    );
    renderPage('/admin/users/second.user/details');
    expect(await screen.findByText(/Could not load accounts/)).toBeInTheDocument();
    expect(screen.queryByText('unexpected')).not.toBeInTheDocument();
  });

  it.each([
    ['zh-TW', '帳號', '檢視帳號 second'],
    ['zh-CN', '账号', '查看账号 second'],
  ])('renders %s translations', async (locale, heading, link) => {
    localStorage.setItem('gallerygo:locale', locale);
    installFetch((url) =>
      url.pathname.endsWith('/auth/me')
        ? Response.json(admin)
        : bound({ ...emptyPage, items: [second] }),
    );
    renderPage('/admin/users');
    expect(await screen.findByRole('heading', { name: heading, level: 1 })).toBeInTheDocument();
    expect(await screen.findByRole('link', { name: link })).toBeInTheDocument();
  });
});
