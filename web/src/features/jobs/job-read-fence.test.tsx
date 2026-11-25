import { onlineManager, QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from '../../app/App';
import type { components } from '../../lib/api/schema';

type User = components['schemas']['User'];
type Job = components['schemas']['Job'];

const admin: User = {
  id: 'admin-a',
  username: 'owner',
  role: 'admin',
  active: true,
  scope_id: 'scope-a',
  version: 1,
  download_original: false,
};
const job: Job = {
  id: 'b-only-job',
  kind: 'scan',
  resource_key: 'source:b-only',
  status: 'running',
  priority: 2,
  cancel_requested: false,
  attempts: 1,
  max_attempts: 3,
  next_run_at: '2026-11-11T01:02:03Z',
  total: 1,
  done: 0,
  succeeded: 0,
  failed: 0,
  skipped: 0,
  cancelled: 0,
  total_known: true,
  error_code: null,
  last_error: null,
  version: 1,
  created_at: '2026-11-10T01:02:03Z',
  updated_at: '2026-11-11T01:02:03Z',
  started_at: '2026-11-11T01:02:03Z',
  finished_at: null,
};
const empty = { items: [], next_cursor: null, has_more: false };
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

describe('job history read identity', () => {
  it.each(['missing', 'wrong-user', 'wrong-scope'] as const)(
    'does not render a job list when its response echo is %s',
    async (echo) => {
      installFetch((url) =>
        url.pathname.endsWith('/auth/me')
          ? Response.json(admin)
          : response({ ...empty, items: [job] }, echo),
      );
      renderPage('/admin/jobs');
      expect(await screen.findByRole('alert')).toBeInTheDocument();
      expect(screen.queryByText('b-only-job')).not.toBeInTheDocument();
    },
  );

  it.each(['job', 'attempts', 'issues'] as const)(
    'does not render any detail when the %s response lacks its echo',
    async (missing) => {
      installFetch((url) => {
        if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
        const part = url.pathname.endsWith('/attempts')
          ? 'attempts'
          : url.pathname.endsWith('/issues')
            ? 'issues'
            : 'job';
        return response(part === 'job' ? job : empty, part === missing ? 'missing' : 'valid');
      });
      renderPage('/admin/jobs/b-only-job/details');
      expect(await screen.findByRole('alert')).toBeInTheDocument();
      expect(screen.queryByText('source:b-only')).not.toBeInTheDocument();
    },
  );

  it.each([
    ['job', 'wrong-user'],
    ['job', 'wrong-scope'],
    ['attempts', 'wrong-user'],
    ['attempts', 'wrong-scope'],
    ['issues', 'wrong-user'],
    ['issues', 'wrong-scope'],
  ] as const)('hides the whole detail for a %s response with %s', async (badPart, echo) => {
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      const part = url.pathname.endsWith('/attempts')
        ? 'attempts'
        : url.pathname.endsWith('/issues')
          ? 'issues'
          : 'job';
      return response(part === 'job' ? job : empty, part === badPart ? echo : 'valid');
    });
    renderPage('/admin/jobs/b-only-job/details');
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(screen.queryByText('source:b-only')).not.toBeInTheDocument();
  });

  it('sends the expected actor on the job list request', async () => {
    const fetch = installFetch((url) =>
      url.pathname.endsWith('/auth/me')
        ? Response.json(admin)
        : response({ ...empty, items: [job] }, 'valid'),
    );
    renderPage('/admin/jobs');
    expect(await screen.findByRole('link', { name: 'View job b-only-job' })).toBeInTheDocument();
    const request = fetch.mock.calls
      .map(([item]) => item)
      .find((item) => new URL(item.url).pathname === '/api/v2/admin/jobs');
    expect(request?.headers.get(userHeader)).toBe(admin.id);
    expect(request?.headers.get(scopeHeader)).toBe(admin.scope_id);
  });

  it('sends the expected actor on every job history request', async () => {
    const fetch = installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      return response(
        url.pathname.endsWith('/attempts') || url.pathname.endsWith('/issues') ? empty : job,
        'valid',
      );
    });
    renderPage('/admin/jobs/b-only-job/details');
    expect(await screen.findByText('source:b-only')).toBeInTheDocument();
    const requests = fetch.mock.calls
      .map(([request]) => request)
      .filter((request) => new URL(request.url).pathname.includes('/admin/jobs'));
    expect(requests).toHaveLength(3);
    for (const request of requests) {
      expect(request.headers.get(userHeader)).toBe(admin.id);
      expect(request.headers.get(scopeHeader)).toBe(admin.scope_id);
    }
  });

  it('never requests jobs for an invalid actor or inactive administrator', async () => {
    const inactive = { ...admin, active: false };
    const fetch = installFetch((url) =>
      url.pathname.endsWith('/auth/me')
        ? Response.json(inactive)
        : response({ ...empty, items: [job] }, 'valid'),
    );
    renderPage('/admin/jobs');
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(fetch.mock.calls.map(([request]) => new URL(request.url).pathname)).toEqual([
      '/api/v2/auth/me',
    ]);
  });

  it('does not use an actor with an invalid header value', async () => {
    const fetch = installFetch((url) =>
      url.pathname.endsWith('/auth/me')
        ? Response.json({ ...admin, scope_id: 'contains space' })
        : response({ ...empty, items: [job] }, 'valid'),
    );
    renderPage('/admin/jobs');
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(fetch.mock.calls.map(([request]) => new URL(request.url).pathname)).toEqual([
      '/api/v2/auth/me',
    ]);
  });

  it('hides previous results during a failed refresh with stripped echoes', async () => {
    let held = false;
    let release!: (value: Response) => void;
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      return held
        ? new Promise<Response>((resolve) => {
            release = resolve;
          })
        : response({ ...empty, items: [job] }, 'valid');
    });
    renderPage('/admin/jobs');
    expect(await screen.findByRole('link', { name: 'View job b-only-job' })).toBeInTheDocument();
    held = true;
    await userEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    await waitFor(() => expect(release).toBeDefined());
    expect(screen.queryByText('b-only-job')).not.toBeInTheDocument();
    await act(async () => release(response({ ...empty, items: [job] }, 'missing')));
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(screen.queryByText('b-only-job')).not.toBeInTheDocument();
  });

  it('does not retain old job data during a reconnect failure', async () => {
    let reads = 0;
    let release!: (value: Response) => void;
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      reads++;
      if (reads === 1) return response({ ...empty, items: [job] }, 'valid');
      return new Promise<Response>((resolve) => {
        release = resolve;
      });
    });
    renderPage('/admin/jobs');
    expect(await screen.findByRole('link', { name: 'View job b-only-job' })).toBeInTheDocument();
    act(() => {
      onlineManager.setOnline(false);
      onlineManager.setOnline(true);
    });
    await waitFor(() => expect(release).toBeDefined());
    expect(screen.queryByText('b-only-job')).not.toBeInTheDocument();
    await act(async () => release(new Response(null, { status: 503 })));
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(screen.queryByText('b-only-job')).not.toBeInTheDocument();
  });

  it.each([401, 403, 409])(
    'revalidates the session after bodyless %i on a job read',
    async (status) => {
      let current: User | null = admin;
      let failing = false;
      let authReads = 0;
      installFetch((url) => {
        if (url.pathname.endsWith('/auth/me')) {
          authReads++;
          return current ? Response.json(current) : new Response(null, { status: 401 });
        }
        if (!failing) return response({ ...empty, items: [job] }, 'valid');
        if (status === 401) current = null;
        if (status === 403) current = { ...admin, role: 'user' };
        if (status === 409) current = { ...admin, scope_id: 'scope-next', version: 2 };
        return new Response(null, { status });
      });
      renderPage('/admin/jobs');
      expect(await screen.findByRole('link', { name: 'View job b-only-job' })).toBeInTheDocument();
      failing = true;
      await userEvent.click(screen.getByRole('button', { name: 'Refresh' }));
      await waitFor(() => expect(authReads).toBeGreaterThan(1));
      expect(screen.queryByText('b-only-job')).not.toBeInTheDocument();
      if (status === 401)
        expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
      if (status === 403)
        expect(await screen.findByText(/Administrator access is required/)).toBeInTheDocument();
      if (status === 409) expect(current?.scope_id).toBe('scope-next');
    },
  );

  it('hides every prior detail when reconnecting and a history part fails', async () => {
    let reconnecting = false;
    let release!: (value: Response) => void;
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      if (reconnecting && url.pathname.endsWith('/issues'))
        return new Promise<Response>((resolve) => {
          release = resolve;
        });
      const isJob = !url.pathname.endsWith('/attempts') && !url.pathname.endsWith('/issues');
      return response(isJob ? job : empty, 'valid');
    });
    renderPage('/admin/jobs/b-only-job/details');
    expect(await screen.findByText('source:b-only')).toBeInTheDocument();
    reconnecting = true;
    act(() => {
      onlineManager.setOnline(false);
      onlineManager.setOnline(true);
    });
    await waitFor(() => expect(release).toBeDefined());
    expect(screen.queryByText('source:b-only')).not.toBeInTheDocument();
    await act(async () => release(new Response(null, { status: 503 })));
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(screen.queryByText('source:b-only')).not.toBeInTheDocument();
  });

  it('drops a held job response after a same-account scope change', async () => {
    const next = { ...admin, scope_id: 'scope-next', version: 2 };
    let oldResponse!: (value: Response) => void;
    let newResponse!: (value: Response) => void;
    installFetch((url, request) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      if (request.headers.get(scopeHeader) === admin.scope_id)
        return new Promise<Response>((resolve) => {
          oldResponse = resolve;
        });
      expect(request.headers.get(scopeHeader)).toBe(next.scope_id);
      return new Promise<Response>((resolve) => {
        newResponse = resolve;
      });
    });
    const client = renderPage('/admin/jobs');
    await waitFor(() => expect(oldResponse).toBeDefined());
    act(() => client.setQueryData(['auth', 'me'], next));
    await waitFor(() => expect(newResponse).toBeDefined());
    await act(async () => oldResponse(response({ ...empty, items: [job] }, 'valid', admin)));
    expect(screen.queryByText('b-only-job')).not.toBeInTheDocument();
    await act(async () => newResponse(response(empty, 'valid', next)));
    expect(await screen.findByText('No jobs are available on this page.')).toBeInTheDocument();
  });
});
