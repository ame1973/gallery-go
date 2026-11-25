import { useQuery } from '@tanstack/react-query';
import { FormattedMessage, useIntl } from 'react-intl';
import { Link, useLocation, useOutletContext, useSearchParams } from 'react-router';
import { validReadActor } from '../../lib/api/read-identity';
import { APIError } from '../auth/api';
import type { User } from '../auth/api';
import {
  LibraryNavigation,
  PageNavigation,
  usePageFocus,
  useRevalidateReadIdentity,
} from '../library/common';
import { listJobs, statuses } from './api';
import { Diagnostic, JobError, Kind, Status, jobPath, validJobsSearch } from './common';

export function JobsPage() {
  const user = useOutletContext<User>();
  const intl = useIntl();
  const location = useLocation();
  const [search, setSearch] = useSearchParams();
  const cursor = search.get('cursor');
  const status = statuses.find((value) => value === search.get('status')) ?? '';
  const valid = validJobsSearch(search);
  const canRead = user.role === 'admin' && user.active && validReadActor(user);
  const main = usePageFocus(cursor, status);
  const query = useQuery({
    queryKey: [user.id, user.scope_id, 'admin-jobs', status, cursor],
    queryFn: ({ signal }) => listJobs(user, status, cursor, signal),
    enabled: canRead && valid,
    staleTime: 0,
    gcTime: 0,
    retry: false,
    networkMode: 'always',
    refetchOnReconnect: true,
  });
  useRevalidateReadIdentity(query.error);
  const firstParams = new URLSearchParams();
  if (status) firstParams.set('status', status);
  const first = `/admin/jobs${firstParams.size ? `?${firstParams}` : ''}`;
  const nextParams = new URLSearchParams(firstParams);
  if (query.data?.next_cursor) nextParams.set('cursor', query.data.next_cursor);
  const next = query.data?.has_more && query.data.next_cursor ? `/admin/jobs?${nextParams}` : null;
  const reload = () => {
    main.current?.focus();
    void query.refetch();
  };
  return (
    <main className="gallery-main" ref={main} tabIndex={-1} aria-labelledby="jobs-title">
      <LibraryNavigation />
      <div className="gallery-heading">
        <div>
          <h1 id="jobs-title">
            <FormattedMessage id="jobs.title" />
          </h1>
          <p>
            <FormattedMessage id="jobs.description" />
          </p>
        </div>
        {canRead && (
          <div className="job-controls">
            <label className="gallery-sort">
              <span>
                <FormattedMessage id="jobs.statusFilter" />
              </span>
              <select
                value={status}
                onChange={(event) => {
                  const selected = statuses.find((value) => value === event.target.value);
                  setSearch(selected ? { status: selected } : {});
                }}
              >
                <option value="">{intl.formatMessage({ id: 'jobs.allStatuses' })}</option>
                {statuses.map((value) => (
                  <option value={value} key={value}>
                    {intl.formatMessage({ id: `jobs.${value}` })}
                  </option>
                ))}
              </select>
            </label>
            {valid && (
              <button
                className="button button-secondary"
                disabled={query.isFetching}
                onClick={reload}
              >
                <FormattedMessage id="jobs.refresh" />
              </button>
            )}
          </div>
        )}
      </div>
      {user.role !== 'admin' || !user.active ? (
        <JobError
          error={new APIError('ADMIN_REQUIRED')}
          first="/admin/jobs"
          firstLabel="library.first"
          onRetry={() => {}}
        />
      ) : !validReadActor(user) ? (
        <JobError
          error={new APIError('READ_CONTEXT_MISMATCH')}
          first="/admin/jobs"
          firstLabel="library.first"
          onRetry={reload}
        />
      ) : !valid ? (
        <JobError
          error={new APIError('INVALID_CURSOR')}
          first="/admin/jobs"
          firstLabel="library.first"
          onRetry={() => {}}
        />
      ) : query.isPending || query.isFetching ? (
        <p role="status">
          <FormattedMessage id="jobs.loading" />
        </p>
      ) : query.isError ? (
        <JobError error={query.error} first={first} firstLabel="library.first" onRetry={reload} />
      ) : (
        <>
          <p className="gallery-note">
            <FormattedMessage id="jobs.countExplanation" />
          </p>
          {query.data.items.length === 0 ? (
            <div className="gallery-message">
              <p>
                <FormattedMessage id="jobs.empty" />
              </p>
            </div>
          ) : (
            <ul className="job-list">
              {query.data.items.map((job) => (
                <li key={job.id}>
                  <div className="job-entry-heading">
                    <Link
                      className="job-entry-link"
                      to={jobPath(job.id, null, null)}
                      state={{ from: location.pathname + location.search }}
                      aria-label={intl.formatMessage({ id: 'jobs.open' }, { id: job.id })}
                    >
                      <strong>
                        <Kind kind={job.kind} />
                      </strong>
                      <span>{job.id}</span>
                    </Link>
                    <strong className="job-status">
                      <Status status={job.status} />
                    </strong>
                  </div>
                  <p>
                    <FormattedMessage
                      id="jobs.progressSummary"
                      values={{
                        done: intl.formatNumber(job.done),
                        total: intl.formatNumber(job.total),
                      }}
                    />
                  </p>
                  {!job.total_known && (
                    <p>
                      <FormattedMessage id="jobs.totalUnknown" />
                    </p>
                  )}
                  {job.status === 'running' && job.cancel_requested && (
                    <p>
                      <FormattedMessage id="jobs.cancellationPending" />
                    </p>
                  )}
                  {job.error_code && (
                    <p>
                      <Diagnostic code={job.error_code} />
                    </p>
                  )}
                </li>
              ))}
            </ul>
          )}
          <PageNavigation first={first} next={next} cursor={cursor} />
          <p className="gallery-note">
            <FormattedMessage id="jobs.readOnly" />
          </p>
        </>
      )}
    </main>
  );
}
