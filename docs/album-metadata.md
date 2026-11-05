# Album display and cover preferences

Administrators can override an indexed album's display label and preferred cover
without renaming, copying, deleting or writing any source file. This current online
API is metadata-only: tags, creator/source/library reassignment, access policy,
batch reprocessing and management screens remain separate operations/work.

All routes below are relative to `/api/v2`, require a current administrator HTTPS
session, and share active library/source visibility. An unavailable mount does not
prevent indexed metadata edits; an inactive/inconsistent source or library is hidden.
Ordinary users receive 403 and anonymous/expired/disabled/revoked sessions receive 401
before resource existence is revealed. Source health and decoder/READY capability
are not inferred from a successful edit.

## Read preferences

`GET /admin/albums/{album_id}` returns an ETag such as `"album:{id}:7"` and:

```json
{
  "id": "opaque-album-id",
  "source_name": "Indexed folder label",
  "display_name": null,
  "cover_media_id": "opaque-current-media-id",
  "cover_override_id": null,
  "version": 7
}
```

- `source_name` is an indexed display label, never the full relative/root path.
  It is not editable through this endpoint.
- `display_name:null` uses the indexed source label in public album metadata.
- `cover_media_id` is the current visibility-checked album-owned projection, or null.
  A media ID does not establish a READY variant or original-download grant.
- `cover_override_id` is the desired preference, or null for automatic selection.
  It can retain a historical choice while that media temporarily moves away. It is
  not current ownership, permission, a URL or authorization to read another album.

## Versioned command

`PATCH /admin/albums/{album_id}` requires all of:

- the exact resource ETag in `If-Match`;
- an explicit unique `Idempotency-Key` of 16 to 128 ASCII letters/digits/underscores/hyphens;
- the normal same-origin `Origin` and session-bound `X-CSRF-Token`.

For example:

```json
{
  "display_name": "A display override",
  "cover_media_id": "opaque-current-album-media-id"
}
```

Both fields are optional but at least one is required. Omitted means unchanged.
An explicit empty string clears that override; **null is rejected**, not an
ambiguous no-op or implicit clear. Nonempty names use the existing display-label
policy: trim whitespace, then 1 to 100 Unicode characters/at most 400 UTF-8 bytes without
control characters. This is not multilingual search normalization. Source labels,
IDs/associations, permissions and arbitrary extra fields are rejected.

A nonempty cover must currently be same-album, shared-policy-visible, non-archived
eligible media with a consistent indexed source. Foreign/hidden/missing/non-media
or mismatched-source choices receive 422 without disclosing their location. No
source filesystem probe or decoder runs during selection. Clearing the desired
cover restores sticky automatic selection: prior valid current cover, then stable
media ID. A nonempty valid desired override takes precedence while it is set.
Therefore clearing a preference
can legitimately leave the same visible current cover. Empty albums remain null.

Successful changes atomically write the album metadata/version, complete album
change, outbox, audit and actor-bound receipt. No-op commands preserve the metadata
version and business events, but still save their receipt. A stale new command
receives 409; missing/invalid strong preconditions are rejected. The API never clears
the album's persistent access restriction or changes account scopes/download policy
for a mere display/cover edit.

## Retries, scans and privacy

Retry the **same actor/key/canonical body/original precondition** for a historical
result. A matching receipt returns the original response and ETag even after later
edits or scans; it does not mean that version is still current. GET refreshes current
metadata. Reusing a key for another body/target/precondition conflicts. Receipt
replay still requires a current administrator and never acts as an auth capability.

Healthy scans retain display/desired-cover overrides. A provable folder move can
update the indexed source label while retaining logical album/media identities and
manual preferences. Scan/source changes are still versioned; refresh before making
a new edit. A desired cover that temporarily leaves its album is retained as a
preference, but its foreign bytes are never exposed as a current cover.

Responses are `private, no-store` and contain no source paths, markers or runtime
pins. Original GET/HEAD/Range retains the additional `download_original` policy,
including for administrators. No public media mount, original fallback, automatic
delete, rendering, offline grant or full management-UI acceptance is added here.
