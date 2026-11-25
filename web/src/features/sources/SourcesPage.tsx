import type { ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { FormattedMessage, useIntl } from 'react-intl';
import { Link, useLocation, useOutletContext, useParams, useSearchParams } from 'react-router';
import { validReadActor } from '../../lib/api/read-identity';
import { APIError } from '../auth/api';
import type { User } from '../auth/api';
import { Diagnostic, JobTime } from '../jobs/common';
import { safeID } from '../library/api';
import {
  LibraryNavigation,
  PageNavigation,
  usePageFocus,
  useRevalidateReadIdentity,
  validCursorSearch,
} from '../library/common';
import { getSource, listSources } from './api';
import type { Source } from './api';

const diagnostics = new Set([
  'SOURCE_UNAVAILABLE',
  'SOURCE_NOT_CONFIGURED',
  'SOURCE_CHANGED',
  'IO_ERROR',
  'MINIMUM_FILES',
  'SCAN_LIMIT',
  'UNSAFE_ENTRY',
  'SYMLINK_CYCLE',
]);

function sourceBack(from: unknown) {
  if (typeof from !== 'string' || from.includes('#')) return '/admin/sources';
  const question = from.indexOf('?');
  const path = question < 0 ? from : from.slice(0, question);
  const search = new URLSearchParams(question < 0 ? '' : from.slice(question + 1));
  return path === '/admin/sources' && validCursorSearch(search) ? from : '/admin/sources';
}

function SourceError({
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
  const message = denied
    ? 'source.denied'
    : code === 'UNAUTHENTICATED'
      ? 'auth.sessionExpired'
      : code === 'NOT_FOUND'
        ? 'source.notFound'
        : invalid
          ? 'source.invalidPage'
          : code === 'VERSION_CONFLICT' || code === 'READ_CONTEXT_MISMATCH'
            ? 'auth.readContextChanged'
            : 'source.requestError';
  return (
    <div className="gallery-message" role="alert">
      <p>
        <FormattedMessage id={message} />
      </p>
      {invalid || code === 'NOT_FOUND' ? (
        <Link className="button button-secondary" to={invalid ? '/admin/sources' : back}>
          <FormattedMessage id={invalid ? 'library.first' : 'source.back'} />
        </Link>
      ) : denied || code === 'UNAUTHENTICATED' ? null : (
        <button className="button button-secondary" onClick={onRetry}>
          <FormattedMessage id="source.refresh" />
        </button>
      )}
    </div>
  );
}

function State({ state }: { state: Source['state'] }) {
  return <FormattedMessage id={state === 'error' ? 'source.errorState' : `source.${state}`} />;
}

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div>
      <dt>
        <FormattedMessage id={label} />
      </dt>
      <dd>{children}</dd>
    </div>
  );
}

