# Administrative job controls

Durable scan enqueue, history, and online-only controls are available through the
HTTPS API. Embedded and standalone [native workers](workers.md) can consume accepted
scan intents and schedule active sources by policy. Job UI is **not available yet**.
A `202` response is
not successful ingestion or codec processing. Source registration, folder
mapping, and separate reachability checks are in [sources.md](sources.md).
[Tag-batch acceptance](tag-batches.md) additionally freezes explicit album/tag
snapshots using this queue and its cancel/retry controls. There is no registered
`batch_tags` executor yet; those accepted jobs remain queued and do not apply tags.

All paths below are relative to `/api/v2`. Sign in as an active administrator as
explained in [authentication.md](authentication.md). Responses use
`Cache-Control: private, no-store`. Ordinary users receive `403 ADMIN_REQUIRED`;
missing, revoked, expired, or disabled sessions receive `401 UNAUTHENTICATED`.
The service rechecks the current account and session inside its transaction.

## Enqueue a scan intent

`POST /admin/scans` requires Origin, session-bound CSRF, and a new
`Idempotency-Key`. Its strict JSON body accepts only:

| Field       | Meaning                                                                               |
| ----------- | ------------------------------------------------------------------------------------- |
| `mode`      | Required, exactly `full` or `incremental`.                                            |
| `source_id` | Optional active registered source ID, or a matching constraint on the selected album. |
| `album_id`  | Optional source-backed indexed album ID defining the target scope.                    |

Omit or explicitly null both IDs to select all currently active registered sources:

```json
{ "mode": "full", "source_id": null, "album_id": null }
```

For one source use `{ "mode": "incremental", "source_id": "<source-id>" }`;
for a target use `{ "mode": "full", "album_id": "<album-id>" }`. If both IDs are
provided, the source must own the album. Empty IDs are invalid, never all-source
shortcuts. IDs are opaque references, not filesystem paths. Inactive/unmapped
album selections return `422 VALIDATION_FAILED`; unknown IDs return 404; an all-source
selection with no active registrations returns `409 SCAN_NO_SOURCES`.

A successful request returns `202`, job metadata, `Location`, and a strong job
`ETag`. Source/configuration/target snapshots and dependencies are frozen in private
rows with the job, event/outbox, audit, and receipt. Large all-source sets do not
inflate private job JSON. Enqueue never probes files, creates missing mount markers,
advances inventory generations, or bypasses worker health/minimum/config/lease gates.

Identical active mode/selection/target/configuration intents coalesce. Full versus
incremental, source versus all-source versus album, and changed source epochs remain
separate; scan never consumes a reprocess intent. Coalescing cannot expand an old
job's target set. Explicit null and omitted IDs have identical selection semantics;
other request fields remain non-null unless their own contract explicitly opts in.

Receipts are actor-scoped for 24 hours. Repeat the original body/key after a lost
response to receive its historical accepted result, even if the job was later
cancelled or a source registration removed. GET the job for its current state.
Reusing the key for a changed command returns `409 IDEMPOTENCY_CONFLICT`.

The queue handler executes bounded inventory, native image/controlled video metadata,
and indexed publication for versioned intents. It rejects unpinned or malformed stored targets,
waits for a live source owner without inventing work, and continues healthy sources
when another source fails. Failure diagnostics, generation state, and progress commit
atomically; retryable source failures use the queue's bounded backoff/dead-letter policy.
`serve` enables a scan consumer by default; set `worker.embedded: false` for a
standalone consumer, or explicitly use [metadata-only mode](health.md) for maintenance.
Queued acceptance, including
for an unavailable/unconfigured source, is not evidence that bytes are reachable,
indexed, or decoded. `scheduler.enabled: true` dispatches initial/periodic full
scans and intervening source polls. Identical active manual/scheduled source intents
coalesce; mode, selection, configuration and reprocess boundaries stay distinct.
System dispatch creates no administrator session or replay receipt. [Readiness](health.md)
checks committed worker/source evidence without NAS I/O in HTTP. Native indexed crash
regressions kill an actual compiled worker during staging and after one source's
nonterminal publication, close all old DB connections, and verify reopened recovery:
failed lease-expired history, a live successor fencing the old owner, stable logical
IDs/revisions, and no duplicate versioned changes or missing outbox events.

