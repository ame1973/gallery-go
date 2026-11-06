# Album creator classification

Administrators can classify an indexed album using its current source mapping, a
concrete creator from the same source, or an explicit no-creator preference. These
are metadata-only commands: no source file/path is renamed, copied, written or
removed, and library/album grants and original-download policy are not widened.
Creator display labels remain a separate [Person metadata operation](people.md).

Routes are relative to `/api/v2` and require a current administrator HTTPS session
plus shared active/consistent album/library/source visibility. Ordinary users
receive 403 and anonymous/expired/disabled/revoked sessions receive 401 before
existence is revealed. Unknown/hidden/inactive/inconsistent albums return 404.
Mount loss alone does not prevent committed indexed classification metadata.

## Current IDs, indexed provenance and preference

`GET /admin/albums/{album_id}/creator` returns a strong **album** ETag, such as
`"album:{id}:7"`, and only:

```json
{
  "album_id": "opaque-album-id",
  "mode": "mapped",
  "person_id": "opaque-current-creator-id",
  "mapped_person_id": "opaque-indexed-creator-id",
  "override_person_id": null,
  "version": 7
}
```

- `person_id` is the current projected association, or null.
- `mapped_person_id` is the latest committed source-mapping fallback, or null. Scans
  keep updating this provenance even while an override or explicit none mode is active.
- `override_person_id` is the persisted choice for `person` mode, otherwise null.
- `version` is the shared album metadata version. Display/cover/tag/policy edits and
  source scans can invalidate a precondition; refresh before a new command.

These opaque IDs are not labels, filesystem access or creator/media permissions.
Public [Person lists, albums, counts and covers](people.md) still use current
same-source album visibility. Equal folder labels never merge different identities.
Responses are `private, no-store` and contain no relative/root paths, markers or pins.

## Three explicit modes

`PUT /admin/albums/{album_id}/creator` requires all of:

- the exact album `If-Match`;
- an explicit unique `Idempotency-Key` of 16 to 128 ASCII letters/digits/underscores/hyphens;
- the normal same-origin `Origin` and session-bound `X-CSRF-Token`.

The body is exactly one of:

```json
{ "mode": "mapped" }
```

Restore automatic classification from the **current indexed fallback**, not an old
folder label or original association. A null fallback remains unclassified.

```json
{ "mode": "person", "person_id": "opaque-same-source-creator-id" }
```

Select an existing source/path-bound creator from this album's current source.
The ID must be nonempty, at most 64 UTF-8 bytes, and refer to a concrete non-root
mapped identity. A same-source identity can be selected even if it currently has no
visible album; the command does not create or merge a Person. Unknown, foreign-source
or unbound selections return 422 without exposing their paths. A stale/unusable
historical preference never becomes foreign content authority.

```json
{ "mode": "none" }
```

Explicitly remove the current association while retaining indexed fallback
provenance. This differs from `mapped` mode and is not automatically undone by a
scan. `person_id` must be omitted in `mapped`/`none` modes. Missing mode, null,
unknown/case-folded fields or ambiguous combinations are rejected, not implicit clears.

## Versions, scopes and atomicity

Real changes co-commit the preference, current association, shared album version,
complete album event, outbox, audit and actor-bound receipt. A creator-graph change
rotates affected library-account and administrator scopes with old-scope revoke
events so stale creator aggregates/snapshots can be rebuilt. Valid sessions remain
valid, actual grants/restrictions/download policy do not change, and online reads
use the new classification immediately. Latching the already-current creator only
changes the preference/version, not an unchanged graph's scopes. A complete no-op
preserves business versions/events/scopes and saves only its receipt.

Retry the same actor/key/canonical target/body/original precondition for the
historical IDs/version/ETag, even after later choices or scans. A historical result
is not current state; GET refreshes it. Reusing the key for another mode/target/body/
precondition conflicts, and replay always requires a current administrator. New
stale commands return 409; missing strong preconditions return 428 and invalid ones 400. Late scope/change/outbox/audit/receipt failures roll back the entire operation.

## Scan and migration boundaries

Migration `0016_album_creators.sql` preserves existing current creator IDs and
interactions, backfills indexed provenance, adds explicit mode/preference constraints
and a mapped-creator lookup index. Use the normal explicit migration/backup procedure
in the [deployment readme](../readme.md); workers never migrate implicitly.

Healthy scans update indexed provenance without overwriting a selected creator or
explicit none preference. Frozen whole-folder rename inference uses the mapped
creator, **never the manual choice**, so moving an album cannot steal another
creator's ID/label/path. Restoring `mapped` after a move uses the updated indexed
identity and label. A source mapping change can make the fallback null; actual graph
changes revoke old scopes in the same fenced publication transaction. Missing roots
or markers still do not create directories or remove indexed records.

This is not full creator-management UI, batch media manager, shared-normalized
search/FTS, offline synchronization, rendering/READY publication or phase acceptance.
