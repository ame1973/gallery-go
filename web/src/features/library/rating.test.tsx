import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { App } from '../../app/App';
import type { components } from '../../lib/api/schema';

type User = components['schemas']['User'];
type Vote = { rating: number | null; version: number };
type Command = { method: string; etag: string | null; key: string | null; body: string };

const user: User = {
  id: 'account-a',
  username: 'reader',
  role: 'user',
  active: true,
  scope_id: 'scope-a',
  version: 1,
  download_original: false,
};
const otherUser: User = { ...user, id: 'account-b', scope_id: 'scope-b', username: 'other' };
const album: components['schemas']['Album'] = {
  id: 'album-one',
  title: 'A family trip',
  library_id: 'library',
  cover_media_id: null,
  media_count: 0,
  rating_avg: null,
  view_count: 0,
  indexed_at: '2026-11-01T00:00:00Z',
  version: 1,
};

function problem(status: number, code: string) {
  return Response.json(
    { code, status, type: 'urn:test', title: 'Test', request_id: 'id' },
    { status },
  );
}

function setup(initial: Vote = { rating: null, version: 1 }, otherVotes: number[] = []) {
  let activeUser: User | null = user;
  let sharedVersion = 1;
  let lostReply = false;
  let nextReply: 'none' | 'unavailable' | 'idempotency-conflict' = 'none';
  let hangingReply: 'none' | 'abort' | 'ignore' = 'none';
  let releaseReply: (() => void) | undefined;
  let holdAlbum = false;
  let releaseAlbum: (() => void) | undefined;
  let revokeAfterWrite = false;
  let denied: 'none' | 'album' | 'read' | 'write' | 'csrf' = 'none';
  let badTag: 'none' | 'missing' | 'weak' | 'list' | 'wrong-album' | 'bad-rating' = 'none';
  let failRefresh = false;
  const votes = new Map<string, Vote>([[user.id, initial]]);
  const receipts = new Map<string, { command: Command; result: Vote }>();
  const commands: Command[] = [];
  const requests: Request[] = [];
  const tag = (id: string, vote: Vote) => `"rating:${id}:${vote.version}"`;
  const current = (id: string) => votes.get(id) ?? { rating: null, version: 1 };
  const average = () => {
    const ratings = [...otherVotes, ...[...votes.values()].flatMap((vote) => vote.rating ?? [])];
    return ratings.length ? ratings.reduce((sum, value) => sum + value, 0) / ratings.length : null;
  };
  const value = (vote: Vote) => ({
    album_id: album.id,
    rating: vote.rating,
    version: vote.version,
    updated_at: vote.version === 1 ? null : '2026-11-11T00:00:00Z',
  });
  const fetch = vi.fn(async (request: Request): Promise<Response> => {
    requests.push(request.clone());
    const path = new URL(request.url).pathname;
    if (path.endsWith('/auth/me'))
      return activeUser ? Response.json(activeUser) : problem(401, 'UNAUTHENTICATED');
    if (!activeUser) return problem(401, 'UNAUTHENTICATED');
    if (path.endsWith('/auth/csrf')) {
      if (denied === 'csrf') {
        activeUser = null;
        return problem(401, 'UNAUTHENTICATED');
      }
      return Response.json({ token: 'csrf-token' });
    }
    if (path.endsWith('/albums/album-one/media')) {
      return Response.json({ items: [], next_cursor: null, has_more: false });
    }
    if (path.endsWith('/albums/album-one')) {
      const response = () =>
        failRefresh
          ? problem(503, 'UNAVAILABLE')
          : denied === 'album'
            ? problem(404, 'NOT_FOUND')
            : Response.json({ ...album, version: sharedVersion, rating_avg: average() });
      if (holdAlbum) {
        holdAlbum = false;
        return new Promise<Response>((resolve) => {
          releaseAlbum = () => resolve(response());
        });
      }
      return response();
    }
    if (path.endsWith('/albums/album-one/rating')) {
      if (request.method === 'GET') {
        if (denied === 'read') return problem(404, 'NOT_FOUND');
        if (failRefresh) return problem(503, 'UNAVAILABLE');
        const vote = current(activeUser.id);
        const baseTag = tag(activeUser.id, vote);
        const etag =
          badTag === 'weak' ? `W/${baseTag}` : badTag === 'list' ? `${baseTag}, "other"` : baseTag;
        const result = value(vote);
        if (badTag === 'wrong-album') result.album_id = 'different-album';
        if (badTag === 'bad-rating') result.rating = 6;
        return Response.json(result, { headers: badTag === 'missing' ? {} : { ETag: etag } });
      }
      const actor = activeUser.id;
      const command = {
        method: request.method,
        etag: request.headers.get('If-Match'),
        key: request.headers.get('Idempotency-Key'),
        body: await request.clone().text(),
      };
      commands.push(command);
      if (denied === 'write') return problem(404, 'NOT_FOUND');
      if (nextReply === 'idempotency-conflict') {
        nextReply = 'none';
        return problem(409, 'IDEMPOTENCY_CONFLICT');
      }
      const receipt = receipts.get(command.key ?? '');
      if (receipt) {
        if (JSON.stringify(command) !== JSON.stringify(receipt.command))
          return problem(409, 'IDEMPOTENCY_CONFLICT');
        return Response.json(value(receipt.result), {
          headers: { ETag: tag(actor, receipt.result) },
        });
      }
      const vote = current(actor);
      if (command.etag !== tag(actor, vote)) return problem(409, 'VERSION_CONFLICT');
      const rating =
        request.method === 'DELETE'
          ? null
          : (JSON.parse(command.body) as { rating: number }).rating;
      const next = { rating, version: vote.version + (rating === vote.rating ? 0 : 1) };
      if (rating !== vote.rating) sharedVersion += 1;
      votes.set(actor, next);
      receipts.set(command.key ?? '', { command, result: next });
      if (revokeAfterWrite) denied = 'read';
      if (hangingReply !== 'none') {
        const mode = hangingReply;
        hangingReply = 'none';
        return new Promise<Response>((resolve, reject) => {
          releaseReply = () =>
            resolve(Response.json(value(next), { headers: { ETag: tag(actor, next) } }));
          if (mode === 'abort') {
            if (request.signal.aborted) reject(new DOMException('Aborted', 'AbortError'));
            request.signal.addEventListener(
              'abort',
              () => reject(new DOMException('Aborted', 'AbortError')),
              { once: true },
            );
          }
        });
      }
      if (lostReply) {
        lostReply = false;
        throw new TypeError('response lost');
      }
      if (nextReply === 'unavailable') {
        nextReply = 'none';
        return problem(503, 'UNAVAILABLE');
      }
      return Response.json(value(next), { headers: { ETag: tag(actor, next) } });
    }
    return problem(404, 'NOT_FOUND');
  });
  vi.stubGlobal('fetch', fetch);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const view = render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={['/albums/album-one']}>
        <App />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return {
    ...view,
    client,
    commands,
    requests,
    setActive(next: User | null) {
      activeUser = next;
      act(() => client.setQueryData(['auth', 'me'], next));
    },
    setVote(id: string, rating: number | null) {
      const prior = current(id);
      votes.set(id, { rating, version: prior.version + 1 });
      sharedVersion += 1;
    },
    loseNextReply() {
      lostReply = true;
    },
    replyNext(value: Exclude<typeof nextReply, 'none'>) {
      nextReply = value;
    },
    hangNextReply(mode: 'abort' | 'ignore') {
      hangingReply = mode;
    },
    releaseReply() {
      releaseReply?.();
    },
    holdNextAlbum() {
      holdAlbum = true;
    },
    releaseAlbum() {
      releaseAlbum?.();
    },
    revokeOnWrite() {
      revokeAfterWrite = true;
    },
    deny(value: typeof denied) {
      denied = value;
    },
    invalidTag(value: typeof badTag) {
      badTag = value;
    },
    failReads(value: boolean) {
      failRefresh = value;
    },
  };
}

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('personal album rating', () => {
  it('saves an explicit vote with the personal ETag and refreshes the shared average', async () => {
    const server = setup(undefined, [1]);
    const input = await screen.findByRole('radio', { name: '5 stars' });
    expect(screen.getByRole('radio', { name: 'No rating' })).toBeChecked();
    expect(screen.getByText('Average rating: 1 of 5')).toBeInTheDocument();
    await userEvent.click(input);
    expect(server.commands).toHaveLength(0);
    await userEvent.click(screen.getByRole('button', { name: 'Save rating' }));
    expect(await screen.findByText('Average rating: 3 of 5')).toBeInTheDocument();
    expect(await screen.findByRole('radio', { name: '5 stars' })).toBeChecked();
    expect(
      screen.getByText('Rating request completed. Current values are shown.'),
    ).toBeInTheDocument();
    expect(server.commands).toHaveLength(1);
    expect(server.commands[0]).toMatchObject({
      method: 'PUT',
      etag: '"rating:account-a:1"',
      body: '{"rating":5}',
    });
    expect(server.commands[0]?.key).toMatch(/^[0-9a-f-]{36}$/);
    const write = server.requests.find((request) => request.method === 'PUT')!;
    expect(write.headers.get('X-CSRF-Token')).toBe('csrf-token');
    expect(server.requests.some((request) => request.url.endsWith('/original'))).toBe(false);
    await userEvent.click(screen.getByRole('radio', { name: '4 stars' }));
    expect(
      screen.queryByText('Rating request completed. Current values are shown.'),
    ).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Save rating' })).toBeEnabled();
  });

  it('replaces and clears the vote without a DELETE body or reused key', async () => {
    const server = setup({ rating: 5, version: 2 }, [1]);
    expect(await screen.findByRole('radio', { name: '5 stars' })).toBeChecked();
    await userEvent.click(screen.getByRole('radio', { name: '3 stars' }));
    await userEvent.click(screen.getByRole('button', { name: 'Save rating' }));
    expect(await screen.findByText('Average rating: 2 of 5')).toBeInTheDocument();
    await userEvent.click(await screen.findByRole('radio', { name: 'No rating' }));
    await userEvent.click(screen.getByRole('button', { name: 'Save rating' }));
    expect(await screen.findByText('Average rating: 1 of 5')).toBeInTheDocument();
    expect(await screen.findByRole('radio', { name: 'No rating' })).toBeChecked();
    expect(server.commands[0]).toMatchObject({
      method: 'PUT',
      etag: '"rating:account-a:2"',
      body: '{"rating":3}',
    });
    expect(server.commands[1]).toMatchObject({
      method: 'DELETE',
      etag: '"rating:account-a:3"',
      body: '',
    });
    expect(server.commands[0]?.key).not.toBe(server.commands[1]?.key);
  });

  it('retries a lost response with the identical command and reads current, not historical, state', async () => {
    const server = setup(undefined, [1]);
    server.loseNextReply();
    await userEvent.click(await screen.findByRole('radio', { name: '5 stars' }));
    await userEvent.click(screen.getByRole('button', { name: 'Save rating' }));
    expect(
      await screen.findByText(/Could not confirm whether your rating was saved/),
    ).toBeInTheDocument();
    server.setVote(user.id, 2);
    await userEvent.click(screen.getByRole('button', { name: 'Retry request' }));
    expect(await screen.findByText('Average rating: 1.5 of 5')).toBeInTheDocument();
    expect(await screen.findByRole('radio', { name: '2 stars' })).toBeChecked();
    expect(server.commands).toHaveLength(2);
    expect(server.commands[1]).toEqual(server.commands[0]);
  });

  it('keeps an uncertain command while the album is revalidated', async () => {
    const server = setup();
    server.loseNextReply();
    await userEvent.click(await screen.findByRole('radio', { name: '5 stars' }));
    await userEvent.click(screen.getByRole('button', { name: 'Save rating' }));
    await screen.findByText(/Could not confirm whether your rating was saved/);
    server.holdNextAlbum();
    act(() => {
      void server.client.invalidateQueries({
        queryKey: [user.id, user.scope_id, 'album', album.id],
      });
    });
    expect(await screen.findByText('Loading albums…')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Retry request' })).not.toBeInTheDocument();
    server.setVote(user.id, 2);
    await act(async () => server.releaseAlbum());
    await userEvent.click(await screen.findByRole('button', { name: 'Retry request' }));
    await waitFor(() => expect(screen.getByRole('radio', { name: '2 stars' })).toBeChecked());
    expect(server.commands).toHaveLength(2);
    expect(server.commands[1]).toEqual(server.commands[0]);
  });

  it('keeps an uncertain command through a transient album revalidation failure', async () => {
    const server = setup();
    server.loseNextReply();
    await userEvent.click(await screen.findByRole('radio', { name: '5 stars' }));
    await userEvent.click(screen.getByRole('button', { name: 'Save rating' }));
    await screen.findByText(/Could not confirm whether your rating was saved/);
    server.holdNextAlbum();
    act(() => {
      void server.client.invalidateQueries({
        queryKey: [user.id, user.scope_id, 'album', album.id],
      });
    });
    expect(await screen.findByText('Loading albums…')).toBeInTheDocument();
    server.failReads(true);
    await act(async () => server.releaseAlbum());
    expect(
      await screen.findByText('Could not load this page. Check your connection and try again.'),
    ).toBeInTheDocument();
    expect(screen.queryByText('A family trip')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Retry request' })).not.toBeInTheDocument();
    server.setVote(user.id, 2);
    server.failReads(false);
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }));
    await userEvent.click(await screen.findByRole('button', { name: 'Retry request' }));
    await waitFor(() => expect(screen.getByRole('radio', { name: '2 stars' })).toBeChecked());
    expect(server.commands).toHaveLength(2);
    expect(server.commands[1]).toEqual(server.commands[0]);
  });

  it('discards the pending command if album revalidation denies access', async () => {
    const server = setup();
    server.loseNextReply();
    await userEvent.click(await screen.findByRole('radio', { name: '5 stars' }));
    await userEvent.click(screen.getByRole('button', { name: 'Save rating' }));
    await screen.findByText(/Could not confirm whether your rating was saved/);
    server.holdNextAlbum();
    act(() => {
      void server.client.invalidateQueries({
        queryKey: [user.id, user.scope_id, 'album', album.id],
      });
    });
    expect(await screen.findByText('Loading albums…')).toBeInTheDocument();
    expect(screen.queryByText('A family trip')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Retry request' })).not.toBeInTheDocument();
    server.deny('album');
    await act(async () => server.releaseAlbum());
    expect(
      await screen.findByText('This album is unavailable or you no longer have access.'),
    ).toBeInTheDocument();
    expect(screen.queryByText('A family trip')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Retry request' })).not.toBeInTheDocument();
    expect(server.commands).toHaveLength(1);
  });

  it('retries an uncertain server error with the same command and refreshes current state', async () => {
    const server = setup();
    server.replyNext('unavailable');
    await userEvent.click(await screen.findByRole('radio', { name: '5 stars' }));
    await userEvent.click(screen.getByRole('button', { name: 'Save rating' }));
    expect(
      await screen.findByText(/Could not confirm whether your rating was saved/),
    ).toBeInTheDocument();
    server.setVote(user.id, 2);
    await userEvent.click(screen.getByRole('button', { name: 'Retry request' }));
    await waitFor(() => expect(screen.getByRole('radio', { name: '2 stars' })).toBeChecked());
    expect(server.commands).toHaveLength(2);
    expect(server.commands[1]).toEqual(server.commands[0]);
  });

  it('requires a reload after an idempotency conflict', async () => {
    const server = setup({ rating: 2, version: 2 });
    server.replyNext('idempotency-conflict');
    await userEvent.click(await screen.findByRole('radio', { name: '5 stars' }));
    await userEvent.click(screen.getByRole('button', { name: 'Save rating' }));
    expect(await screen.findByText(/This request cannot be retried safely/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Retry request' })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Reload current rating' }));
    await waitFor(() => expect(screen.getByRole('radio', { name: '2 stars' })).toBeChecked());
    expect(server.commands).toHaveLength(1);
  });

  it('requires an explicit refresh after a concurrent personal change', async () => {
    const server = setup();
    await userEvent.click(await screen.findByRole('radio', { name: '5 stars' }));
    server.setVote(user.id, 2);
    await userEvent.click(screen.getByRole('button', { name: 'Save rating' }));
    expect(await screen.findByText(/Your rating changed elsewhere/)).toBeInTheDocument();
    expect(server.commands).toHaveLength(1);
    expect(screen.queryByRole('button', { name: 'Retry request' })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Reload current rating' }));
    expect(await screen.findByRole('radio', { name: '2 stars' })).toBeChecked();
    expect(screen.getByText('Average rating: 2 of 5')).toBeInTheDocument();
    expect(server.commands).toHaveLength(1);
  });

  it('does not repeat a successful write when refreshing current values fails', async () => {
    const server = setup();
    await userEvent.click(await screen.findByRole('radio', { name: '4 stars' }));
    server.failReads(true);
    await userEvent.click(screen.getByRole('button', { name: 'Save rating' }));
    expect(await screen.findByText(/Could not refresh the current values/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Retry request' })).not.toBeInTheDocument();
    server.failReads(false);
    await userEvent.click(screen.getByRole('button', { name: 'Reload current rating' }));
    expect(await screen.findByRole('radio', { name: '4 stars' })).toBeChecked();
    expect(server.commands).toHaveLength(1);
  });

  it('isolates personal state on an account and scope change', async () => {
    const server = setup({ rating: 5, version: 2 });
    server.setVote(otherUser.id, 2);
    expect(await screen.findByRole('radio', { name: '5 stars' })).toBeChecked();
    server.setActive(otherUser);
    await waitFor(() => expect(screen.getByRole('radio', { name: '2 stars' })).toBeChecked());
    expect(screen.getByRole('radio', { name: '5 stars' })).not.toBeChecked();
    expect(server.commands).toHaveLength(0);
  });

  it('discards an unsaved selection after the same account receives a new scope', async () => {
    const server = setup({ rating: 2, version: 2 });
    expect(await screen.findByRole('radio', { name: '2 stars' })).toBeChecked();
    await userEvent.click(screen.getByRole('radio', { name: '5 stars' }));
    expect(screen.getByRole('button', { name: 'Save rating' })).toBeEnabled();
    server.setActive({ ...user, scope_id: 'scope-a-new' });
    await waitFor(() => expect(screen.getByRole('radio', { name: '2 stars' })).toBeChecked());
    expect(screen.getByRole('button', { name: 'Save rating' })).toBeDisabled();
    expect(server.commands).toHaveLength(0);
  });

  it.each(['read', 'write'] as const)(
    'hides the album if rating access is revoked on %s',
    async (phase) => {
      const server = setup({ rating: 5, version: 2 });
      await screen.findByRole('radio', { name: '5 stars' });
      server.deny(phase);
      if (phase === 'read') {
        await act(async () => {
          await server.client.invalidateQueries({
            queryKey: [user.id, user.scope_id, 'album-rating', album.id],
          });
        });
      } else {
        await userEvent.click(screen.getByRole('radio', { name: '3 stars' }));
        await userEvent.click(screen.getByRole('button', { name: 'Save rating' }));
      }
      expect(
        await screen.findByText('This album is unavailable or you no longer have access.'),
      ).toBeInTheDocument();
      expect(screen.queryByText('A family trip')).not.toBeInTheDocument();
      expect(screen.queryByRole('radio', { name: '5 stars' })).not.toBeInTheDocument();
    },
  );

  it('revalidates the session if the CSRF endpoint rejects it', async () => {
    const server = setup();
    await userEvent.click(await screen.findByRole('radio', { name: '4 stars' }));
    server.deny('csrf');
    await userEvent.click(screen.getByRole('button', { name: 'Save rating' }));
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
    expect(server.commands).toHaveLength(0);
  });

  it.each(['missing', 'weak', 'list', 'wrong-album', 'bad-rating'] as const)(
    'refuses to write after an invalid personal response: %s',
    async (kind) => {
      const server = setup();
      server.invalidTag(kind);
      await act(async () => {
        await server.client.invalidateQueries({
          queryKey: [user.id, user.scope_id, 'album-rating', album.id],
        });
      });
      expect(await screen.findByText(/Could not load your rating/)).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Save rating' })).not.toBeInTheDocument();
      expect(server.commands).toHaveLength(0);
    },
  );

  it('treats a request timeout as uncertain and retries the same accepted command', async () => {
    const server = setup();
    server.hangNextReply('abort');
    await userEvent.click(await screen.findByRole('radio', { name: '4 stars' }));
    vi.useFakeTimers();
    fireEvent.click(screen.getByRole('button', { name: 'Save rating' }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(15_000);
    });
    expect(server.commands).toHaveLength(1);
    expect(screen.getByText(/Could not confirm whether your rating was saved/)).toBeInTheDocument();
    vi.useRealTimers();
    await userEvent.click(screen.getByRole('button', { name: 'Retry request' }));
    expect(await screen.findByRole('radio', { name: '4 stars' })).toBeChecked();
    expect(server.commands[1]).toEqual(server.commands[0]);
  });

  it('ignores a late response after the account and scope change', async () => {
    const server = setup();
    server.setVote(otherUser.id, 2);
    server.hangNextReply('ignore');
    await userEvent.click(await screen.findByRole('radio', { name: '4 stars' }));
    await userEvent.click(screen.getByRole('button', { name: 'Save rating' }));
    await waitFor(() => expect(server.commands).toHaveLength(1));
    server.setActive(otherUser);
    await waitFor(() => expect(screen.getByRole('radio', { name: '2 stars' })).toBeChecked());
    await act(async () => {
      server.releaseReply();
    });
    expect(screen.getByRole('radio', { name: '2 stars' })).toBeChecked();
    expect(
      screen.queryByText('Rating request completed. Current values are shown.'),
    ).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Retry request' })).not.toBeInTheDocument();
    expect(server.commands).toHaveLength(1);
  });

  it('hides the album if authorization is lost during the post-write read', async () => {
    const server = setup();
    server.revokeOnWrite();
    await userEvent.click(await screen.findByRole('radio', { name: '3 stars' }));
    await userEvent.click(screen.getByRole('button', { name: 'Save rating' }));
    expect(
      await screen.findByText('This album is unavailable or you no longer have access.'),
    ).toBeInTheDocument();
    expect(screen.queryByText('A family trip')).not.toBeInTheDocument();
    expect(screen.queryByRole('radio', { name: '3 stars' })).not.toBeInTheDocument();
    expect(server.commands).toHaveLength(1);
  });

  it('expires uncertain retries locally rather than sending a possibly forgotten key', async () => {
    const server = setup();
    server.loseNextReply();
    await userEvent.click(await screen.findByRole('radio', { name: '5 stars' }));
    await userEvent.click(screen.getByRole('button', { name: 'Save rating' }));
    await screen.findByText(/Could not confirm whether your rating was saved/);
    const now = Date.now();
    vi.spyOn(Date, 'now').mockReturnValue(now + 16 * 60 * 1000);
    fireEvent.click(screen.getByRole('button', { name: 'Retry request' }));
    expect(await screen.findByText(/This request is too old to retry safely/)).toBeInTheDocument();
    expect(server.commands).toHaveLength(1);
  });

  it('uses labeled native controls in all three locales', async () => {
    setup();
    expect(await screen.findByRole('group', { name: 'Your rating' })).toBeInTheDocument();
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Language' }), 'zh-TW');
    expect(screen.getByRole('group', { name: '你的評分' })).toBeInTheDocument();
    expect(screen.getByRole('radio', { name: '5 顆星' })).toBeInTheDocument();
    await userEvent.selectOptions(screen.getByRole('combobox', { name: '語言' }), 'zh-CN');
    expect(screen.getByRole('group', { name: '您的评分' })).toBeInTheDocument();
    expect(screen.getByRole('radio', { name: '5 颗星' })).toBeInTheDocument();
  });
});
