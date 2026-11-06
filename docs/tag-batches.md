# Album-tag batch acceptance

Administrators can freeze an explicit selection as a durable queued tag-batch
intent and inspect its historical targets. **This release does not yet register
an executor for `batch_tags`.** Embedded and standalone workers consume native
scan work, not these batches. A `202` response means accepted, never applied or
completed. Batch processing, per-target results, the manager UI, cover batches and
reprocessing remain under development. Use the existing [single-album tag
API](tags.md#versioned-album-association-replacement) when an immediate metadata edit is needed.

All paths below are relative to `/api/v2`. Use an active administrator session,
same-origin requests and the session-bound CSRF token as described in
[authentication.md](authentication.md). Responses are `private, no-store`.
Ordinary accounts receive `403 ADMIN_REQUIRED`; absent, revoked, expired or disabled
sessions receive `401 UNAUTHENTICATED`. Fresh authority is checked inside the
transaction, including before historical receipt replay.

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

- `album_ids` is an explicit array of 1 to 100 unique existing, currently visible
  album IDs. Active library/source and source-library consistency requirements
  apply even to administrators. There is no implicit all-library expansion.
- `tag_ids` is an explicit array of 0 to 100 unique existing tag IDs, not labels.
  `[]` requests replacing the selected associations with an empty set; it never
  deletes catalog tags or source files. Omitted/null arrays are rejected.
- Each opaque ID is at most 64 UTF-8 bytes. Invalid, duplicate, unknown or oversized
  sets are rejected; the strict JSON body limit is 16 KiB. Neither versions,
  private paths, source configuration nor another actor may be supplied in JSON.
- Acceptance freezes each album's **current** version, library/source IDs and
  source configuration epoch in one transaction. There is no client `If-Match`
  precondition for this enqueue operation. This is not a promise to apply against
  a later resource version.
- No source probe, media read, codec command or album mutation occurs during
  acceptance. An unavailable mount does not prevent accepting indexed metadata;
  inactive or inconsistent source registrations do.

A successful response is `202` with the existing safe [Job](jobs.md) DTO:
`kind: "batch_tags"`, initially `status: "queued"`, `attempts: 0`, unknown total
and zero work counters. `Location` points to `/api/v2/admin/jobs/{job_id}` and
`ETag` is the quoted job identity/version. Follow that URL for current status;
the presence of a healthy scan worker is not evidence of batch execution.

### Idempotency and active coalescing

The command key has the same 16-to-128-character ASCII grammar and 24-hour receipt
window as other administrative mutations. Canonical ordering treats reordered
album/tag arrays as the same request; duplicates are not silently removed.

The same current administrator, key and canonical body replay the **original
accepted response**, even after source changes, cancellation or removal of an
original target/tag. The returned historical `status` and `ETag` are not current
job state. A different body with the same actor/key returns
`409 IDEMPOTENCY_CONFLICT`. Another account cannot replay that actor's receipt.

A new key can coalesce with an identical active snapshot for that actor. Changing
selected IDs, desired tags, album versions, source epochs or actor creates a
different intent; existing targets are never expanded. Terminal jobs release their
active deduplication key. Job creation, immutable intent/targets, resource
bindings, job change/outbox, audits and receipt commit or roll back together.
Album versions, actual tag links, policy and account scopes are not changed by
enqueue.

## Inspect accepted targets

```http
GET /api/v2/admin/batches/<job-id>/targets?limit=30
```

The result uses `items`, `has_more` and nullable `next_cursor`. Default limit is 30,
maximum 100. Only `limit` and `cursor` are supported. Stable album-ID keysets are
bound to the current account, scope and job collection; a cursor is not an
access capability. Unknown jobs and jobs without a tag-batch intent return `404`.

Each item contains only:

```json
{
  "album_id": "album-one",
  "album_version": 7,
  "library_id": "library-one",
  "source_id": "source-one",
  "source_config_version": 3,
  "tag_ids": ["tag-one", "tag-two"]
}
```

Both source fields are `null` for an album without a source. An explicit tag clear
remains `[]`, never null. IDs, versions and desired tag IDs remain historical even
if source registrations, albums, tags or the original actor are later removed.
Current administrator authority is still required to read that history. The
response is not a current catalog lookup, a processing-result page, a content
permission or proof that any tag was applied. Filesystem roots, relative paths,
labels, session credentials, payloads and worker lease tokens are not returned.
History is owned by the job and follows its eventual deletion; this API provides
no history-purge operation.

## Cancellation and current limitations

Use the existing versioned [cancel/retry controls](jobs.md#cancel-or-retry), not a
second batch lifecycle. Cancelling a queued batch immediately settles it without
inventing successful work. Editing/removing a bound source cancels its related
active jobs atomically while retaining frozen history. A later new request must
use the new source epoch. An ordinary enqueue retry does not undo cancellation.

There is no claim of per-target application, leased mutation, restart recovery of
applied targets or processing diagnostics in this acceptance-only release. A
future executor must independently fence current actor authority, original target
versions/source epochs and live queue lease, and co-commit real business changes,
truthful progress, complete events, outbox and audit. Accepted IDs or a historical
receipt never substitute for those checks. Source media always remains read-only.
