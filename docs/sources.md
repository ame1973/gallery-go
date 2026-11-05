# Read-only source configuration

Administrators can register declared source roots, configure folder mapping and
safety policies, and inspect root reachability through the HTTPS API. They can
also [enqueue durable scan intents](jobs.md), which the embedded or standalone
[native worker](workers.md) can execute. Enabled consumers also schedule active
registrations automatically. Management screens are **not available yet**.
Registration and reachability endpoints do no synchronous scanning; background
policy dispatch is separate. A fresh installation has no indexed media until
healthy scan publication.

All paths below are relative to `/api/v2`. Use an active administrator's session,
configured `Origin`, and session-bound `X-CSRF-Token` as described in
[authentication.md](authentication.md). Responses are private and `no-store`.
Ordinary users receive `403 ADMIN_REQUIRED`; missing, disabled, revoked, or expired
sessions receive `401 UNAUTHENTICATED`. Authorization is rechecked in transactions.

## Declare storage outside the API

The operator declares an allowlist in the private YAML configuration:

```yaml
sources:
  - name: family
    root: /mnt/nas/family
    marker: .gallerygo-source
```

Names are unique labels of 1..64 UTF-8 bytes without control characters. They are
not filesystem paths. Relative roots resolve against the configuration file, and
roots must not overlap database, cache, web storage, or each other. Markers are
normalized root-relative paths without traversal or backslashes.

The mounted source must already contain a readable regular marker file. GalleryGo
never creates roots, markers, or source files. Use trusted read-only NAS/container
mounts and appropriate filesystem timeouts; a Go context cannot guarantee that a
blocked kernel NAS syscall immediately stops. See [deployment](../deploy/readme.md).
The API cannot add a new arbitrary filesystem path to the allowlist.

`GET /admin/source-roots` lists only `{root_name}` labels in sorted order. It never
returns physical paths, marker filenames, or private configuration digests. Lists
use default limit 30, maximum 100, and opaque account/scope/collection-bound cursors.

## Register a source

`POST /admin/sources` requires a new `Idempotency-Key` and accepts:

| Field                        | Default                       | Meaning                                                                         |
| ---------------------------- | ----------------------------- | ------------------------------------------------------------------------------- |
| `library_id`                 | Required                      | Existing destination library ID. Its grants remain mandatory.                   |
| `root_name`                  | Required                      | Exact YAML allowlist label, not an absolute or relative path.                   |
| `mapping`                    | Album depth 1; person depth 0 | Optional full mapping object described below.                                   |
| `active`                     | `true`                        | Whether this source's albums and content may be visible.                        |
| `minimum_files`              | `0`                           | Completed whole-source scan safety guard, 0..1,000,000,000.                     |
| `poll_interval_seconds`      | `60`                          | Dispatch interval, 10..86,400 seconds; full until healthy full evidence exists. |
| `full_scan_interval_seconds` | `86400`                       | Periodic full policy, at least the poll interval and at most 604,800 seconds.   |

For example:

```json
{
  "library_id": "<library-id>",
  "root_name": "family",
  "mapping": { "album_depth": 2, "person_depth": 1 },
  "minimum_files": 100
}
```

The response is `201` with source metadata, `Location`, and a strong `ETag` of the
form `"source:<id>:<version>"`. A root label or canonical path already registered
to a source returns `409 SOURCE_ROOT_IN_USE`. An undeclared root or invalid policy
returns `422 VALIDATION_FAILED`. No default library grant is created.

Root paths, private path/marker pins, worker capabilities, and arbitrary source
credentials are not input fields. Unknown names, explicit nulls, duplicate keys,
case-folded nested names, and oversized/invalid JSON are rejected rather than
silently interpreted as a different mapping.

### Folder mapping

Depth counts **directory segments from the declared root**, not the file itself.
Both values range from 0..16, and `person_depth` must not exceed `album_depth`.

