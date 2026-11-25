import { useQuery } from '@tanstack/react-query';
import { FormattedMessage, useIntl } from 'react-intl';
import { Link, useLocation, useOutletContext, useParams, useSearchParams } from 'react-router';
import { validReadActor } from '../../lib/api/read-identity';
import { APIError } from '../auth/api';
import type { User } from '../auth/api';
import { JobTime } from '../jobs/common';
import { safeID } from '../library/api';
import {
  LibraryNavigation,
  PageNavigation,
  usePageFocus,
  useRevalidateReadIdentity,
  validCursorSearch,
} from '../library/common';
import { getGroup, listGroups } from './api';

function groupsBack(from: unknown) {
  if (typeof from !== 'string' || from.includes('#')) return '/admin/groups';
  const question = from.indexOf('?');
  const path = question < 0 ? from : from.slice(0, question);
  const search = new URLSearchParams(question < 0 ? '' : from.slice(question + 1));
  return path === '/admin/groups' && validCursorSearch(search) ? from : '/admin/groups';
}

function GroupError({
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
    ? 'groups.denied'
    : code === 'UNAUTHENTICATED'
      ? 'auth.sessionExpired'
      : code === 'NOT_FOUND'
        ? 'groups.notFound'
        : invalid
          ? 'groups.invalidPage'
          : context
            ? 'auth.readContextChanged'
            : 'groups.error';
  return (
    <div className="gallery-message" role="alert">
      <p>
        <FormattedMessage id={message} />
      </p>
      {invalid || code === 'NOT_FOUND' ? (
        <Link className="button button-secondary" to={invalid ? '/admin/groups' : back}>
          <FormattedMessage id={invalid ? 'library.first' : 'groups.back'} />
        </Link>
      ) : denied || code === 'UNAUTHENTICATED' ? null : (
        <button className="button button-secondary" onClick={onRetry}>
          <FormattedMessage id="groups.refresh" />
        </button>
      )}
    </div>
  );
}

export function GroupsPage() {
  const user = useOutletContext<User>();
  const intl = useIntl();
  const location = useLocation();
  const [search] = useSearchParams();
  const cursor = search.get('cursor');
  const valid = validCursorSearch(search);
  const canRead = user.role === 'admin' && user.active && validReadActor(user);
  const main = usePageFocus(cursor, 'groups');
  const query = useQuery({
    queryKey: [user.id, user.scope_id, 'admin-groups', cursor],
    queryFn: ({ signal }) => listGroups(user, cursor, signal),
    enabled: canRead && valid,
    staleTime: 0,
    gcTime: 0,
    retry: false,
    networkMode: 'always',
    refetchOnReconnect: true,
  });
  useRevalidateReadIdentity(query.error);
  const next =
    query.data?.has_more && query.data.next_cursor
      ? `/admin/groups?${new URLSearchParams({ cursor: query.data.next_cursor })}`
      : null;
  const reload = () => {
    main.current?.focus();
    void query.refetch();
  };
  return (
    <main className="gallery-main" ref={main} tabIndex={-1} aria-labelledby="groups-title">
      <LibraryNavigation />
      <div className="gallery-heading">
        <div>
          <h1 id="groups-title">
            <FormattedMessage id="groups.title" />
          </h1>
          <p>
            <FormattedMessage id="groups.description" />
          </p>
        </div>
        {canRead && valid && (
          <button className="button button-secondary" disabled={query.isFetching} onClick={reload}>
            <FormattedMessage id="groups.refresh" />
          </button>
        )}
      </div>
      {user.role !== 'admin' || !user.active ? (
        <GroupError error={new APIError('ADMIN_REQUIRED')} back="/admin/groups" onRetry={reload} />
      ) : !validReadActor(user) ? (
        <GroupError
          error={new APIError('READ_CONTEXT_MISMATCH')}
          back="/admin/groups"
          onRetry={reload}
        />
      ) : !valid ? (
        <GroupError error={new APIError('INVALID_CURSOR')} back="/admin/groups" onRetry={reload} />
      ) : query.isPending || query.isFetching ? (
        <p role="status">
          <FormattedMessage id="groups.loading" />
        </p>
      ) : query.isError ? (
        <GroupError error={query.error} back="/admin/groups" onRetry={reload} />
      ) : (
        <>
          {query.data.items.length === 0 ? (
            <div className="gallery-message">
              <p>
                <FormattedMessage id="groups.empty" />
              </p>
            </div>
          ) : (
            <ul className="job-list">
              {query.data.items.map((group) => (
                <li key={group.id}>
                  <Link
                    className="job-entry-link"
                    to={`/admin/groups/${encodeURIComponent(group.id)}/details`}
                    state={{ from: location.pathname + location.search }}
                    aria-label={intl.formatMessage({ id: 'groups.open' }, { name: group.name })}
                  >
                    <strong>{group.name}</strong>
                  </Link>
                </li>
              ))}
            </ul>
          )}
          <PageNavigation first="/admin/groups" next={next} cursor={cursor} />
          <p className="gallery-note">
            <FormattedMessage id="groups.readOnly" />
          </p>
        </>
      )}
    </main>
  );
}

export function GroupPage() {
  const user = useOutletContext<User>();
  const intl = useIntl();
  const { groupId = '' } = useParams();
  const location = useLocation();
  const [search] = useSearchParams();
  const back = groupsBack((location.state as { from?: unknown } | null)?.from);
  const validID = safeID(groupId);
  const validSearch = [...search.keys()].length === 0;
  const canRead = user.role === 'admin' && user.active && validReadActor(user);
  const main = usePageFocus(null, groupId);
  const query = useQuery({
    queryKey: [user.id, user.scope_id, 'admin-group', groupId],
    queryFn: ({ signal }) => getGroup(user, groupId, signal),
    enabled: canRead && validID && validSearch,
    staleTime: 0,
    gcTime: 0,
    retry: false,
    networkMode: 'always',
    refetchOnReconnect: true,
  });
  useRevalidateReadIdentity(query.error);
  const reload = () => {
    main.current?.focus();
    void query.refetch();
  };
  return (
    <main className="gallery-main" ref={main} tabIndex={-1} aria-labelledby="group-title">
      <LibraryNavigation />
      <Link className="gallery-back" to={back}>
        <FormattedMessage id="groups.back" />
      </Link>
      <div className="gallery-heading">
        <h1 id="group-title">
          <FormattedMessage id="groups.details" />
        </h1>
        {canRead && validID && validSearch && (
          <button className="button button-secondary" disabled={query.isFetching} onClick={reload}>
            <FormattedMessage id="groups.refresh" />
          </button>
        )}
      </div>
      {user.role !== 'admin' || !user.active ? (
        <GroupError error={new APIError('ADMIN_REQUIRED')} back={back} onRetry={reload} />
      ) : !validReadActor(user) ? (
        <GroupError error={new APIError('READ_CONTEXT_MISMATCH')} back={back} onRetry={reload} />
      ) : !validID ? (
        <GroupError error={new APIError('NOT_FOUND')} back={back} onRetry={reload} />
      ) : !validSearch ? (
        <GroupError error={new APIError('INVALID_CURSOR')} back={back} onRetry={reload} />
      ) : query.isPending || query.isFetching ? (
        <p role="status">
          <FormattedMessage id="groups.loading" />
        </p>
      ) : query.isError ? (
        <GroupError error={query.error} back={back} onRetry={reload} />
      ) : (
        <section className="job-section" aria-labelledby="group-name">
          <h2 id="group-name">{query.data.name}</h2>
          <dl className="job-facts">
            <div>
              <dt>
                <FormattedMessage id="groups.identifier" />
              </dt>
              <dd>{query.data.id}</dd>
            </div>
            <div>
              <dt>
                <FormattedMessage id="groups.version" />
              </dt>
              <dd>{intl.formatNumber(query.data.version)}</dd>
            </div>
            <div>
              <dt>
                <FormattedMessage id="groups.created" />
              </dt>
              <dd>
                <JobTime value={query.data.created_at} />
              </dd>
            </div>
          </dl>
          <p className="gallery-note">
            <FormattedMessage id="groups.readOnly" />
          </p>
        </section>
      )}
    </main>
  );
}