export function SourcesPage() {
  const user = useOutletContext<User>();
  const intl = useIntl();
  const location = useLocation();
  const [search] = useSearchParams();
  const cursor = search.get('cursor');
  const valid = validCursorSearch(search);
  const canRead = user.role === 'admin' && user.active && validReadActor(user);
  const main = usePageFocus(cursor, 'sources');
  const query = useQuery({
    queryKey: [user.id, user.scope_id, 'admin-sources', cursor],
    queryFn: ({ signal }) => listSources(user, cursor, signal),
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
      ? `/admin/sources?${new URLSearchParams({ cursor: query.data.next_cursor })}`
      : null;
  const reload = () => {
    main.current?.focus();
    void query.refetch();
  };
  return (
    <main className="gallery-main" ref={main} tabIndex={-1} aria-labelledby="sources-title">
      <LibraryNavigation />
      <div className="gallery-heading">
        <div>
          <h1 id="sources-title">
            <FormattedMessage id="source.title" />
          </h1>
          <p>
            <FormattedMessage id="source.description" />
          </p>
        </div>
        {canRead && valid && (
          <button className="button button-secondary" disabled={query.isFetching} onClick={reload}>
            <FormattedMessage id="source.refresh" />
          </button>
        )}
      </div>
      {user.role !== 'admin' || !user.active ? (
        <SourceError
          error={new APIError('ADMIN_REQUIRED')}
          back="/admin/sources"
          onRetry={reload}
        />
      ) : !validReadActor(user) ? (
        <SourceError
          error={new APIError('READ_CONTEXT_MISMATCH')}
          back="/admin/sources"
          onRetry={reload}
        />
      ) : !valid ? (
        <SourceError
          error={new APIError('INVALID_CURSOR')}
          back="/admin/sources"
          onRetry={reload}
        />
      ) : query.isPending || query.isFetching ? (
        <p role="status">
          <FormattedMessage id="source.loading" />
        </p>
      ) : query.isError ? (
        <SourceError error={query.error} back="/admin/sources" onRetry={reload} />
      ) : (
        <>
          <p className="gallery-note">
            <FormattedMessage id="source.healthNote" />
          </p>
          {query.data.items.length === 0 ? (
            <div className="gallery-message">
              <p>
                <FormattedMessage id="source.empty" />
              </p>
            </div>
          ) : (
            <ul className="job-list">
              {query.data.items.map((source) => (
                <li key={source.id}>
                  <div className="job-entry-heading">
                    <Link
                      className="job-entry-link"
                      to={`/admin/sources/${encodeURIComponent(source.id)}/details`}
                      state={{ from: location.pathname + location.search }}
                      aria-label={intl.formatMessage(
                        { id: 'source.open' },
                        { name: source.root_name },
                      )}
                    >
                      <strong>{source.root_name}</strong>
                      <span>{source.id}</span>
                    </Link>
                    <strong className="job-status">
                      <State state={source.state} />
                    </strong>
                  </div>
                  <p>
                    <FormattedMessage id="source.active" />:{' '}
                    <FormattedMessage id={source.active ? 'source.enabled' : 'source.disabled'} />
                  </p>
                  <p>
                    <FormattedMessage id="source.configured" />:{' '}
                    <FormattedMessage
                      id={source.configured ? 'source.configuredYes' : 'source.configuredNo'}
                    />
                  </p>
                </li>
              ))}
            </ul>
          )}
          <PageNavigation first="/admin/sources" next={next} cursor={cursor} />
          <p className="gallery-note">
            <FormattedMessage id="source.readOnly" />
          </p>
        </>
      )}
    </main>
  );
}

export function SourcePage() {
  const user = useOutletContext<User>();
  const intl = useIntl();
  const { sourceId = '' } = useParams();
  const location = useLocation();
  const [search] = useSearchParams();
  const back = sourceBack((location.state as { from?: unknown } | null)?.from);
  const validID = safeID(sourceId);
  const validSearch = [...search.keys()].length === 0;
  const canRead = user.role === 'admin' && user.active && validReadActor(user);
  const main = usePageFocus(null, sourceId);
  const query = useQuery({
    queryKey: [user.id, user.scope_id, 'admin-source', sourceId],
    queryFn: ({ signal }) => getSource(user, sourceId, signal),
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
    <main className="gallery-main" ref={main} tabIndex={-1} aria-labelledby="source-title">
      <LibraryNavigation />
      <Link className="gallery-back" to={back}>
        <FormattedMessage id="source.back" />
      </Link>
      <div className="gallery-heading">
        <div>
          <h1 id="source-title">
            <FormattedMessage id="source.details" />
          </h1>
          <p>
            <FormattedMessage id="source.healthNote" />
          </p>
        </div>
        {canRead && validID && validSearch && (
          <button className="button button-secondary" disabled={query.isFetching} onClick={reload}>
            <FormattedMessage id="source.refresh" />
          </button>
        )}
      </div>
      {user.role !== 'admin' || !user.active ? (
        <SourceError error={new APIError('ADMIN_REQUIRED')} back={back} onRetry={reload} />
      ) : !validReadActor(user) ? (
        <SourceError error={new APIError('READ_CONTEXT_MISMATCH')} back={back} onRetry={reload} />
      ) : !validID ? (
        <SourceError error={new APIError('NOT_FOUND')} back={back} onRetry={reload} />
      ) : !validSearch ? (
        <SourceError error={new APIError('INVALID_CURSOR')} back={back} onRetry={reload} />
      ) : query.isPending || query.isFetching ? (
        <p role="status">
          <FormattedMessage id="source.loading" />
        </p>
      ) : query.isError ? (
        <SourceError error={query.error} back={back} onRetry={reload} />
      ) : (
        <section className="job-section" aria-labelledby="source-name">
          <h2 id="source-name">{query.data.root_name}</h2>
          <dl className="job-facts">
            <Fact label="source.identifier">{query.data.id}</Fact>
            <Fact label="source.library">{query.data.library_id}</Fact>
            <Fact label="source.active">
              <FormattedMessage id={query.data.active ? 'source.enabled' : 'source.disabled'} />
            </Fact>
            <Fact label="source.configured">
              <FormattedMessage
                id={query.data.configured ? 'source.configuredYes' : 'source.configuredNo'}
              />
            </Fact>
            <Fact label="source.state">
              <State state={query.data.state} />
            </Fact>
            <Fact label="source.generation">{intl.formatNumber(query.data.scan_generation)}</Fact>
            <Fact label="source.albumDepth">
              {intl.formatNumber(query.data.mapping.album_depth)}
            </Fact>
            <Fact label="source.personDepth">
              {intl.formatNumber(query.data.mapping.person_depth)}
            </Fact>
            <Fact label="source.minimum">{intl.formatNumber(query.data.minimum_files)}</Fact>
            <Fact label="source.poll">
              <FormattedMessage
                id="source.seconds"
                values={{ value: intl.formatNumber(query.data.poll_interval_seconds) }}
              />
            </Fact>
            <Fact label="source.fullInterval">
              <FormattedMessage
                id="source.seconds"
                values={{ value: intl.formatNumber(query.data.full_scan_interval_seconds) }}
              />
            </Fact>
            <Fact label="source.checked">
              <JobTime value={query.data.last_checked_at} />
            </Fact>
            <Fact label="source.healthyAt">
              <JobTime value={query.data.last_healthy_at} />
            </Fact>
            <Fact label="source.fullAt">
              <JobTime value={query.data.last_full_scan_at} />
            </Fact>
            {query.data.last_error_code && (
              <Fact label="source.diagnostic">
                {diagnostics.has(query.data.last_error_code) ? (
                  <Diagnostic code={query.data.last_error_code} />
                ) : (
                  <FormattedMessage id="source.otherDiagnostic" />
                )}
              </Fact>
            )}
            <Fact label="source.version">{intl.formatNumber(query.data.version)}</Fact>
            <Fact label="source.configVersion">{intl.formatNumber(query.data.config_version)}</Fact>
          </dl>
          <p className="gallery-note">
            <FormattedMessage id="source.readOnly" />
          </p>
        </section>
      )}
    </main>
  );
}
