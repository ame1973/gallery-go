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
const person: components['schemas']['Person'] = {
  id: 'person-one',
  name: 'Same name',
  source_id: 'source-one',
  album_count: 2,
  cover_media_id: 'media-one',
  version: 1,
};
const album: components['schemas']['Album'] = {
  id: 'album-one',
  title: 'First album',
  library_id: 'library',
  cover_media_id: 'media-one',
  media_count: 1,
  rating_avg: null,
  view_count: 0,
  indexed_at: '2026-11-01T00:00:00Z',
  version: 1,
};
const media: components['schemas']['Media'] = {
  id: 'media-one',
  album_id: 'album-two',
  kind: 'image',
  codec: null,
  file_name: 'photo.png',
  width: 1,
  height: 1,
  duration_ms: null,
  animated: false,
  frame_count: 1,
  loop_count: 1,
  orientation: null,
  capture_at: null,
  revision: 'revision',
  offline_capable: false,
};
const variant: components['schemas']['Variant'] = {
  id: 'variant-one',
  revision_id: 'revision',
  preset_key: 'thumb_320',
  preset_version: 1,
  mime: 'image/png',
  size_bytes: 64,
  sha256: 'a'.repeat(64),
  width: 1,
  height: 1,
  duration_ms: null,
  content_url: 'https://untrusted.example/private',
};

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
    { code, status, type: 'urn:test', title: 'Test', request_id: 'id', detail: 'private path' },
    { status },
  );
}

function installFetch(handler: (url: URL) => Response | Promise<Response>) {
  const fetch = vi.fn(async (request: Request) => handler(new URL(request.url)));
  vi.stubGlobal('fetch', fetch);
  return fetch;
}

const emptyPage = { items: [], next_cursor: null, has_more: false };
const invalidPage =
  'This page link is invalid or expired, or access has changed. Return to the first page.';

afterEach(() => vi.unstubAllGlobals());

