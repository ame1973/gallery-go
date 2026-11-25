import { onlineManager, QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from '../../app/App';
import type { components } from '../../lib/api/schema';

type User = components['schemas']['User'];
type Session = components['schemas']['Session'];
type SessionPage = components['schemas']['SessionPage'];
const admin: User = {
  id: 'account-a',
  username: 'owner',
  role: 'admin',
  active: true,
  scope_id: 'scope-a',
  version: 1,
  download_original: false,
};
const session: Session = {
  id: 'session-b',
  created_at: '2026-11-26T00:00:00Z',
  last_seen_at: '2026-11-26T00:01:00Z',
  expires_at: '2026-11-27T00:00:00Z',
  user_agent: 'B private browser',
  ip_address: '192.0.2.42',
  current: false,
};
const page: SessionPage = { items: [session], next_cursor: null, has_more: false };
const userHeader = 'X-GalleryGo-Expected-User';
const scopeHeader = 'X-GalleryGo-Expected-Scope';
type Echo = 'valid' | 'missing' | 'wrong-user' | 'wrong-scope';

function response(echo: Echo, actor = admin, body = page) {
  if (echo === 'missing') return Response.json(body);
  return Response.json(body, {
    headers: {
      [userHeader]: echo === 'wrong-user' ? 'account-b' : actor.id,
      [scopeHeader]: echo === 'wrong-scope' ? 'scope-b' : actor.scope_id,
    },
  });
}

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={['/settings/security']}>
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

beforeEach(() => localStorage.clear());
afterEach(() => {
  onlineManager.setOnline(true);
  vi.unstubAllGlobals();
});

describe('session list read identity', () => {
  it.each(['missing', 'wrong-user', 'wrong-scope'] as const)(
    'hides session rows when the response echo is %s',
    async (echo) => {
      installHandler((url) =>
        url.pathname.endsWith('/auth/me') ? Response.json(admin) : response(echo),
      );
      renderPage();
      expect(await screen.findByRole('alert')).toBeInTheDocument();
      expect(screen.queryByText('B private browser')).not.toBeInTheDocument();
    },
  );

  it('sends the expected actor on the session list request', async () => {
    const fetch = installHandler((url) =>
      url.pathname.endsWith('/auth/me') ? Response.json(admin) : response('valid'),
    );
    renderPage();
    expect(await screen.findByText('B private browser')).toBeInTheDocument();
    const request = fetch.mock.calls
      .map(([item]) => item)
      .find((item) => new URL(item.url).pathname === '/api/v2/auth/sessions');
    expect(request?.headers.get(userHeader)).toBe(admin.id);
    expect(request?.headers.get(scopeHeader)).toBe(admin.scope_id);
  });

  it('hides stale rows and revocation dialog while refreshing', async () => {
    let held = false;
    let release!: (value: Response) => void;
    installHandler((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      return held
        ? new Promise<Response>((resolve) => {
            release = resolve;
          })
        : response('valid');
    });
    const client = renderPage();
    expect(await screen.findByText('B private browser')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Revoke session' }));
    expect(await screen.findByRole('alertdialog')).toBeInTheDocument();
    held = true;
    act(() => {
      void client.invalidateQueries({ predicate: (query) => query.queryKey.includes('sessions') });
    });
    await waitFor(() => expect(release).toBeDefined());
    expect(screen.queryByText('B private browser')).not.toBeInTheDocument();
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument();
    await act(async () => release(new Response(null, { status: 503 })));
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(screen.queryByText('B private browser')).not.toBeInTheDocument();
  });

  it.each([
    { label: 'inactive account', actor: { ...admin, active: false } },
    { label: 'invalid scope', actor: { ...admin, scope_id: 'invalid scope' } },
  ])('does not load sessions for an $label', async ({ actor }) => {
    const fetch = installHandler((url) =>
      url.pathname.endsWith('/auth/me') ? Response.json(actor) : response('valid'),
    );
    renderPage();
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(fetch.mock.calls.map(([request]) => new URL(request.url).pathname)).toEqual([
      '/api/v2/auth/me',
    ]);
  });

  it.each([401, 403, 409])(
    'revalidates identity after a bodyless %i session read',
    async (status) => {
      let failing = false;
      let current: User | null = admin;
      let authReads = 0;
      installHandler((url) => {
        if (url.pathname.endsWith('/auth/me')) {
          authReads++;
          return current ? Response.json(current) : new Response(null, { status: 401 });
        }
        if (!failing) return response('valid');
        if (status === 401) current = null;
        if (status === 409) current = { ...admin, scope_id: 'scope-next', version: 2 };
        return new Response(null, { status });
      });
      const client = renderPage();
      expect(await screen.findByText('B private browser')).toBeInTheDocument();
      failing = true;
      act(() => {
        void client.invalidateQueries({
          predicate: (query) => query.queryKey.includes('sessions'),
        });
      });
      await waitFor(() => expect(authReads).toBeGreaterThan(1));
      expect(screen.queryByText('B private browser')).not.toBeInTheDocument();
    },
  );

  it('does not reopen an old dialog after the account scope changes', async () => {
    const next = { ...admin, scope_id: 'scope-next', version: 2 };
    const nextPage = { ...page, items: [{ ...session, user_agent: 'Current browser' }] };
    let release!: (value: Response) => void;
    installHandler((url, request) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      if (request.headers.get(scopeHeader) === next.scope_id)
        return new Promise<Response>((resolve) => {
          release = resolve;
        });
      return response('valid');
    });
    const client = renderPage();
    expect(await screen.findByText('B private browser')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Revoke session' }));
    expect(await screen.findByRole('alertdialog')).toBeInTheDocument();
    act(() => client.setQueryData(['auth', 'me'], next));
    await waitFor(() => expect(release).toBeDefined());
    expect(screen.queryByText('B private browser')).not.toBeInTheDocument();
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument();
    await act(async () => release(response('valid', next, nextPage)));
    expect(await screen.findByText('Current browser')).toBeInTheDocument();
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument();
  });

  it('hides session rows during a reconnect failure', async () => {
    let reads = 0;
    let release!: (value: Response) => void;
    installHandler((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      reads++;
      return reads === 1
        ? response('valid')
        : new Promise<Response>((resolve) => {
            release = resolve;
          });
    });
    renderPage();
    expect(await screen.findByText('B private browser')).toBeInTheDocument();
    act(() => {
      onlineManager.setOnline(false);
      onlineManager.setOnline(true);
    });
    await waitFor(() => expect(release).toBeDefined());
    expect(screen.queryByText('B private browser')).not.toBeInTheDocument();
    await act(async () => release(new Response(null, { status: 503 })));
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(screen.queryByText('B private browser')).not.toBeInTheDocument();
  });

  it('closes the dialog during an uncertain revoke and refreshes without retrying the mutation', async () => {
    let release!: (value: Response) => void;
    let reads = 0;
    const fetch = installHandler((url, request) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      if (url.pathname.endsWith('/auth/csrf')) return Response.json({ token: 'fixture-csrf' });
      if (request.method === 'DELETE')
        return new Promise<Response>((resolve) => {
          release = resolve;
        });
      reads++;
      return response('valid');
    });
    renderPage();
    expect(await screen.findByText('B private browser')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Revoke session' }));
    const dialog = await screen.findByRole('alertdialog');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Revoke session' }));
    await waitFor(() => expect(release).toBeDefined());
    expect(screen.queryByText('B private browser')).not.toBeInTheDocument();
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument();
    await act(async () => release(new Response(null, { status: 503 })));
    await waitFor(() => expect(reads).toBeGreaterThan(1));
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(fetch.mock.calls.filter(([request]) => request.method === 'DELETE')).toHaveLength(1);
  });

  it('allows an ordinary active account to read its own sessions', async () => {
    const reader = { ...admin, role: 'user' as const };
    const fetch = installHandler((url) =>
      url.pathname.endsWith('/auth/me') ? Response.json(reader) : response('valid', reader),
    );
    renderPage();
    expect(await screen.findByText('B private browser')).toBeInTheDocument();
    const request = fetch.mock.calls
      .map(([item]) => item)
      .find((item) => new URL(item.url).pathname === '/api/v2/auth/sessions');
    expect(request?.headers.get(userHeader)).toBe(reader.id);
    expect(request?.headers.get(scopeHeader)).toBe(reader.scope_id);
  });

  it('hides every old page if a later page has no matching echoes', async () => {
    const first: SessionPage = { ...page, next_cursor: 'next', has_more: true };
    let release!: (value: Response) => void;
    installHandler((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      if (url.searchParams.has('cursor'))
        return new Promise<Response>((resolve) => {
          release = resolve;
        });
      return response('valid', admin, first);
    });
    renderPage();
    expect(await screen.findByText('B private browser')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Load more sessions' }));
    await waitFor(() => expect(release).toBeDefined());
    expect(screen.queryByText('B private browser')).not.toBeInTheDocument();
    await act(async () => release(response('missing')));
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(screen.queryByText('B private browser')).not.toBeInTheDocument();
  });

  it('revalidates identity after a bodyless 401 from revocation', async () => {
    let revoked = false;
    const fetch = installHandler((url, request) => {
      if (url.pathname.endsWith('/auth/me'))
        return revoked ? new Response(null, { status: 401 }) : Response.json(admin);
      if (url.pathname.endsWith('/auth/csrf')) return Response.json({ token: 'fixture-csrf' });
      if (request.method === 'DELETE') {
        revoked = true;
        return new Response(null, { status: 401 });
      }
      return revoked ? new Response(null, { status: 401 }) : response('valid');
    });
    renderPage();
    expect(await screen.findByText('B private browser')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Revoke session' }));
    const dialog = await screen.findByRole('alertdialog');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Revoke session' }));
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
    expect(screen.queryByText('B private browser')).not.toBeInTheDocument();
    expect(fetch.mock.calls.filter(([request]) => request.method === 'DELETE')).toHaveLength(1);
  });

  it('retains revocation success after refreshing the session list', async () => {
    let revoked = false;
    const fetch = installHandler((url, request) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      if (url.pathname.endsWith('/auth/csrf')) return Response.json({ token: 'fixture-csrf' });
      if (request.method === 'DELETE') {
        revoked = true;
        return new Response(null, { status: 204 });
      }
      return response('valid', admin, revoked ? { ...page, items: [] } : page);
    });
    renderPage();
    expect(await screen.findByText('B private browser')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Revoke session' }));
    const dialog = await screen.findByRole('alertdialog');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Revoke session' }));
    expect(await screen.findByText('Session revoked.')).toBeInTheDocument();
    expect(screen.queryByText('B private browser')).not.toBeInTheDocument();
    expect(fetch.mock.calls.filter(([request]) => request.method === 'DELETE')).toHaveLength(1);
  });
});
