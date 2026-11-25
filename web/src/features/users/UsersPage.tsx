import { useEffect } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { FormattedMessage, useIntl } from 'react-intl';
import { Link, useLocation, useOutletContext, useParams, useSearchParams } from 'react-router';
import { APIError } from '../auth/api';
import type { User } from '../auth/api';
import { safeID } from '../library/api';
import {
  LibraryNavigation,
  PageNavigation,
  usePageFocus,
  validCursorSearch,
} from '../library/common';
import { getUser, listUsers, validReadActor } from './api';

function usersBack(from: unknown) {
  if (typeof from !== 'string' || from.includes('#')) return '/admin/users';
  const question = from.indexOf('?');
  const path = question < 0 ? from : from.slice(0, question);
  const search = new URLSearchParams(question < 0 ? '' : from.slice(question + 1));
  return path === '/admin/users' && validCursorSearch(search) ? from : '/admin/users';
}

function useIdentityRecovery(error: unknown) {
  const client = useQueryClient();
  useEffect(() => {
    if (
      error instanceof APIError &&
      ['UNAUTHENTICATED', 'ADMIN_REQUIRED', 'VERSION_CONFLICT', 'READ_CONTEXT_MISMATCH'].includes(
        error.code,
      )
    ) {
      void client.invalidateQueries({ queryKey: ['auth', 'me'] });
    }
  }, [client, error]);
}

function AccountError({
  error,
  back,
  onRetry,
}: {
  error: unknown;
  back: string;
  onRetry: () => void;
}) {
  const code = error instanceof APIError ? error.code : '';
  const invalid = ['INVALID_CURSOR', 'INVALID_FILTER', 'INVALID_PAGINATION'].includes(code);
  const denied = code === 'ADMIN_REQUIRED' || code === 'FORBIDDEN';
  const context = code === 'VERSION_CONFLICT' || code === 'READ_CONTEXT_MISMATCH';
  const message = denied
    ? 'users.denied'
    : code === 'UNAUTHENTICATED'
      ? 'auth.sessionExpired'
      : code === 'NOT_FOUND'
        ? 'users.notFound'
        : invalid
          ? 'users.invalidPage'
          : context
            ? 'users.contextChanged'
            : 'users.error';
  return (
    <div className="gallery-message" role="alert">
      <p>
        <FormattedMessage id={message} />
      </p>
      {invalid || code === 'NOT_FOUND' ? (
        <Link className="button button-secondary" to={invalid ? '/admin/users' : back}>
          <FormattedMessage id={invalid ? 'library.first' : 'users.back'} />
        </Link>
      ) : denied || code === 'UNAUTHENTICATED' ? null : (
        <button className="button button-secondary" onClick={onRetry}>
          <FormattedMessage id="users.refresh" />
        </button>
      )}
    </div>
  );
}

function Status({ account }: { account: User }) {
  return <FormattedMessage id={account.active ? 'users.active' : 'users.inactive'} />;
}

function Role({ account }: { account: User }) {
  return <FormattedMessage id={account.role === 'admin' ? 'users.admin' : 'users.user'} />;
}

function Download({ account }: { account: User }) {
  return <FormattedMessage id={account.download_original ? 'users.allowed' : 'users.notAllowed'} />;
}

export function UsersPage() {
  const user = useOutletContext<User>();
  const intl = useIntl();
  const location = useLocation();
  const [search] = useSearchParams();
  const cursor = search.get('cursor');
  const valid = validCursorSearch(search);
  const canRead = user.role === 'admin' && user.active && validReadActor(user);
  const main = usePageFocus(cursor, 'users');
  const query = useQuery({
    queryKey: [user.id, user.scope_id, 'admin-users', cursor],
    queryFn: ({ signal }) => listUsers(user, cursor, signal),
    enabled: canRead && valid,
    staleTime: 0,
    gcTime: 0,
    retry: false,
    networkMode: 'always',
    refetchOnReconnect: true,
  });
  useIdentityRecovery(query.error);
  const next =
    query.data?.has_more && query.data.next_cursor
      ? `/admin/users?${new URLSearchParams({ cursor: query.data.next_cursor })}`
      : null;
  const reload = () => {
    main.current?.focus();
    void query.refetch();
  };
  return (
    <main className="gallery-main" ref={main} tabIndex={-1} aria-labelledby="users-title">
      <LibraryNavigation />
      <div className="gallery-heading">
        <div>
          <h1 id="users-title">
            <FormattedMessage id="users.title" />
          </h1>
          <p>
            <FormattedMessage id="users.description" />
          </p>
        </div>
        {canRead && valid && (
          <button className="button button-secondary" disabled={query.isFetching} onClick={reload}>
            <FormattedMessage id="users.refresh" />
          </button>
        )}
      </div>
      {user.role !== 'admin' || !user.active ? (
        <AccountError error={new APIError('ADMIN_REQUIRED')} back="/admin/users" onRetry={reload} />
      ) : !validReadActor(user) ? (
        <AccountError
          error={new APIError('READ_CONTEXT_MISMATCH')}
          back="/admin/users"
          onRetry={reload}
        />
      ) : !valid ? (
        <AccountError error={new APIError('INVALID_CURSOR')} back="/admin/users" onRetry={reload} />
      ) : query.isPending || query.isFetching ? (
        <p role="status">
          <FormattedMessage id="users.loading" />
        </p>
      ) : query.isError ? (
        <AccountError error={query.error} back="/admin/users" onRetry={reload} />
      ) : (
        <>
          {query.data.items.length === 0 ? (
            <div className="gallery-message">
              <p>
                <FormattedMessage id="users.empty" />
              </p>
            </div>
          ) : (
            <ul className="job-list">
              {query.data.items.map((account) => (
                <li key={account.id}>
                  <div className="job-entry-heading">
                    <Link
                      className="job-entry-link"
                      to={`/admin/users/${encodeURIComponent(account.id)}/details`}
                      state={{ from: location.pathname + location.search }}
                      aria-label={intl.formatMessage(
                        { id: 'users.open' },
                        { name: account.username },
                      )}
                    >
                      <strong>{account.username}</strong>
                      <span>
                        <Role account={account} />
                      </span>
                    </Link>
                    <strong className="job-status">
                      <Status account={account} />
                    </strong>
                  </div>
                  <p>
                    <FormattedMessage id="users.original" />: <Download account={account} />
                  </p>
                  {account.id === user.id && (
                    <p>
                      <FormattedMessage id="users.current" />
                    </p>
                  )}
                </li>
              ))}
            </ul>
          )}
          <PageNavigation first="/admin/users" next={next} cursor={cursor} />
          <p className="gallery-note">
            <FormattedMessage id="users.readOnly" />
          </p>
        </>
      )}
    </main>
  );
}

