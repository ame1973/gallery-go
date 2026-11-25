import { api } from '../../lib/api/client';
import { checkReadEcho, identityHeaders } from '../../lib/api/read-identity';
import type { ReadActor } from '../../lib/api/read-identity';
import type { components } from '../../lib/api/schema';
import { APIError } from '../auth/api';
import { safeID } from '../library/api';

export type Job = components['schemas']['Job'];
export type JobStatus = components['schemas']['JobStatus'];
export type Attempt = components['schemas']['JobAttempt'];
export type Issue = components['schemas']['JobIssue'];

export const statuses: JobStatus[] = [
  'queued',
  'running',
  'succeeded',
  'failed',
  'cancelled',
  'dead',
];
const attemptStatuses: Attempt['status'][] = ['running', 'succeeded', 'failed', 'cancelled'];

function validPage(page: { items: unknown[]; has_more: boolean; next_cursor: string | null }) {
  return (
    Array.isArray(page.items) &&
    page.items.length <= 30 &&
    (page.has_more === false
      ? page.next_cursor === null
      : page.has_more === true &&
        typeof page.next_cursor === 'string' &&
        page.next_cursor.length > 0 &&
        page.next_cursor.length <= 512)
  );
}

function validCounts(
  counts: Pick<Job, 'total' | 'done' | 'succeeded' | 'failed' | 'skipped' | 'cancelled'>,
) {
  const values = [
    counts.total,
    counts.done,
    counts.succeeded,
    counts.failed,
    counts.skipped,
    counts.cancelled,
  ];
  const sum = counts.succeeded + counts.failed + counts.skipped + counts.cancelled;
  return (
    values.every((value) => Number.isSafeInteger(value) && value >= 0) &&
    Number.isSafeInteger(sum) &&
    counts.done === sum &&
    counts.done <= counts.total
  );
}

function validJob(job: Job) {
  return (
    safeID(job.id) &&
    statuses.includes(job.status) &&
    typeof job.total_known === 'boolean' &&
    typeof job.cancel_requested === 'boolean' &&
    Number.isSafeInteger(job.priority) &&
    Number.isSafeInteger(job.attempts) &&
    Number.isSafeInteger(job.max_attempts) &&
    job.attempts >= 0 &&
    job.max_attempts >= 1 &&
    job.max_attempts <= 100 &&
    job.max_attempts >= job.attempts &&
    validCounts(job)
  );
}

type Failure = { response: Response; error?: components['schemas']['Problem'] };

function failure(result: Failure): never {
  const code =
    result.response.status === 401
      ? 'UNAUTHENTICATED'
      : result.response.status === 403
        ? 'ADMIN_REQUIRED'
        : result.response.status === 404
          ? 'NOT_FOUND'
          : result.response.status === 409
            ? 'VERSION_CONFLICT'
            : result.error?.code;
  throw new APIError(code ?? 'INTERNAL_ERROR', result.error?.request_id);
}

export async function listJobs(
  actor: ReadActor,
  status: JobStatus | '',
  cursor: string | null,
  signal: AbortSignal,
) {
  const result = await api.GET('/admin/jobs', {
    signal,
    headers: identityHeaders(actor),
    params: { query: { limit: 30, ...(status && { status }), ...(cursor && { cursor }) } },
  });
  if (!result.data) return failure(result);
  checkReadEcho(result.response, actor);
  if (!validPage(result.data) || result.data.items.some((job) => !validJob(job))) {
    throw new APIError('INVALID_JOB_RESPONSE');
  }
  return result.data;
}

export async function getJobHistory(
  actor: ReadActor,
  id: string,
  attemptCursor: string | null,
  issueCursor: string | null,
  signal: AbortSignal,
) {
  const headers = identityHeaders(actor);
  const [job, attempts, issues] = await Promise.all([
    api.GET('/admin/jobs/{job_id}', { signal, headers, params: { path: { job_id: id } } }),
    api.GET('/admin/jobs/{job_id}/attempts', {
      signal,
      headers,
      params: {
        path: { job_id: id },
        query: { limit: 30, ...(attemptCursor && { cursor: attemptCursor }) },
      },
    }),
    api.GET('/admin/jobs/{job_id}/issues', {
      signal,
      headers,
      params: {
        path: { job_id: id },
        query: { limit: 30, ...(issueCursor && { cursor: issueCursor }) },
      },
    }),
  ]);
  if (!job.data) return failure(job);
  if (!attempts.data) return failure(attempts);
  if (!issues.data) return failure(issues);
  checkReadEcho(job.response, actor);
  checkReadEcho(attempts.response, actor);
  checkReadEcho(issues.response, actor);
  if (
    job.data.id !== id ||
    !validJob(job.data) ||
    !validPage(attempts.data) ||
    !validPage(issues.data) ||
    attempts.data.items.some(
      (attempt) =>
        !Number.isSafeInteger(attempt.attempt) ||
        attempt.attempt < 1 ||
        attempt.attempt > 100 ||
        !attemptStatuses.includes(attempt.status) ||
        !validCounts(attempt),
    ) ||
    issues.data.items.some(
      (issue) =>
        !safeID(issue.id) ||
        !Number.isSafeInteger(issue.attempt) ||
        issue.attempt < 1 ||
        issue.attempt > 100 ||
        !Number.isSafeInteger(issue.source_config_version) ||
        issue.source_config_version < 1,
    )
  ) {
    throw new APIError('INVALID_JOB_RESPONSE');
  }
  return { job: job.data, attempts: attempts.data, issues: issues.data };
}
