import type { ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { FormattedMessage, useIntl } from 'react-intl';
import { Link, useLocation, useOutletContext, useParams, useSearchParams } from 'react-router';
import { validReadActor } from '../../lib/api/read-identity';
import { APIError } from '../auth/api';
import type { User } from '../auth/api';
import { safeID } from '../library/api';
import { LibraryNavigation, usePageFocus, useRevalidateReadIdentity } from '../library/common';
import { getJobHistory } from './api';
import {
  Diagnostic,
  HistoryPagination,
  JobError,
  JobTime,
  Kind,
  Status,
  WorkCounts,
  jobPath,
  jobsBack,
  validHistorySearch,
} from './common';

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

export function JobPage() {
  const user = useOutletContext<User>();
  const { jobId = '' } = useParams();
  const location = useLocation();
  const [search] = useSearchParams();
  const attempt = search.get('attempt_cursor');
  const issue = search.get('issue_cursor');
  const back = jobsBack((location.state as { from?: unknown } | null)?.from);
  const validID = safeID(jobId);
  const validSearch = validHistorySearch(search);
  const canRead = user.role === 'admin' && user.active && validReadActor(user);
  const main = usePageFocus(JSON.stringify([attempt, issue]), jobId);
  const intl = useIntl();
  const query = useQuery({
    queryKey: [user.id, user.scope_id, 'admin-job', jobId, attempt, issue],
    queryFn: ({ signal }) => getJobHistory(user, jobId, attempt, issue, signal),
    enabled: canRead && validID && validSearch,
    staleTime: 0,
    gcTime: 0,
    retry: false,
    networkMode: 'always',
    refetchOnReconnect: true,
  });
  useRevalidateReadIdentity(query.error);
  const first = validID ? jobPath(jobId, null, null) : '/admin/jobs';
  const reload = () => {
    main.current?.focus();
    void query.refetch();
  };
  return (
    <main className="gallery-main" ref={main} tabIndex={-1} aria-labelledby="job-title">
      <LibraryNavigation />
      <Link className="gallery-back" to={back}>
        <FormattedMessage id="jobs.back" />
      </Link>
      <div className="gallery-heading">
        <div>
          <h1 id="job-title">
            <FormattedMessage id="jobs.details" />
          </h1>
          <p>
            <FormattedMessage id="jobs.historyNote" />
          </p>
        </div>
        {canRead && validID && validSearch && (
          <button className="button button-secondary" disabled={query.isFetching} onClick={reload}>
            <FormattedMessage id="jobs.refresh" />
          </button>
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
      ) : !validID ? (
        <JobError
          error={new APIError('NOT_FOUND')}
          first="/admin/jobs"
          firstLabel="library.first"
          onRetry={() => {}}
        />
      ) : !validSearch ? (
        <JobError
          error={new APIError('INVALID_CURSOR')}
          first={first}
          firstLabel="jobs.firstHistory"
          state={{ from: back }}
          onRetry={() => {}}
        />
      ) : query.isPending || query.isFetching ? (
        <p role="status">
          <FormattedMessage id="jobs.loading" />
        </p>
      ) : query.isError ? (
        <JobError
          error={query.error}
          first={first}
          firstLabel="jobs.firstHistory"
          missing={back}
          state={{ from: back }}
          onRetry={reload}
        />
      ) : (
        <>
          <section className="job-section" aria-labelledby="job-summary">
            <h2 id="job-summary">
              <Kind kind={query.data.job.kind} />{' '}
              <span className="job-id">{query.data.job.id}</span>
            </h2>
            {query.data.job.status === 'running' && query.data.job.cancel_requested && (
              <p role="status">
                <FormattedMessage id="jobs.cancellationPending" />
              </p>
            )}
            <dl className="job-facts">
              <Fact label="jobs.status">
                <Status status={query.data.job.status} />
              </Fact>
              <Fact label="jobs.resource">
                {query.data.job.resource_key || <FormattedMessage id="jobs.unknown" />}
              </Fact>
              <Fact label="jobs.priority">{intl.formatNumber(query.data.job.priority)}</Fact>
              <Fact label="jobs.attemptBudget">
                {intl.formatNumber(query.data.job.attempts)} /{' '}
                {intl.formatNumber(query.data.job.max_attempts)}
              </Fact>
              <Fact label="jobs.created">
                <JobTime value={query.data.job.created_at} />
              </Fact>
              <Fact label="jobs.updated">
                <JobTime value={query.data.job.updated_at} />
              </Fact>
              <Fact label="jobs.started">
                <JobTime value={query.data.job.started_at} />
              </Fact>
              <Fact label="jobs.finishedAt">
                <JobTime value={query.data.job.finished_at} />
              </Fact>
              {query.data.job.status === 'queued' && (
                <Fact label="jobs.nextRun">
                  <JobTime value={query.data.job.next_run_at} />
                </Fact>
              )}
              {query.data.job.error_code && (
                <Fact label="jobs.diagnostic">
                  <Diagnostic code={query.data.job.error_code} />
                </Fact>
              )}
            </dl>
            <h3>
              <FormattedMessage id="jobs.progress" />
            </h3>
            <p>
              <FormattedMessage id="jobs.countExplanation" />
            </p>
            {!query.data.job.total_known && (
              <p>
                <FormattedMessage id="jobs.totalUnknown" />
              </p>
            )}
            <WorkCounts counts={query.data.job} />
          </section>
          <section className="job-section" aria-labelledby="attempts-title">
            <h2 id="attempts-title">
              <FormattedMessage id="jobs.attempts" />
            </h2>
            <p>
              <FormattedMessage id="jobs.attemptDescription" />
            </p>
            {query.data.attempts.items.length === 0 ? (
              <p>
                <FormattedMessage id="jobs.noAttempts" />
              </p>
            ) : (
              <ol className="job-history">
                {query.data.attempts.items.map((item) => (
                  <li key={item.attempt}>
                    <h3>
                      <FormattedMessage
                        id="jobs.attemptNumber"
                        values={{ number: intl.formatNumber(item.attempt) }}
                      />
                    </h3>
                    <dl className="job-facts">
                      <Fact label="jobs.status">
                        <Status status={item.status} />
                      </Fact>
                      <Fact label="jobs.started">
                        <JobTime value={item.started_at} />
                      </Fact>
                      <Fact label="jobs.finishedAt">
                        <JobTime value={item.finished_at} />
                      </Fact>
                      {item.error_code && (
                        <Fact label="jobs.diagnostic">
                          <Diagnostic code={item.error_code} />
                        </Fact>
                      )}
                    </dl>
                    <WorkCounts counts={item} />
                  </li>
                ))}
              </ol>
            )}
            <HistoryPagination
              id={jobId}
              back={back}
              attempt={attempt}
              issue={issue}
              next={query.data.attempts.next_cursor}
              section="attempt"
            />
          </section>
          <section className="job-section" aria-labelledby="issues-title">
            <h2 id="issues-title">
              <FormattedMessage id="jobs.issues" />
            </h2>
            <p>
              <FormattedMessage id="jobs.issueDescription" />
            </p>
            {query.data.issues.items.length === 0 ? (
              <p>
                <FormattedMessage id="jobs.noIssues" />
              </p>
            ) : (
              <ol className="job-history">
                {query.data.issues.items.map((item) => (
                  <li key={item.id}>
                    <h3>
                      <FormattedMessage
                        id="jobs.attemptNumber"
                        values={{ number: intl.formatNumber(item.attempt) }}
                      />
                    </h3>
                    <dl className="job-facts">
                      <Fact label="jobs.source">{item.source_id}</Fact>
                      <Fact label="jobs.sourceVersion">
                        {intl.formatNumber(item.source_config_version)}
                      </Fact>
                      <Fact label="jobs.generation">
                        {item.generation_id ?? <FormattedMessage id="jobs.unknown" />}
                      </Fact>
                      <Fact label="jobs.path">
                        {item.relative_path ?? <FormattedMessage id="jobs.unknown" />}
                      </Fact>
                      <Fact label="jobs.diagnostic">
                        <Diagnostic code={item.error_code} />
                      </Fact>
                      <Fact label="jobs.created">
                        <JobTime value={item.created_at} />
                      </Fact>
                    </dl>
                  </li>
                ))}
              </ol>
            )}
            <HistoryPagination
              id={jobId}
              back={back}
              attempt={attempt}
              issue={issue}
              next={query.data.issues.next_cursor}
              section="issue"
            />
          </section>
          <p className="gallery-note">
            <FormattedMessage id="jobs.readOnly" />
          </p>
        </>
      )}
    </main>
  );
}