- `album_depth: 2` maps `Alice/Trip/deeper/photo.png` to album path `Alice/Trip`,
  source title `Trip`.
- `person_depth: 1` maps the optional person label to `Alice`.
- `album_depth: 3, person_depth: 2` supports layouts such as
  `Region/Alice/Trip/photo.png`; there is no hardcoded two-level NAS layout.
- `album_depth: 0, person_depth: 0` groups the source root as one album without a
  person. Files shallower than the configured depth use their actual parent;
  root-level files remain in the root album instead of disappearing.

The `mapping` object is a **full replacement**. Missing member depths in an
explicit mapping object are zero; `{}` explicitly groups the whole source. Omit
`mapping` entirely on creation to use album depth 1/person depth 0. Display-name
and other indexed overrides are not rewritten by a configuration edit.

## Inspect or change

- `GET /admin/sources` returns a bounded stable-ID page.
- `GET /admin/sources/{source_id}` returns metadata and its current `ETag`.
- `PATCH /admin/sources/{source_id}` accepts any nonempty subset of the creation
  fields and requires the exact current `If-Match` plus Origin/CSRF.
- `DELETE /admin/sources/{source_id}` requires `If-Match` and only removes an
  **empty registration**. Indexed albums or entries return `409 SOURCE_NOT_EMPTY`.
  Deactivate an indexed source instead; this endpoint never purges its index or
  original files. Job and audit history remain.

Missing `If-Match` returns `428 PRECONDITION_REQUIRED`; malformed, weak, wildcard,
multiple, or cross-resource tags return `400 INVALID_PRECONDITION`. A stale version
returns `409 VERSION_CONFLICT`. Reload current metadata before deciding on a new
change. A no-op preserves versions, scopes, and pending jobs.

Real configuration changes increment both `version` and `config_version`, rotate
old/destination library authorization scopes without reviving/revoking valid login
sessions, cancel stale source work, and reset reachability to `unknown`. Existing
indexed rows, favorites, ratings, and display overrides remain untouched. A source
moved to a different library hides mismatched old albums from **every account**
until a healthy inventory publication reassigns them. Deactivating a source hides
its metadata and bytes even from administrators.

`configured` means that this process's YAML path/marker registry matches the
source's private pin. It is independent of network/mount reachability. After
changing YAML roots/markers, restart relevant processes with consistent config and
PATCH the same `root_name` with its current ETag to explicitly adopt a changed pin.
Old processes cannot publish or serve originals under a mismatched pin. Original
entries also require the current configuration epoch; a root probe alone cannot
make an older inventory authoritative. Authorized cached previews can survive an
outage while the source remains active and its library relationships consistent.

## Check reachability, not inventory

`POST /admin/sources/{source_id}/check` requires current `If-Match`, a new
`Idempotency-Key`, Origin, and CSRF. Send **no body**, not even `{}`.

A completed inspection returns `200` and source metadata:

- `state: healthy` means the declared root and regular mount marker are readable.
- `state: unavailable` includes a safe `last_error_code`, such as
  `SOURCE_UNAVAILABLE` or `SOURCE_NOT_CONFIGURED`.
- `last_checked_at` records inspection; `last_healthy_at` retains the latest good
  reachability result. `config_version` does not change for an inspection.
- `scan_generation: 0` and `last_full_scan_at: null` remain unchanged until actual
  inventory publication. **Healthy reachability is not a completed inventory,
  minimum-file check, successful ingestion, or authorization to reconcile/purge.**

Filesystem access occurs outside writer transactions. Before publishing the
result, the service rechecks the administrator/session/resource version. Missing
roots or markers are never created; a failed probe never deletes or reconciles
indexed entries, interactions, or metadata.