## Inspect history

| Method | Path                            | Result                                                           |
| ------ | ------------------------------- | ---------------------------------------------------------------- |
| GET    | `/admin/jobs`                   | Jobs in ascending stable ID order.                               |
| GET    | `/admin/jobs/{job_id}`          | One job and its strong `ETag`.                                   |
| GET    | `/admin/jobs/{job_id}/attempts` | Attempt history in ascending attempt order.                      |
| GET    | `/admin/jobs/{job_id}/issues`   | Safe committed source/entry scan diagnostics in stable ID order. |

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

A job can internally depend on several sources. Those durable dependency keys are
private, are bound only in the job's creation transaction, and cannot be added to
an existing/coalesced intent. A source configuration edit or empty-source deletion
atomically requests cancellation of every related queued/running parent job, not
only jobs whose primary resource key names that source. Deletion retains the job's
dependency and attempt history. Unrelated source jobs remain untouched.

## Inspect per-entry diagnostics

`GET /admin/jobs/{job_id}/issues` complements the aggregate job/attempt summaries.
It currently records scan source and entry failures or deferred identity decisions.
Only the current administrator can read it; ordinary media visibility is not an
operations grant. An unknown job returns `404`, while an existing job with no recorded
diagnostics returns `{ "items": [], "next_cursor": null, "has_more": false }`.

Each item contains:

- Opaque `id`, the historical `attempt`, `source_id` and `source_config_version`.
  These identify the processed configuration, not current reachability or a grant
  to source bytes. A source registration may no longer exist.
- Nullable `generation_id`: null means the source failed before an inventory
  generation was created. A non-null ID is historical even if private staging was
  later removed; it is not a worker capability or a public filesystem link.
- Nullable `relative_path`: a validated root-relative entry name, or `.` for a
  source-level issue. Absolute/unsafe/unrepresentable, control-bearing or excessive
  text is null, not truncated, normalized into another filename or exposed as a root.
- A stable `error_code`, a fixed safe English `message`, and UTC `created_at`.
  Unknown stored codes become `PROCESSING_FAILED`; raw process stderr, SQL errors,
  root/marker/pin paths, lease owners, payloads and EXIF maps are never returned.

Common entry codes include `INVALID_MEDIA`, `CODEC_UNAVAILABLE`, `MEDIA_LIMIT`,
`UNSAFE_ENTRY`, `SYMLINK_CYCLE`, `AMBIGUOUS_RENAME`, `AMBIGUOUS_ALBUM_RENAME` and
`TARGET_RENAME_OUTSIDE_SCOPE`. Source checks may report `SOURCE_UNAVAILABLE`,
`SOURCE_CHANGED`, `SOURCE_NOT_CONFIGURED`, `IO_ERROR`, `SCAN_LIMIT` or `MINIMUM_FILES`.
A deferred move is not proof that a logical identity was successfully reassigned.

Only `limit` and `cursor` are supported; other filters return `400 INVALID_FILTER`.
The normal default of 30 and maximum of 100 apply. Cursors bind account, scope and **this job's
issue collection**. This is live committed keyset history, not an immutable offline
snapshot. Revisit the first page when the job changes or reaches its terminal state.
Issue count cannot be used as `failed`, `done` or unique-media count: one input can
have several diagnostics and one attempt performs several kinds of work units.

Issue history shares the staging/source-failure transaction, including live lease,
source configuration, progress, event/outbox and audit checks. A rollback cannot
leave a public issue from uncommitted work. Retries retain earlier attempts and
immutable issue IDs. Source removal may discard private inventory staging but never
this job-owned projection. Migration backfills **available** private issues; missing
historical causes that were never recorded or already removed are not fabricated.
Reading diagnostics performs no source filesystem/probe I/O and never deletes,
changes, downloads or purges source files. Full job/issue management screens remain
under development; this API does not complete the operations UI.

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
