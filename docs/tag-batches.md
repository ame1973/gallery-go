# Album-tag batch processing

Administrators can freeze a bounded selection, let embedded or standalone workers
apply its album-tag replacement, and inspect committed per-target results. A `202`
response means accepted, **not applied or completed**. Targets commit independently;
a batch can partially apply before a failure or cancellation. The manager UI,
cover batches and media reprocessing remain under development. The existing
[single-album tag API](tags.md#versioned-album-association-replacement) remains
available for synchronous versioned edits.

All paths below are relative to `/api/v2`. Administrative HTTP requests require an
active administrator session, same origin and session-bound CSRF protections as
applicable; see [authentication.md](authentication.md). Responses are
`private, no-store`. Ordinary accounts receive `403 ADMIN_REQUIRED`; absent,
revoked, expired or disabled sessions receive `401 UNAUTHENTICATED`. Fresh session
and account authority is checked inside each API transaction, including receipt
replay. Accepted worker authority is described separately below.

## Accept a bounded intent

```http
POST /api/v2/admin/batches/tags
Content-Type: application/json
Origin: https://gallery.example.test
X-CSRF-Token: <session-token>
Idempotency-Key: <new-unique-command-key>

{
  "album_ids": ["album-one", "album-two"],
  "tag_ids": ["tag-one", "tag-two"]
}
```

- `album_ids` is an explicit array of 1 to 100 unique currently visible album IDs.
  Active library/source and source-library consistency apply even to administrators.
  There is no implicit all-library expansion.
- `tag_ids` is an explicit array of 0 to 100 unique existing tag IDs, not labels.
  `[]` replaces associations with an empty set; it never deletes catalog tags or
  source files. Omitted/null arrays are rejected.
- Each ID is at most 64 UTF-8 bytes. Invalid, duplicate, unknown or oversized sets
  are rejected; the strict JSON body limit is 16 KiB. Versions, private paths,
  source configuration and another actor cannot be supplied in the body.
- Enqueue freezes each album's **current** version, library/source IDs and source
  configuration epoch in one transaction. There is no client `If-Match` for this
  operation. Changed targets are not silently retargeted during execution.
- Enqueue performs no source probe, media read, codec command or album mutation.
  Batch processing is also DB-only: an unavailable mount does not prevent editing
  indexed metadata, but inactive or inconsistent registrations fail closed.

The `202` response uses the existing safe [Job](jobs.md) DTO: `kind: "batch_tags"`,
initially `status: "queued"`, `attempts: 0`, unknown total and zero work counters.
`Location` points to `/api/v2/admin/jobs/{job_id}`; `ETag` is its quoted identity and
version. Follow that URL for current status, then inspect results below. A worker
heartbeat alone is not proof that this job completed. With no compatible consumer
running, the job remains queued rather than fabricating progress.

### Idempotency and active coalescing

The command key uses the existing 16-to-128-character ASCII grammar and 24-hour
receipt window. Canonical ordering treats reordered arrays as the same request;
duplicates are rejected, not silently removed.

The same current administrator, key and canonical body replay the **original
accepted response**, even after execution, cancellation, source changes or target/
tag removal. Its historical `status` and `ETag` are not current state. A different
body with that actor/key returns `409 IDEMPOTENCY_CONFLICT`. Another account cannot
replay the original actor's receipt.

A new key can coalesce with an identical active snapshot for that actor. Changed
selection, desired tags, album versions, source epochs or actor create a different
intent. Existing targets never expand; terminal jobs release their active dedupe
key. Intent, targets, resource bindings, job change/outbox, audits and receipt commit
or roll back together. Enqueue itself does not change album metadata or scopes.

## Execution and durable authority

Both [worker modes](workers.md) consume `batch_tags` on the existing queue, with the
same concurrency, lease, heartbeat, retry and cancellation policy as other work.
There is no second queue or scheduler. Workers do not require a web listener or
assets and never write source files.

The accepted private intent identifies its original administrator. Before each
unresolved target, the worker checks that this **current account** still exists,
is active and has the administrator role. The accepted operation is durable: it
can outlive logout, session expiration or session revocation. No session token or
password is saved or forged. Disabling, demoting or removing the account prevents
further unresolved target changes. Retrying through another administrator does
not replace the original actor or repair their authority implicitly.

Each unresolved target also requires its frozen album version, library/source
identity and source configuration epoch to match current shared SQL visibility.
The live lease owner, attempt, expiry and cancellation flag are checked in the
same writer transaction and again before commit. Tags must still exist. A genuine
change commits links, album version, complete album upsert/outbox, audits, result
and job progress atomically. A no-op leaves album version/events unchanged but
records a real skipped work unit and result. Scope/grant/download policy never
widens as a side effect.

### Counts and retries

Each attempt reserves exactly one bounded budget for all accepted targets. Job
counters are cumulative **work units across attempts**, not unique album counts:

| Outcome           | Counter     | Meaning                                                             |
| ----------------- | ----------- | ------------------------------------------------------------------- |
| `applied`         | `succeeded` | Links and album version actually changed.                           |
| `unchanged`       | `skipped`   | The version-fenced target already had the desired set.              |
| `already_applied` | `skipped`   | A previous attempt permanently resolved this target.                |
| `failed`          | `failed`    | A safe per-target rejection occurred; no target mutation committed. |

Both `applied` and `unchanged` permanently resolve that frozen target. A retry does
not apply it again, even if another later command changed or removed it; the result
reports the original resolved version. The retry still records one truthful
`already_applied` skipped unit. It never overwrites the later edit to make the old
request appear current.

Interruption/expiry closes unfinished attempt budget as `cancelled`, never
`succeeded`. The next attempt adds a new budget and retains previous counters and
history. A successful latest attempt can therefore coexist with cumulative failed
or cancelled units. For example, a 100-target job killed after 37 committed changes
can finish its second attempt with total/done 200, succeeded 100, skipped 37 and
cancelled 63. Read [attempt history](jobs.md#inspect-history), not only aggregate
percentages.

Per-target validation failures finish that attempt as failed while other valid
targets continue. Transient processing/storage failures follow the queue's bounded
retry/backoff policy. A changed frozen version/epoch is not refreshed by retry;
review current state and create a new intent when appropriate.

## Inspect accepted targets

```http
GET /api/v2/admin/batches/<job-id>/targets?limit=30
```

This historical selection page uses `items`, `has_more` and nullable `next_cursor`.
Default limit is 30, maximum 100; only `limit` and `cursor` are supported. Stable
album-ID keysets bind the current account, scope and job collection. Each item has
`album_id`, `album_version`, `library_id`, nullable `source_id` and
`source_config_version`, and explicit desired `tag_ids`. Both source fields are
null for a source-less album. An explicit clear stays `[]`, not null.

IDs, versions and desired tags remain historical after resource, tag or original
actor removal. They do not prove current visibility, tag existence, application or
content permission. Unknown jobs and jobs without tag-batch intent return `404`.

## Inspect committed results

```http
GET /api/v2/admin/batches/<job-id>/results?limit=30
```

Current administrators receive stable result-ID keysets across recorded attempts,
using the same bounded page envelope and account/scope/job cursor binding. Only
`limit` and `cursor` are supported; there is no attempt filter. The same album may
appear in several attempts. An accepted batch without committed target results
returns an empty page; unknown/non-batch jobs return `404`.

Each result has `id`, `album_id`, `attempt`, `outcome`, nullable `album_version`,
nullable `error_code`/`message`, and `created_at`. A non-failure's version is its
**historical resolution**, not a current ETag. Failures have no applied version:

- `ACTOR_NOT_AUTHORIZED`: the original administrator is no longer active/authorized.
- `TARGET_CHANGED`: the album/source changed, disappeared or became unavailable to
  the shared SQL predicate. Create a reviewed new intent instead of guessing.
- `TAG_NOT_FOUND`: an accepted tag ID no longer exists.
- `PROCESSING_FAILED`: safe fallback for an unknown stored code.

Messages are fixed safe English text, never raw SQL, paths or subprocess errors.
No row means **no committed target outcome**, not success. Queued cancellation or
an interrupted attempt can leave targets without result rows while aggregate
unfinished budget is truthfully cancelled. History remains readable to current
administrators after resource/account removal; it is owned by the job. No history
purge endpoint, filesystem roots/relative paths, session credentials or lease
capabilities are exposed.

## Cancellation and remaining scope

Use the versioned [cancel/retry controls](jobs.md#cancel-or-retry), not a second
batch lifecycle. Queued cancellation settles immediately; running cancellation
fences subsequent commits. Already committed target changes remain. Successful
source edits/removal cancel related active jobs atomically and retain history.
Replaying an enqueue receipt does not undo cancellation; retry does not refresh
frozen targets or overwrite prior resolutions.

This implements backend tag-batch execution/results, not a full media-manager UI,
cover batch, codec reprocessing, variant rendering or full-product acceptance.
Source media remains read-only throughout.