Create/check receipts last 24 hours and are actor-scoped. UUID keys are recommended
(16..128 ASCII letters, digits, underscores, or hyphens). Retry a lost response with
the same operation, body, key, and **original** precondition. The original result is
returned without repeating work, even if it is now historical; GET refreshes the
latest state. Reusing a key for a different command returns
`409 IDEMPOTENCY_CONFLICT`. Source/job changes, scope revokes, event/outbox, audit,
and successful POST receipts commit atomically. See [job controls](jobs.md).

Inventory failures can also record `state: error` with a safe `last_error_code`:
`SOURCE_CHANGED`, `IO_ERROR`, `MINIMUM_FILES`, `SCAN_LIMIT`, `UNSAFE_ENTRY`, or
`SYMLINK_CYCLE`. A missing/unconfigured root uses `state: unavailable`. Failure
records retain the last healthy/full-scan timestamps, published generation, indexed
rows, and interactions. Failure state, generation issue, job progress, and audit
share one fenced transaction; no raw source path or system error enters these DTOs.

## Inventory processing boundaries

The backend can stage a complete, bounded, root-confined filesystem manifest and
native JPEG/PNG/GIF header metadata in private database tables. It revalidates
streamed hashes instead of trusting equal sizes/timestamps, pins the inspector
policy for retries, and records unsupported/corrupt inputs explicitly. Non-media
paths stay in the manifest and count as skipped metadata work; invalid or unsupported
media count as failed work, not successful decoding. Metadata, issues, generation
counters, and job progress/events commit together in bounded batches.

The scan handler connects these services to the durable embedded/standalone worker,
using only versioned payloads and immutable selected targets. Staging alone publishes
no indexed resources or READY variants. The indexed publisher first
re-walks and rehashes the complete manifest, then rechecks metadata completion,
source configuration, exact live lease/attempt, cancellation, and generation version
inside one writer transaction. Source entries, logical media, owned revisions,
albums/people, events/outbox, source generation, and job progress commit atomically;
the final source also commits terminal job completion. Intermediate source parts
keep a multi-source job running with an unknown total, never report all-source
success after one source. Filesystem and codec work never run inside that transaction.

Matching gives an existing exact path priority. A missing old path and unmatched
new path can retain an entry/media identity only when their full SHA-256 and size
candidates are uniquely paired in a frozen pre-mutation plan. Duplicates and mixed
folder moves are diagnosed, never guessed. A provable whole-folder transition can
retain album/person IDs, display overrides, restrictions, tags, favorites, ratings,
and cover preferences. Equal person labels at different source-relative paths are
not merged. Same-path changed source signatures create a new owned immutable
revision; older variants do not become current merely because their rows remain.

A missing path becomes unavailable without deleting its entry, revisions, variants,
or interactions. Quarantine requires at least two healthy full-source publications
and 24 hours of absence; failed scans and target/incremental scans do not manufacture
full-scan evidence. Reappearing files retain their logical identities. There is no
automatic purge. Target jobs validate the whole source but reconcile only their
selected scope; a cross-scope rename is deferred with a diagnostic until a full
publication can resolve it safely. Ownership/library changes revoke historical and
destination authorization projections without revoking valid login sessions.

The [scan enqueue API](jobs.md) accepts these private durable intents; `serve` consumes
them by default, or use `worker` with consistent configuration. Consumers also
schedule active process-configured sources by default; initial/overdue full work
and incremental polls share the same immutable source-intent keys as manual jobs.
Disable `scheduler.enabled` in every consumer for manual-only operation. Private
dispatch deadlines do not imply a healthy scan or advance full-publication evidence.
[Readiness](health.md) reports degraded for unknown/unavailable/stale/mismatched
source observations without probing NAS inside an HTTP request. It does not change
source health or grant inventory authority. Queued acceptance alone cannot prove
successful ingestion. Header inspection does not prove complete pixel/frame decoding. External
codecs, EXIF, animation timing, variant generation, and indexed SIGKILL/reopened-DB
recovery acceptance remain pending. These capabilities do not complete the
source-processing phase or authorize index purge.