describe('people browsing', () => {
  it.each(['/people', '/people/person-one'])('requires a session for %s', async (path) => {
    const fetch = installFetch(() => problem(401, 'UNAUTHENTICATED'));
    renderPage(path);
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
    expect(fetch.mock.calls.map(([request]) => new URL(request.url).pathname)).toEqual([
      '/api/v2/auth/me',
    ]);
  });

  it('shows a translated empty state without disclosing server diagnostics', async () => {
    installFetch((url) =>
      url.pathname.endsWith('/auth/me') ? Response.json(user) : Response.json(emptyPage),
    );
    renderPage('/people');
    expect(await screen.findByText('No people are available on this page.')).toBeInTheDocument();
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Language' }), 'zh-TW');
    expect(screen.getByRole('heading', { name: '人物' })).toBeInTheDocument();
    expect(screen.getByText('此頁面目前沒有可用的人物。')).toBeInTheDocument();
    await userEvent.selectOptions(screen.getByRole('combobox', { name: '語言' }), 'zh-CN');
    expect(screen.getByRole('heading', { name: '人物' })).toBeInTheDocument();
    expect(screen.getByText('此页面目前没有可用的人物。')).toBeInTheDocument();
    expect(screen.queryByText('private path')).not.toBeInTheDocument();
  });

  it('keeps equal names distinct and requests only bounded protected previews', async () => {
    const calls: URL[] = [];
    installFetch((url) => {
      calls.push(url);
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname.endsWith('/variants'))
        return Response.json({ ...emptyPage, items: [variant] });
      if (url.pathname === '/api/v2/people') {
        return Response.json(
          url.searchParams.has('cursor')
            ? { ...emptyPage, items: [{ ...person, id: 'person-three', name: 'Another person' }] }
            : {
                items: [person, { ...person, id: 'person-two', cover_media_id: null }],
                next_cursor: 'people-next',
                has_more: true,
              },
        );
      }
      return problem(404, 'NOT_FOUND');
    });
    renderPage('/people');
    const cards = await screen.findAllByRole('link', { name: /Same name/ });
    expect(cards).toHaveLength(2);
    expect(cards[0]).toHaveAttribute('href', '/people/person-one');
    expect(cards[1]).toHaveAttribute('href', '/people/person-two');
    expect(screen.getAllByText('2 albums')).toHaveLength(2);
    await waitFor(() =>
      expect(cards[0]!.querySelector('img')).toHaveAttribute(
        'src',
        '/api/v2/media/media-one/variants/variant-one/content',
      ),
    );
    expect(calls.find((url) => url.pathname === '/api/v2/people')?.searchParams.get('limit')).toBe(
      '30',
    );
    expect(calls.every((url) => url.origin !== 'https://untrusted.example')).toBe(true);
    expect(calls.every((url) => !url.pathname.endsWith('/original'))).toBe(true);
    await userEvent.click(screen.getByRole('link', { name: 'Next page' }));
    expect(await screen.findByRole('link', { name: /Another person/ })).toBeInTheDocument();
    expect(screen.getByRole('main')).toHaveFocus();
    expect(calls.find((url) => url.searchParams.get('cursor') === 'people-next')).toBeDefined();
  });

  it('preserves both people and person-album cursors through album-media navigation', async () => {
    const calls: URL[] = [];
    installFetch((url) => {
      calls.push(url);
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname === '/api/v2/people')
        return Response.json({ ...emptyPage, items: [person] });
      if (url.pathname === '/api/v2/people/person-one') return Response.json(person);
      if (url.pathname === '/api/v2/people/person-one/albums') {
        return Response.json(
          url.searchParams.has('cursor')
            ? { ...emptyPage, items: [{ ...album, id: 'album-two', title: 'Second album' }] }
            : { items: [album], next_cursor: 'album-next', has_more: true },
        );
      }
      if (url.pathname === '/api/v2/albums/album-two')
        return Response.json({ ...album, id: 'album-two', title: 'Second album' });
      if (url.pathname === '/api/v2/albums/album-two/rating')
        return Response.json(
          { album_id: 'album-two', rating: null, updated_at: null, version: 1 },
          { headers: { ETag: '"rating-test"' } },
        );
      if (url.pathname === '/api/v2/albums/album-two/media')
        return Response.json(
          url.searchParams.has('cursor')
            ? { ...emptyPage, items: [media] }
            : { items: [media], next_cursor: 'media-next', has_more: true },
        );
      if (url.pathname.endsWith('/variants')) return Response.json(emptyPage);
      return problem(404, 'NOT_FOUND');
    });
    renderPage('/people?cursor=people-cursor');
    await userEvent.click(await screen.findByRole('link', { name: /Same name/ }));
    expect(await screen.findByRole('heading', { name: 'Same name' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to people' })).toHaveAttribute(
      'href',
      '/people?cursor=people-cursor',
    );
    expect(
      calls.find((url) => url.pathname.endsWith('/person-one/albums'))?.searchParams.get('limit'),
    ).toBe('30');
    await userEvent.click(screen.getByRole('link', { name: 'Next page' }));
    await userEvent.click(await screen.findByRole('link', { name: /Second album/ }));
    expect(await screen.findByRole('heading', { name: 'Second album' })).toBeInTheDocument();
    await userEvent.click(await screen.findByRole('link', { name: 'Next page' }));
    expect(await screen.findByText('photo.png')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('link', { name: 'Back to person' }));
    expect(await screen.findByRole('link', { name: /Second album/ })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to people' })).toHaveAttribute(
      'href',
      '/people?cursor=people-cursor',
    );
    await userEvent.click(screen.getByRole('link', { name: 'Back to people' }));
    expect(await screen.findByRole('link', { name: /Same name/ })).toBeInTheDocument();
    expect(
      calls.some(
        (url) =>
          url.pathname.endsWith('/person-one/albums') &&
          url.searchParams.get('cursor') === 'album-next',
      ),
    ).toBe(true);
    expect(
      calls.filter(
        (url) =>
          url.pathname === '/api/v2/people' && url.searchParams.get('cursor') === 'people-cursor',
      ).length,
    ).toBeGreaterThanOrEqual(2);
  });

  it.each([
    '/people?cursor=',
    '/people?cursor=one&cursor=two',
    '/people?sort=name',
    `/people?cursor=${'x'.repeat(513)}`,
    '/people/person-one?cursor=',
    '/people/person-one?cursor=one&cursor=two',
    '/people/person-one?sort=name',
    `/people/person-one?cursor=${'x'.repeat(513)}`,
  ])('rejects ambiguous or unsupported page parameters: %s', async (path) => {
    const fetch = installFetch((url) =>
      url.pathname.endsWith('/auth/me') ? Response.json(user) : problem(400, 'INVALID_CURSOR'),
    );
    renderPage(path);
    expect(await screen.findByText(invalidPage)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'First page' })).toHaveAttribute(
      'href',
      path.startsWith('/people/person-one') ? '/people/person-one' : '/people',
    );
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1));
  });

  it('accepts the documented 512-character cursor boundary', async () => {
    const cursor = 'x'.repeat(512);
    const fetch = installFetch((url) =>
      url.pathname.endsWith('/auth/me') ? Response.json(user) : Response.json(emptyPage),
    );
    renderPage(`/people?cursor=${cursor}`);
    expect(await screen.findByText('No people are available on this page.')).toBeInTheDocument();
    expect(
      fetch.mock.calls.some(
        ([request]) => new URL(request.url).searchParams.get('cursor') === cursor,
      ),
    ).toBe(true);
  });

  it('recovers from a server-rejected scoped cursor', async () => {
    installFetch((url) =>
      url.pathname.endsWith('/auth/me') ? Response.json(user) : problem(400, 'INVALID_CURSOR'),
    );
    renderPage('/people?cursor=old');
    expect(await screen.findByText(invalidPage)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'First page' })).toHaveAttribute('href', '/people');
  });

  it.each([
    'https://untrusted.example/people/person-one',
    '//untrusted.example/people/person-one',
    '/people/..',
    '/people/%2e%2e',
    '/people/p%2Fq',
    '/people/person-one?sort=name',
    '/people/person-one?cursor=',
    '/people/person-one?cursor=a&cursor=b',
    '/people/person-one#fragment',
  ])('does not navigate back to an unsafe person origin: %s', async (from) => {
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      return problem(404, 'NOT_FOUND');
    });
    renderPage({ pathname: '/albums/album-one', state: { from } });
    expect(await screen.findByRole('link', { name: 'Back to albums' })).toHaveAttribute(
      'href',
      '/albums',
    );
  });

  it('keeps the person and people pages when an album becomes unavailable', async () => {
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname === '/api/v2/people/person-one') return Response.json(person);
      if (url.pathname === '/api/v2/people/person-one/albums') return Response.json(emptyPage);
      return problem(404, 'NOT_FOUND');
    });
    renderPage({
      pathname: '/albums/album-one',
      state: {
        from: '/people/person-one?cursor=album-cursor',
        peopleFrom: '/people?cursor=people-cursor',
      },
    });
    expect(
      await screen.findByText('This album is unavailable or you no longer have access.'),
    ).toBeInTheDocument();
    const links = screen.getAllByRole('link', { name: 'Back to person' });
    expect(links).toHaveLength(2);
    await userEvent.click(links[1]!);
    expect(await screen.findByRole('heading', { name: 'Same name' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to people' })).toHaveAttribute(
      'href',
      '/people?cursor=people-cursor',
    );
  });

  it('keeps person return context when an expired album-media page returns to its first page', async () => {
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname === '/api/v2/albums/album-one') return Response.json(album);
      if (url.pathname === '/api/v2/albums/album-one/rating')
        return Response.json(
          { album_id: 'album-one', rating: null, updated_at: null, version: 1 },
          { headers: { ETag: '"rating-test"' } },
        );
      if (url.pathname === '/api/v2/albums/album-one/media')
        return url.searchParams.has('cursor')
          ? problem(400, 'INVALID_CURSOR')
          : Response.json(emptyPage);
      if (url.pathname === '/api/v2/people/person-one') return Response.json(person);
      if (url.pathname === '/api/v2/people/person-one/albums') return Response.json(emptyPage);
      return problem(404, 'NOT_FOUND');
    });
    renderPage({
      pathname: '/albums/album-one',
      search: '?cursor=expired',
      state: {
        from: '/people/person-one?cursor=album-cursor',
        peopleFrom: '/people?cursor=people-cursor',
      },
    });
    expect(await screen.findByText(invalidPage)).toBeInTheDocument();
    await userEvent.click(screen.getByRole('link', { name: 'First page' }));
    await userEvent.click(await screen.findByRole('link', { name: 'Back to person' }));
    expect(await screen.findByRole('heading', { name: 'Same name' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to people' })).toHaveAttribute(
      'href',
      '/people?cursor=people-cursor',
    );
  });

  it('drops an unsafe parent-list origin while retaining the safe person page', async () => {
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname === '/api/v2/people/person-one') return Response.json(person);
      if (url.pathname === '/api/v2/people/person-one/albums') return Response.json(emptyPage);
      return problem(404, 'NOT_FOUND');
    });
    renderPage({
      pathname: '/albums/album-one',
      state: { from: '/people/person-one', peopleFrom: '//untrusted.example/people' },
    });
    await userEvent.click(await screen.findByRole('link', { name: 'Back to person' }));
    expect(await screen.findByRole('heading', { name: 'Same name' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to people' })).toHaveAttribute('href', '/people');
  });

  it.each([
    { id: 'account-b', scope_id: 'scope-b' },
    { id: 'account-a', scope_id: 'scope-b' },
    { id: 'account-b', scope_id: 'scope-a' },
  ])('separates account and scope: $id/$scope_id', async (next) => {
    let current = user;
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(current);
      if (url.pathname.endsWith('/variants')) return Response.json(emptyPage);
      const item = current === user ? person : { ...person, id: 'person-new', name: 'New account' };
      return Response.json({ ...emptyPage, items: [item] });
    });
    const client = renderPage('/people');
    expect(await screen.findByRole('link', { name: /Same name/ })).toBeInTheDocument();
    current = { ...user, ...next };
    act(() => client.setQueryData(['auth', 'me'], current));
    expect(await screen.findByRole('link', { name: /New account/ })).toBeInTheDocument();
    expect(screen.queryByText('Same name')).not.toBeInTheDocument();
  });

  it.each([
    { id: 'account-b', scope_id: 'scope-a' },
    { id: 'account-a', scope_id: 'scope-b' },
  ])('separates person detail by account and scope: $id/$scope_id', async (next) => {
    let current = user;
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(current);
      if (url.pathname.endsWith('/variants')) return Response.json(emptyPage);
      if (url.pathname.endsWith('/albums')) {
        return Response.json({
          ...emptyPage,
          items: [current === user ? album : { ...album, title: 'New account album' }],
        });
      }
      return Response.json(current === user ? person : { ...person, name: 'New account person' });
    });
    const client = renderPage('/people/person-one');
    expect(await screen.findByRole('heading', { name: 'Same name' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /First album/ })).toBeInTheDocument();
    current = { ...user, ...next };
    act(() => client.setQueryData(['auth', 'me'], current));
    expect(await screen.findByRole('heading', { name: 'New account person' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /New account album/ })).toBeInTheDocument();
    expect(screen.queryByText('Same name')).not.toBeInTheDocument();
    expect(screen.queryByText('First album')).not.toBeInTheDocument();
  });

  it('hides stale people while refreshing and after a transient failure', async () => {
    let finish!: (value: Response) => void;
    let mode: 'initial' | 'pending' | 'recovered' = 'initial';
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname.endsWith('/variants')) return Response.json(emptyPage);
      return mode === 'pending'
        ? new Promise((resolve) => {
            finish = resolve;
          })
        : Response.json({ ...emptyPage, items: [person] });
    });
    const client = renderPage('/people');
    expect(await screen.findByRole('link', { name: /Same name/ })).toBeInTheDocument();
    mode = 'pending';
    act(() => {
      void client.invalidateQueries({ queryKey: [user.id, user.scope_id, 'people'] });
    });
    await waitFor(() => expect(finish).toBeDefined());
    expect(screen.queryByText('Same name')).not.toBeInTheDocument();
    await act(async () => finish(problem(503, 'INTERNAL_ERROR')));
    expect(
      await screen.findByText('Could not load this page. Check your connection and try again.'),
    ).toBeInTheDocument();
    expect(screen.queryByText('Same name')).not.toBeInTheDocument();
    mode = 'recovered';
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByRole('link', { name: /Same name/ })).toBeInTheDocument();
  });

  it('hides person and albums while refreshing and after a transient failure', async () => {
    let finish!: (value: Response) => void;
    let mode: 'initial' | 'pending' | 'recovered' = 'initial';
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname === '/api/v2/people/person-one') {
        return mode === 'pending'
          ? new Promise((resolve) => {
              finish = resolve;
            })
          : Response.json(person);
      }
      if (url.pathname === '/api/v2/people/person-one/albums')
        return Response.json({ ...emptyPage, items: [album] });
      return Response.json(emptyPage);
    });
    const client = renderPage('/people/person-one');
    expect(await screen.findByRole('heading', { name: 'Same name' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /First album/ })).toBeInTheDocument();
    mode = 'pending';
    act(() => {
      void client.invalidateQueries({ queryKey: [user.id, user.scope_id, 'person', person.id] });
    });
    await waitFor(() => expect(finish).toBeDefined());
    expect(screen.queryByText('Same name')).not.toBeInTheDocument();
    expect(screen.queryByText('First album')).not.toBeInTheDocument();
    await act(async () => finish(problem(503, 'INTERNAL_ERROR')));
    expect(
      await screen.findByText('Could not load this page. Check your connection and try again.'),
    ).toBeInTheDocument();
    expect(screen.queryByText('Same name')).not.toBeInTheDocument();
    expect(screen.queryByText('First album')).not.toBeInTheDocument();
    expect(screen.queryByText('private path')).not.toBeInTheDocument();
    mode = 'recovered';
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByRole('heading', { name: 'Same name' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /First album/ })).toBeInTheDocument();
  });

  it('hides the person when the separately fetched album page loses access', async () => {
    let finish!: (value: Response) => void;
    installFetch((url) => {
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname === '/api/v2/people/person-one') return Response.json(person);
      if (url.pathname === '/api/v2/people/person-one/albums')
        return new Promise((resolve) => {
          finish = resolve;
        });
      return problem(404, 'NOT_FOUND');
    });
    renderPage('/people/person-one');
    await waitFor(() => expect(finish).toBeDefined());
    expect(screen.queryByText('Same name')).not.toBeInTheDocument();
    await act(async () => finish(problem(404, 'NOT_FOUND')));
    expect(
      await screen.findByText('This person is unavailable or you no longer have access.'),
    ).toBeInTheDocument();
    expect(screen.queryByText('Same name')).not.toBeInTheDocument();
    expect(screen.queryByText('private path')).not.toBeInTheDocument();
    expect(
      screen
        .getAllByRole('link', { name: 'Back to people' })
        .every((link) => link.getAttribute('href') === '/people'),
    ).toBe(true);
  });

  it('rechecks the actual session after a people request returns 401', async () => {
    let revoked = false;
    installFetch((url) => {
      if (revoked) return problem(401, 'UNAUTHENTICATED');
      if (url.pathname.endsWith('/auth/me')) return Response.json(user);
      if (url.pathname.endsWith('/variants')) return Response.json(emptyPage);
      return Response.json({ ...emptyPage, items: [person] });
    });
    const client = renderPage('/people');
    expect(await screen.findByRole('link', { name: /Same name/ })).toBeInTheDocument();
    revoked = true;
    await act(async () => {
      await client.invalidateQueries({ queryKey: [user.id, user.scope_id, 'people'] });
    });
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
    expect(screen.queryByText('Same name')).not.toBeInTheDocument();
  });
});
