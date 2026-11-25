import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from '../../app/App';
import type { components } from '../../lib/api/schema';

type User = components['schemas']['User'];
type Job = components['schemas']['Job'];
type Attempt = components['schemas']['JobAttempt'];
type Issue = components['schemas']['JobIssue'];

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
const other: User = { ...admin, id: 'admin-b', scope_id: 'scope-b', username: 'other' };
const job: Job = {
  id: 'job.with.dot',
  kind: 'scan',
  resource_key: 'source:historical',
  status: 'running',
  priority: 2,
  cancel_requested: true,
  attempts: 2,
  max_attempts: 3,
  next_run_at: '2026-11-11T01:02:03Z',
  total: 10,
  done: 4,
  succeeded: 1,
  failed: 1,
  skipped: 1,
  cancelled: 1,
  total_known: false,
  error_code: 'LEASE_EXPIRED',
  last_error: 'The previous worker lease expired.',
  version: 4,
  created_at: '2026-11-10T01:02:03Z',
  updated_at: '2026-11-11T01:02:03Z',
  started_at: '2026-11-11T01:02:03Z',
  finished_at: null,
};
const attempt: Attempt = {
  attempt: 1,
  status: 'failed',
  started_at: '2026-11-10T01:02:03Z',
  finished_at: '2026-11-10T01:03:03Z',
  error_code: 'INVALID_MEDIA',
  message: 'The input media could not be validated.',
  total: 3,
  done: 3,
  succeeded: 1,
  failed: 1,
  skipped: 0,
  cancelled: 1,
};
const issue: Issue = {
  id: 'issue-one',
  attempt: 1,
  source_id: 'historical-source',
  source_config_version: 5,
  generation_id: null,
  relative_path: 'album/photo.png',
  error_code: 'INVALID_MEDIA',
  message: 'The input media could not be validated.',
  created_at: '2026-11-10T01:02:03Z',
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

function problem(status: number, code: string) {
  return Response.json(
    { code, status, type: 'urn:test', title: 'Test', request_id: 'id', detail: '/private/secret' },
    { status },
  );
}

function installFetch(handler: (url: URL, request: Request) => Response | Promise<Response>) {
  const fetch = vi.fn(async (request: Request) => {
    const url = new URL(request.url);
    const result = await handler(url, request);
    if (!result.ok || !url.pathname.startsWith('/api/v2/admin/jobs')) return result;
    const user = request.headers.get('X-GalleryGo-Expected-User');
    const scope = request.headers.get('X-GalleryGo-Expected-Scope');
    if (!user || !scope) return result;
    const headers = new Headers(result.headers);
    headers.set('X-GalleryGo-Expected-User', user);
    headers.set('X-GalleryGo-Expected-Scope', scope);
    return new Response(result.body, { status: result.status, headers });
  });
  vi.stubGlobal('fetch', fetch);
  return fetch;
}

function history(url: URL) {
  if (url.pathname === '/api/v2/admin/jobs/job.with.dot') return Response.json(job);
  if (url.pathname.endsWith('/attempts')) {
    return Response.json(
      url.searchParams.has('cursor')
        ? { ...emptyPage, items: [{ ...attempt, attempt: 2, status: 'succeeded' }] }
        : { items: [attempt], next_cursor: 'attempt-next', has_more: true },
    );
  }
  if (url.pathname.endsWith('/issues')) {
    return Response.json(
      url.searchParams.has('cursor')
        ? { ...emptyPage, items: [{ ...issue, id: 'issue-two', relative_path: null }] }
        : { items: [issue], next_cursor: 'issue-next', has_more: true },
    );
  }
  return problem(404, 'NOT_FOUND');
}

beforeEach(() => localStorage.clear());
afterEach(() => vi.unstubAllGlobals());

describe('administrative job history', () => {
  it('provides job navigation to the authenticated administrator', async () => {
    installFetch((url) =>
      url.pathname.endsWith('/auth/me') ? Response.json(admin) : Response.json(emptyPage),
    );
    renderPage('/settings/security');
    const navigation = await screen.findByRole('navigation', { name: 'Gallery navigation' });
    expect(navigation).toHaveTextContent('Jobs');
    expect(screen.getByRole('link', { name: 'Jobs' })).toHaveAttribute('href', '/admin/jobs');
  });

  it('does not offer administrative navigation to an ordinary account', async () => {
    installFetch((url) =>
      url.pathname.endsWith('/auth/me') ? Response.json(reader) : Response.json(emptyPage),
    );
    renderPage('/settings/security');
    await screen.findByRole('navigation', { name: 'Gallery navigation' });
    expect(screen.queryByRole('link', { name: 'Jobs' })).not.toBeInTheDocument();
  });

  it.each(['/admin/jobs', '/admin/jobs/job.with.dot/details'])(
    'requires a real session before requesting %s',
    async (path) => {
      const fetch = installFetch(() => problem(401, 'UNAUTHENTICATED'));
      renderPage(path);
      expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
      expect(fetch.mock.calls.map(([request]) => new URL(request.url).pathname)).toEqual([
        '/api/v2/auth/me',
      ]);
    },
  );

  it.each(['/admin/jobs', '/admin/jobs/job.with.dot/details'])(
    'denies an ordinary user without requesting %s',
    async (path) => {
      const fetch = installFetch((url) =>
        url.pathname.endsWith('/auth/me') ? Response.json(reader) : problem(403, 'ADMIN_REQUIRED'),
      );
      renderPage(path);
      expect(
        await screen.findByText('Administrator access is required to view job history.'),
      ).toBeInTheDocument();
      expect(fetch.mock.calls.map(([request]) => new URL(request.url).pathname)).toEqual([
        '/api/v2/auth/me',
      ]);
      expect(screen.queryByRole('link', { name: 'Jobs' })).not.toBeInTheDocument();
    },
  );

  it('shows truthful cumulative progress and resets the scoped cursor on filter changes', async () => {
    const requests: URL[] = [];
    installFetch((url) => {
      requests.push(url);
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      if (url.pathname === '/api/v2/admin/jobs') {
        if (url.searchParams.get('status') === 'failed') {
          return Response.json({
            ...emptyPage,
            items: [
              {
                ...job,
                id: 'failed-job',
                status: 'failed',
                cancel_requested: false,
                total_known: true,
              },
            ],
          });
        }
        if (url.searchParams.has('cursor'))
          return Response.json({ ...emptyPage, items: [{ ...job, id: 'job-two' }] });
        return Response.json({ items: [job], next_cursor: 'jobs-next', has_more: true });
      }
      return problem(404, 'NOT_FOUND');
    });
    renderPage('/admin/jobs');
    expect(await screen.findByRole('link', { name: 'View job job.with.dot' })).toHaveAttribute(
      'href',
      '/admin/jobs/job.with.dot/details',
    );
    expect(screen.getByText('4 of 10 discovered work units finished')).toBeInTheDocument();
    expect(screen.getByText('The total is not known yet and may increase.')).toBeInTheDocument();
    expect(
      screen.getByText('Cancellation has been requested; this job is still running.'),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/Finished includes succeeded, failed, skipped and cancelled/),
    ).toBeInTheDocument();
    expect(screen.queryByText(/%/)).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole('link', { name: 'Next page' }));
    expect(await screen.findByRole('link', { name: 'View job job-two' })).toBeInTheDocument();
    expect(screen.getByRole('main')).toHaveFocus();
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Status' }), 'failed');
    expect(await screen.findByRole('link', { name: 'View job failed-job' })).toBeInTheDocument();
    const last = requests.filter((url) => url.pathname === '/api/v2/admin/jobs').at(-1)!;
    expect(last.searchParams.get('status')).toBe('failed');
    expect(last.searchParams.get('cursor')).toBeNull();
    expect(last.searchParams.get('limit')).toBe('30');
    expect(requests.some((url) => url.searchParams.get('cursor') === 'jobs-next')).toBe(true);
  });

  it('separates per-attempt counters and diagnostic pages while preserving the filtered return link', async () => {
    const calls: URL[] = [];
    installFetch((url) => {
      calls.push(url);
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      if (url.pathname === '/api/v2/admin/jobs')
        return Response.json({ ...emptyPage, items: [job] });
      return history(url);
    });
    renderPage('/admin/jobs?status=running&cursor=list-cursor');
    await userEvent.click(await screen.findByRole('link', { name: 'View job job.with.dot' }));
    expect(await screen.findByRole('heading', { name: /Scan job.with.dot/ })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to jobs' })).toHaveAttribute(
      'href',
      '/admin/jobs?status=running&cursor=list-cursor',
    );
    expect(
      screen.getByText(
        'Counts include all attempts. Finished includes succeeded, failed, skipped and cancelled work; it does not mean success.',
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText('Each attempt shows its own work units, not cumulative job counts.'),
    ).toBeInTheDocument();
    expect(screen.getByText('album/photo.png')).toBeInTheDocument();
    expect(screen.getByText('historical-source')).toBeInTheDocument();
    expect(
      screen.getByText(
        'These are historical diagnostics, not a count of failed work or proof that a source is currently available.',
      ),
    ).toBeInTheDocument();
    expect(screen.getAllByText('The input media could not be validated.')).toHaveLength(2);
    expect(screen.getAllByText(/UTC$/).length).toBeGreaterThan(0);
    expect(screen.queryByText('/private/secret')).not.toBeInTheDocument();
    await userEvent.click(
      within(screen.getByRole('navigation', { name: 'Attempt pages' })).getByRole('link', {
        name: 'Next page',
      }),
    );
    expect(await screen.findByRole('heading', { name: 'Attempt 2' })).toBeInTheDocument();
    expect(screen.getByRole('main')).toHaveFocus();
    expect(screen.getByText('album/photo.png')).toBeInTheDocument();
    await userEvent.click(
      within(screen.getByRole('navigation', { name: 'Diagnostic pages' })).getByRole('link', {
        name: 'Next page',
      }),
    );
    await waitFor(() => expect(screen.queryByText('album/photo.png')).not.toBeInTheDocument());
    expect(await screen.findByRole('heading', { name: 'Attempt 2' })).toBeInTheDocument();
    expect(screen.getByText('historical-source')).toBeInTheDocument();
    const firstAttempts = within(
      screen.getByRole('navigation', { name: 'Attempt pages' }),
    ).getByRole('link', { name: 'First page' });
    expect(firstAttempts).toHaveAttribute(
      'href',
      '/admin/jobs/job.with.dot/details?issue_cursor=issue-next',
    );
    expect(screen.getByRole('link', { name: 'Back to jobs' })).toHaveAttribute(
      'href',
      '/admin/jobs?status=running&cursor=list-cursor',
    );
    expect(
      calls.some(
        (url) =>
          url.pathname.endsWith('/attempts') && url.searchParams.get('cursor') === 'attempt-next',
      ),
    ).toBe(true);
    expect(
      calls.some(
        (url) =>
          url.pathname.endsWith('/issues') && url.searchParams.get('cursor') === 'issue-next',
      ),
    ).toBe(true);
    expect(
      calls
        .filter((url) => /\/(attempts|issues)$/.test(url.pathname))
        .every((url) => url.searchParams.get('limit') === '30'),
    ).toBe(true);
  });

  it.each([
    '/admin/jobs?status=',
    '/admin/jobs?status=unknown',
    '/admin/jobs?status=queued&status=failed',
    '/admin/jobs?cursor=',
    '/admin/jobs?cursor=one&cursor=two',
    '/admin/jobs?sort=latest',
    `/admin/jobs?cursor=${'x'.repeat(513)}`,
    '/admin/jobs/job.with.dot/details?cursor=one',
    '/admin/jobs/job.with.dot/details?attempt_cursor=',
    '/admin/jobs/job.with.dot/details?issue_cursor=one&issue_cursor=two',
    `/admin/jobs/job.with.dot/details?attempt_cursor=${'x'.repeat(513)}`,
  ])('rejects unsupported or ambiguous parameters before a job request: %s', async (path) => {
    const fetch = installFetch((url) =>
      url.pathname.endsWith('/auth/me') ? Response.json(admin) : problem(400, 'INVALID_CURSOR'),
    );
    renderPage(path);
    expect(
      await screen.findByText('This page link is invalid or expired. Return to the first page.'),
    ).toBeInTheDocument();
    expect(fetch.mock.calls.map(([request]) => new URL(request.url).pathname)).toEqual([
      '/api/v2/auth/me',
    ]);
  });

  it.each([
    'https://untrusted.example/admin/jobs',
    '/admin/jobs#private',
    '/admin/jobs?status=queued&status=failed',
    '/admin/jobs?sort=unknown',
    '/admin/jobs/extra',
  ])('discards an unsafe job-list return path: %s', async (from) => {
    installFetch((url) =>
      url.pathname.endsWith('/auth/me') ? Response.json(admin) : history(url),
    );
    renderPage({ pathname: '/admin/jobs/job.with.dot/details', state: { from } });
    await screen.findByRole('heading', { name: /Scan job.with.dot/ });
    expect(screen.getByRole('link', { name: 'Back to jobs' })).toHaveAttribute(
      'href',
      '/admin/jobs',
    );
  });

  it('hides every prior detail during a held refresh and after a failed history read', async () => {
    let mode: 'ok' | 'hold' = 'ok';
    let release: (() => void) | undefined;
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      if (url.pathname.endsWith('/issues') && mode === 'hold') {
        return new Promise<Response>((resolve) => {
          release = () => resolve(problem(503, 'INTERNAL_ERROR'));
        });
      }
      return history(url);
    });
    renderPage('/admin/jobs/job.with.dot/details');
    expect(await screen.findByText('album/photo.png')).toBeInTheDocument();
    mode = 'hold';
    await userEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    await waitFor(() => expect(release).toBeDefined());
    expect(screen.getByText('Loading job history…')).toBeInTheDocument();
    expect(screen.queryByText('album/photo.png')).not.toBeInTheDocument();
    expect(screen.queryByText('source:historical')).not.toBeInTheDocument();
    await act(async () => release!());
    expect(
      await screen.findByText('Could not load job history. Check your connection and try again.'),
    ).toBeInTheDocument();
    expect(screen.queryByText('album/photo.png')).not.toBeInTheDocument();
    expect(screen.queryByText('/private/secret')).not.toBeInTheDocument();
    mode = 'ok';
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByText('album/photo.png')).toBeInTheDocument();
    expect(screen.getByRole('main')).toHaveFocus();
  });

  it.each(['job', 'attempts', 'issues'])(
    'hides all detail if the %s read loses administrator access',
    async (which) => {
      let denied = false;
      installFetch((url) => {
        if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
        const match =
          which === 'job'
            ? url.pathname.endsWith('/job.with.dot')
            : url.pathname.endsWith(`/${which}`);
        return denied && match ? problem(403, 'ADMIN_REQUIRED') : history(url);
      });
      renderPage('/admin/jobs/job.with.dot/details');
      await screen.findByText('album/photo.png');
      denied = true;
      await userEvent.click(screen.getByRole('button', { name: 'Refresh' }));
      expect(
        await screen.findByText('Administrator access is required to view job history.'),
      ).toBeInTheDocument();
      expect(screen.queryByText('album/photo.png')).not.toBeInTheDocument();
      expect(screen.queryByText('source:historical')).not.toBeInTheDocument();
      expect(screen.queryByText('/private/secret')).not.toBeInTheDocument();
    },
  );

  it('revalidates the session and hides history after a 401', async () => {
    let signedIn = true;
    let expire = false;
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me'))
        return signedIn ? Response.json(admin) : problem(401, 'UNAUTHENTICATED');
      if (expire) {
        signedIn = false;
        return problem(401, 'UNAUTHENTICATED');
      }
      return history(url);
    });
    renderPage('/admin/jobs/job.with.dot/details');
    await screen.findByText('album/photo.png');
    expire = true;
    await userEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
    expect(screen.queryByText('album/photo.png')).not.toBeInTheDocument();
  });

  it('keeps administrator accounts and scopes separate even when an old read completes late', async () => {
    let current = admin;
    let hold = true;
    let release: (() => void) | undefined;
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(current);
      if (url.pathname === '/api/v2/admin/jobs') {
        const id = `${current.id}-job`;
        if (hold && current.id === admin.id) {
          return new Promise<Response>((resolve) => {
            release = () => resolve(Response.json({ ...emptyPage, items: [{ ...job, id }] }));
          });
        }
        return Response.json({ ...emptyPage, items: [{ ...job, id }] });
      }
      return problem(404, 'NOT_FOUND');
    });
    const client = renderPage('/admin/jobs');
    await waitFor(() => expect(release).toBeDefined());
    current = other;
    act(() => client.setQueryData(['auth', 'me'], other));
    expect(await screen.findByRole('link', { name: 'View job admin-b-job' })).toBeInTheDocument();
    await act(async () => release!());
    expect(screen.getByRole('link', { name: 'View job admin-b-job' })).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'View job admin-a-job' })).not.toBeInTheDocument();
    hold = false;
    current = admin;
    act(() => client.setQueryData(['auth', 'me'], admin));
    expect(await screen.findByRole('link', { name: 'View job admin-a-job' })).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'View job admin-b-job' })).not.toBeInTheDocument();
    current = reader;
    act(() => client.setQueryData(['auth', 'me'], reader));
    expect(
      await screen.findByText('Administrator access is required to view job history.'),
    ).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /View job/ })).not.toBeInTheDocument();
  });

  it.each([
    { ...job, id: '\ud800' },
    { ...job, done: 3 },
    { ...job, total: Number.MAX_SAFE_INTEGER + 1 },
    { ...job, total_known: 'yes' },
    { ...job, status: 'invented' },
    { ...job, max_attempts: 0 },
    { ...job, max_attempts: 101 },
  ])('does not display a malformed or unrepresentable job', async (bad) => {
    installFetch((url) =>
      url.pathname.endsWith('/auth/me')
        ? Response.json(admin)
        : Response.json({ ...emptyPage, items: [bad] }),
    );
    renderPage('/admin/jobs');
    expect(
      await screen.findByText('Could not load job history. Check your connection and try again.'),
    ).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /View job/ })).not.toBeInTheDocument();
  });

  it.each([
    ['queued attempt', 'attempts', { ...attempt, status: 'queued' }],
    ['attempt beyond the retry budget', 'attempts', { ...attempt, attempt: 101 }],
    ['inconsistent attempt counts', 'attempts', { ...attempt, done: 0 }],
    ['zero issue attempt', 'issues', { ...issue, attempt: 0 }],
    [
      'unrepresentable source version',
      'issues',
      { ...issue, source_config_version: Number.MAX_SAFE_INTEGER + 1 },
    ],
  ])('hides all history for a malformed %s', async (_description, collection, item) => {
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      if (url.pathname.endsWith(`/${collection}`))
        return Response.json({ ...emptyPage, items: [item] });
      return history(url);
    });
    renderPage('/admin/jobs/job.with.dot/details');
    expect(
      await screen.findByText('Could not load job history. Check your connection and try again.'),
    ).toBeInTheDocument();
    expect(screen.queryByText('source:historical')).not.toBeInTheDocument();
    expect(screen.queryByText('album/photo.png')).not.toBeInTheDocument();
  });

  it('localizes job labels and known diagnostics without rendering raw server messages', async () => {
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(admin);
      if (url.pathname.endsWith('/issues'))
        return Response.json({ ...emptyPage, items: [{ ...issue, message: '/private/secret' }] });
      return history(url);
    });
    renderPage('/admin/jobs/job.with.dot/details');
    await screen.findByText('album/photo.png');
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Language' }), 'zh-TW');
    expect(screen.getByRole('heading', { name: '任務詳細資料' })).toBeInTheDocument();
    expect(screen.getAllByText('無法驗證輸入的媒體。')).toHaveLength(2);
    await userEvent.selectOptions(screen.getByRole('combobox', { name: '語言' }), 'zh-CN');
    expect(screen.getByRole('heading', { name: '任务详情' })).toBeInTheDocument();
    expect(screen.getAllByText('无法验证输入的媒体。')).toHaveLength(2);
    expect(screen.queryByText('/private/secret')).not.toBeInTheDocument();
  });
});
