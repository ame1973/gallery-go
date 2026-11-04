# Administrative job controls

Job history and online-only controls are available through the HTTPS API. There
is not yet a source-scanning enqueue API, runnable `worker` command, or job UI.
A fresh installation therefore has no jobs. The internal durable queue and worker
executor are implemented and tested separately; synthetic test jobs do not imply
that source ingestion or codec processing is available.

All paths below are relative to `/api/v2`. Sign in as an active administrator as
explained in [authentication.md](authentication.md). Responses use
`Cache-Control: private, no-store`. Ordinary users receive `403 ADMIN_REQUIRED`;
missing, revoked, expired, or disabled sessions receive `401 UNAUTHENTICATED`.
The service rechecks the current account and session inside its transaction.

## Inspect history

| Method | Path                            | Result                                      |
| ------ | ------------------------------- | ------------------------------------------- |
| GET    | `/admin/jobs`                   | Jobs in ascending stable ID order.          |
| GET    | `/admin/jobs/{job_id}`          | One job and its strong `ETag`.              |
| GET    | `/admin/jobs/{job_id}/attempts` | Attempt history in ascending attempt order. |

Lists have a default `limit` of 30 and accept 1..100. Follow `next_cursor` only
while `has_more` is true. The job list accepts one optional `status` filter:
`queued`, `running`, `succeeded`, `failed`, `cancelled`, or `dead`. Omit it for all
states. Cursors bind the account, authorization scope, collection, and canonical
filter/sort parameters. Changing the status filter requires starting a new page
sequence; changing the page limit does not. These are live keyset pages, not an
offline snapshot or a promise to retain a fixed candidate set during mutations.

Public job fields include identity, kind, opaque resource key, state, priority,
cancellation flag, attempt budget, next-run time, counts, safe diagnostics, version,
and UTC timestamps. A job that has never started has `started_at: null`; unfinished
jobs have `finished_at: null`. Private payloads, lease tokens, worker capabilities,
deduplication tokens, filesystem roots, and raw subprocess stderr are not returned.

## Interpret progress correctly

- `total` is the cumulative number of discovered **work units across attempts**,
  not the number of unique media objects in a source.
- `done = succeeded + failed + skipped + cancelled`. Done does not mean success.
- `total_known` is false while the current attempt is discovering its work. Do not
  display a definitive completion percentage during that state.
- Closing a failed, cancelled, or expired attempt marks its unfinished discovered
  units as `cancelled`, never as succeeded. A retry adds new units without resetting
  totals or erasing earlier failures.
- The attempt endpoint reports the **local delta for each attempt**. A successful
  retry can have `status: succeeded` while cumulative job counts still include
  failures or cancelled work from earlier attempts. The job status describes the
  current or latest attempt, not a rewrite of that history.
- `error_code` and `last_error` are nullable, safe diagnostics. Codes include
  `SOURCE_UNAVAILABLE`, `SOURCE_CHANGED`, `IO_ERROR`, `INVALID_MEDIA`,
  `PROCESSING_FAILED`, `JOB_TIMEOUT`, `LEASE_EXPIRED`, `WORKER_STOPPED`, and
  `INTERNAL_ERROR`. They are not authorization capabilities.

## Cancel or retry

Read the job and copy its exact quoted tag, `"job:<id>:<version>"`, to `If-Match`.
Send the configured `Origin`, session-bound `X-CSRF-Token`, and a new
`Idempotency-Key` on either command. **Do not send a request body**, even `{}`.

- `POST /admin/jobs/{job_id}/cancel` returns `200`. Queued work immediately becomes
  cancelled. Running work retains `status: running` with `cancel_requested: true`
  until the worker acknowledges or its lease expires. Cancellation is rechecked
  before publishing business changes; it is not a claim that a blocked NAS syscall
  can always be interrupted immediately.
- `POST /admin/jobs/{job_id}/retry` returns `202` with queued metadata. Only failed,
  dead, or cancelled jobs can be retried. All counters and attempts remain. The
  configured attempt budget is extended, with a ceiling of 100 attempts per job.
  After that ceiling, create a new explicit task when the relevant enqueue API is
  available; never edit job rows directly.

A missing precondition returns `428 PRECONDITION_REQUIRED`; malformed, weak,
wildcard, multiple, or cross-resource tags return `400 INVALID_PRECONDITION`.
Stale versions return `409 VERSION_CONFLICT`. Reload the current job before
retrying an intended new action. Invalid state transitions return
`409 JOB_STATE_CONFLICT`; another active task with the same deduplication key
returns `409 RESOURCE_BUSY`.

Idempotency keys are 16..128 ASCII letters, digits, underscores, or hyphens. UUIDs
are recommended. Receipts last 24 hours and are scoped to the administrator.
Retry a lost response with the same operation, key, and **original** precondition.
The original response is replayed without repeating events or cancelling a newer
attempt. Reusing a key for a different operation or precondition returns
`409 IDEMPOTENCY_CONFLICT`. A replay is historical: use GET for the latest state.

The job change, event/outbox, administrative audit, and successful replay receipt
commit together. A failure rolls them all back. Active deduplication keys are
released on terminal completion so later intentional work is not blocked forever.
Automatic retries use bounded exponential backoff and dead-letter exhausted work;
graceful worker shutdown also respects that budget. None of these controls deletes
source files or purges indexed media.
