import { FormattedMessage, useIntl } from 'react-intl';
import { Link } from 'react-router';
import { APIError } from '../auth/api';
import type { Attempt, Job, JobStatus } from './api';
import { statuses } from './api';
import { jobMessages } from './messages';

function validCursor(value: string | null) {
  return value === null || (value.length > 0 && value.length <= 512);
}

export function validJobsSearch(search: URLSearchParams) {
  const status = search.get('status');
  return (
    [...search.keys()].every((key) => key === 'status' || key === 'cursor') &&
    search.getAll('status').length <= 1 &&
    (status === null || statuses.some((value) => value === status)) &&
    search.getAll('cursor').length <= 1 &&
    validCursor(search.get('cursor'))
  );
}

export function validHistorySearch(search: URLSearchParams) {
  return (
    [...search.keys()].every((key) => key === 'attempt_cursor' || key === 'issue_cursor') &&
    search.getAll('attempt_cursor').length <= 1 &&
    search.getAll('issue_cursor').length <= 1 &&
    validCursor(search.get('attempt_cursor')) &&
    validCursor(search.get('issue_cursor'))
  );
}

export function jobsBack(from: unknown) {
  if (typeof from !== 'string' || from.includes('#')) return '/admin/jobs';
  const question = from.indexOf('?');
  const path = question < 0 ? from : from.slice(0, question);
  const search = new URLSearchParams(question < 0 ? '' : from.slice(question + 1));
  return path === '/admin/jobs' && validJobsSearch(search) ? from : '/admin/jobs';
}

export function jobPath(id: string, attempt: string | null, issue: string | null) {
  const search = new URLSearchParams();
  if (attempt) search.set('attempt_cursor', attempt);
  if (issue) search.set('issue_cursor', issue);
  return `/admin/jobs/${encodeURIComponent(id)}/details${search.size ? `?${search}` : ''}`;
}

export function Status({ status }: { status: JobStatus }) {
  return <FormattedMessage id={`jobs.${status}`} />;
}

export function Kind({ kind }: { kind: string }) {
  const key =
    kind === 'scan'
      ? 'jobs.scan'
      : kind === 'batch_tags'
        ? 'jobs.batchTags'
        : kind === 'batch_covers'
          ? 'jobs.batchCovers'
          : null;
  return key ? <FormattedMessage id={key} /> : kind;
}

export function Diagnostic({ code }: { code: string | null }) {
  if (!code) return null;
  const key = `jobs.error.${code}`;
  return (
    <span>
      <code>{code}</code>{' '}
      <FormattedMessage id={Object.hasOwn(jobMessages.en, key) ? key : 'jobs.unknownDiagnostic'} />
    </span>
  );
}

export function JobTime({ value }: { value: string | null }) {
  const intl = useIntl();
  if (!value || !Number.isFinite(Date.parse(value))) return <FormattedMessage id="jobs.unknown" />;
  const time = intl.formatDate(value, {
    dateStyle: 'medium',
    timeStyle: 'medium',
    timeZone: 'UTC',
  });
  return (
    <time dateTime={value}>
      <FormattedMessage id="jobs.utc" values={{ time }} />
    </time>
  );
}

type Counts = Pick<
  Job | Attempt,
  'total' | 'done' | 'succeeded' | 'failed' | 'skipped' | 'cancelled'
>;
const countFields = ['total', 'done', 'succeeded', 'failed', 'skipped', 'cancelled'] as const;

export function WorkCounts({ counts }: { counts: Counts }) {
  const intl = useIntl();
  return (
    <dl className="job-counts">
      {countFields.map((field) => (
        <div key={field}>
          <dt>
            <FormattedMessage id={`jobs.${field}`} />
          </dt>
          <dd>{intl.formatNumber(counts[field])}</dd>
        </div>
      ))}
    </dl>
  );
}

export function JobError({
  error,
  first,
  firstLabel,
  missing = first,
  state,
  onRetry,
}: {
  error: unknown;
  first: string;
  firstLabel: string;
  missing?: string;
  state?: { from: string };
  onRetry: () => void;
}) {
  const code = error instanceof APIError ? error.code : '';
  const invalid = ['INVALID_CURSOR', 'INVALID_FILTER', 'INVALID_PAGINATION'].includes(code);
  const message =
    code === 'ADMIN_REQUIRED' || code === 'FORBIDDEN'
      ? 'jobs.denied'
      : code === 'UNAUTHENTICATED'
        ? 'auth.sessionExpired'
        : code === 'NOT_FOUND'
          ? 'jobs.notFound'
          : invalid
            ? 'jobs.invalidPage'
            : code === 'VERSION_CONFLICT' || code === 'READ_CONTEXT_MISMATCH'
              ? 'auth.readContextChanged'
              : 'jobs.error';
  return (
    <div className="gallery-message" role="alert">
      <p>
        <FormattedMessage id={message} />
      </p>
      {invalid || code === 'NOT_FOUND' ? (
        <Link className="button button-secondary" to={invalid ? first : missing} state={state}>
          <FormattedMessage id={invalid ? firstLabel : 'jobs.back'} />
        </Link>
      ) : code === 'ADMIN_REQUIRED' || code === 'FORBIDDEN' || code === 'UNAUTHENTICATED' ? null : (
        <button className="button button-secondary" onClick={onRetry}>
          <FormattedMessage id="library.refresh" />
        </button>
      )}
    </div>
  );
}

export function HistoryPagination({
  id,
  back,
  attempt,
  issue,
  next,
  section,
}: {
  id: string;
  back: string;
  attempt: string | null;
  issue: string | null;
  next: string | null;
  section: 'attempt' | 'issue';
}) {
  const intl = useIntl();
  const current = section === 'attempt' ? attempt : issue;
  if (!current && !next) return null;
  const path = (cursor: string | null) =>
    jobPath(id, section === 'attempt' ? cursor : attempt, section === 'issue' ? cursor : issue);
  return (
    <nav
      className="gallery-pagination job-pagination"
      aria-label={intl.formatMessage({
        id: section === 'attempt' ? 'jobs.attemptPages' : 'jobs.issuePages',
      })}
    >
      {current && (
        <Link className="button button-secondary" to={path(null)} state={{ from: back }}>
          <FormattedMessage id="library.first" />
        </Link>
      )}
      {next && (
        <Link className="button button-secondary" to={path(next)} state={{ from: back }}>
          <FormattedMessage id="library.next" />
        </Link>
      )}
    </nav>
  );
}