export function UserPage() {
  const user = useOutletContext<User>();
  const intl = useIntl();
  const { userId = '' } = useParams();
  const location = useLocation();
  const [search] = useSearchParams();
  const back = usersBack((location.state as { from?: unknown } | null)?.from);
  const validID = safeID(userId);
  const validSearch = [...search.keys()].length === 0;
  const canRead = user.role === 'admin' && user.active && validReadActor(user);
  const main = usePageFocus(null, userId);
  const query = useQuery({
    queryKey: [user.id, user.scope_id, 'admin-user', userId],
    queryFn: ({ signal }) => getUser(user, userId, signal),
    enabled: canRead && validID && validSearch,
    staleTime: 0,
    gcTime: 0,
    retry: false,
    networkMode: 'always',
    refetchOnReconnect: true,
  });
  useIdentityRecovery(query.error);
  const reload = () => {
    main.current?.focus();
    void query.refetch();
  };
  return (
    <main className="gallery-main" ref={main} tabIndex={-1} aria-labelledby="user-title">
      <LibraryNavigation />
      <Link className="gallery-back" to={back}>
        <FormattedMessage id="users.back" />
      </Link>
      <div className="gallery-heading">
        <h1 id="user-title">
          <FormattedMessage id="users.details" />
        </h1>
        {canRead && validID && validSearch && (
          <button className="button button-secondary" disabled={query.isFetching} onClick={reload}>
            <FormattedMessage id="users.refresh" />
          </button>
        )}
      </div>
      {user.role !== 'admin' || !user.active ? (
        <AccountError error={new APIError('ADMIN_REQUIRED')} back={back} onRetry={reload} />
      ) : !validReadActor(user) ? (
        <AccountError error={new APIError('READ_CONTEXT_MISMATCH')} back={back} onRetry={reload} />
      ) : !validID ? (
        <AccountError error={new APIError('NOT_FOUND')} back={back} onRetry={reload} />
      ) : !validSearch ? (
        <AccountError error={new APIError('INVALID_CURSOR')} back={back} onRetry={reload} />
      ) : query.isPending || query.isFetching ? (
        <p role="status">
          <FormattedMessage id="users.loading" />
        </p>
      ) : query.isError ? (
        <AccountError error={query.error} back={back} onRetry={reload} />
      ) : (
        <section className="job-section" aria-labelledby="user-name">
          <h2 id="user-name">{query.data.username}</h2>
          <dl className="job-facts">
            <div>
              <dt>
                <FormattedMessage id="users.identifier" />
              </dt>
              <dd>{query.data.id}</dd>
            </div>
            <div>
              <dt>
                <FormattedMessage id="users.role" />
              </dt>
              <dd>
                <Role account={query.data} />
              </dd>
            </div>
            <div>
              <dt>
                <FormattedMessage id="users.availability" />
              </dt>
              <dd>
                <Status account={query.data} />
              </dd>
            </div>
            <div>
              <dt>
                <FormattedMessage id="users.original" />
              </dt>
              <dd>
                <Download account={query.data} />
              </dd>
            </div>
            <div>
              <dt>
                <FormattedMessage id="users.version" />
              </dt>
              <dd>{intl.formatNumber(query.data.version)}</dd>
            </div>
          </dl>
          {query.data.id === user.id && (
            <p>
              <FormattedMessage id="users.current" />
            </p>
          )}
          <p className="gallery-note">
            <FormattedMessage id="users.readOnly" />
          </p>
        </section>
      )}
    </main>
  );
}
