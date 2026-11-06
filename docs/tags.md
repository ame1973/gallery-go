# Tags and album associations

The tag catalog is an online administrator-only metadata surface. It can create,
inspect, rename and remove **unassigned** tags without touching source files,
album associations, access policy or account scopes. Separate versioned album
association commands below assign existing IDs, and authorized readers can inspect
only the labels associated with visible albums. Batch media management, rich search/
filter options, offline sync and management UI remain separate required work.

All catalog routes below are relative to `/api/v2` and require a current administrator HTTPS
session. Ordinary users receive 403, including for unused/private labels and unknown
IDs; anonymous/expired/disabled/revoked sessions receive 401 before existence is
revealed. A tag ID or name is not a library/media/original/offline grant. Responses
are `private, no-store` and never contain source roots, paths, markers or runtime pins.

## Exact labels and identities

A tag has only:

```json
{
  "id": "opaque-tag-id",
  "name": "Travel",
  "version": 1
}
```

Labels use the existing display-label policy: trim whitespace, then 1 to 100 Unicode
characters and at most 400 UTF-8 bytes without control characters. Missing, null,
blank, oversized or unknown/case-folded request fields are rejected, not ignored.
All name-bearing requests accept only `{"name":"..."}`.

Exact binary labels are unique after trimming. `Travel`, `travel` and `Ｔravel` may
be different tags with different IDs. Renaming preserves the opaque ID. This
identity rule is **not NFKC/full case-fold search normalization**; the required shared
online/offline search normalization and candidate-parity fixtures remain separate.
The catalog does not silently substitute lowercasing for that work.

## Read and paginate

- `GET /admin/tags` returns `{items,next_cursor,has_more}` in stable tag-ID order.
  Default limit is 30, maximum 100. Only `limit` and `cursor` are supported; unknown
  search/sort/filter parameters are rejected. Empty results use `items:[]`.
- `GET /admin/tags/{tag_id}` returns one current tag and a strong resource ETag such
  as `"tag:{id}:7"`. Unknown/deleted IDs return 404.

Cursors bind the current account, authorization scope and collection; they cannot
be reused for group pages or after scope rotation. These are live keysets, not a
fixed offline snapshot. Reads do not probe source filesystems; an unavailable mount
does not block catalog metadata.

## Versioned commands and retries

Every mutation requires the normal same-origin `Origin` and session-bound
`X-CSRF-Token`, plus an explicit unique `Idempotency-Key` of 16 to 128 ASCII
letters/digits/underscores/hyphens:

- `POST /admin/tags` accepts `{"name":"Travel"}` and returns 201, a relative
  `Location`, ETag and tag metadata. An existing exact label returns
  `409 TAG_NAME_TAKEN`; a historical same-key retry returns its original result.
- `PATCH /admin/tags/{tag_id}` accepts the same bounded name object and requires
  the exact resource `If-Match`. Real changes increment the tag version. An identical
  preference preserves its version/business events but still saves a receipt.
  Existing album associations and policies are unchanged, including on assigned tags.
- `DELETE /admin/tags/{tag_id}` requires the exact `If-Match` and **no request body**.
  It returns 204 without a body only when there are no album associations. Any edge,
  including an inactive/hidden album, returns `409 TAG_IN_USE`. The tag and all links
  remain intact. Use explicit album-tag replacement below to change only links;
  the catalog never invokes an implicit cascade, source cleanup or original deletion.

Missing strong preconditions return 428; invalid/wrong-resource/weak/list/wildcard
preconditions return 400. New commands with stale versions return 409. Label/version
or deletion, complete change/tombstone, outbox, audit and actor receipt co-commit.
A failure in any late journal/receipt write rolls back the entire operation.

Retry the **same actor/key/canonical body/target/original precondition** for its
historical response and ETag. Creation can replay 201 after a later rename/deletion;
rename can replay its historical 200 after deletion; deletion safely replays 204
without requiring the row to reappear. These historical results do not mean the
resource/version still exists. GET refreshes current state. Reusing a key for another
operation/body/target/precondition conflicts. Every replay still rechecks a current
administrator and is never an authentication capability.

Migration `0015_tag_catalog.sql` preserves existing IDs/labels/associations, adds
positive tag versions and a reverse association index for the guarded deletion
lookup. Use the normal explicit migration/backup procedure from
[deployment documentation](../readme.md); workers never migrate implicitly. No
source/cache storage, decoder, variant publication or content-serving policy is
changed by this metadata catalog.

## Authorized album tag reads

`GET /albums/{album_id}/tags` requires a current session, not administrator status.
It applies the same fresh-transaction library/source/additional-album visibility as
other album reads. Hidden, inactive, inconsistent or unknown albums return 404;
a visible album with no tags returns `items:[]`. Only associated catalog labels are
returned, never unrelated/unused/private global names. A tag is not an access group
and adding it cannot grant access to an album or its original media.

Pages reuse `{items,next_cursor,has_more}`, default 30/max 100, stable tag-ID order
and account/scope/album-collection-bound cursors. Only `limit`/`cursor` are accepted.
These are live `Tag` rows with each tag's own version, not an offline snapshot.
A catalog rename can change this response without changing the album version, so
label-bearing pages deliberately have **no album-only ETag**. They remain
`private, no-store` and do no source filesystem I/O.

## Versioned album association replacement

`GET /admin/albums/{album_id}/tags` requires a current administrator and shared
album visibility. It returns the strong **album** ETag and only:

```json
{
  "album_id": "opaque-album-id",
  "tag_ids": ["opaque-tag-id"],
  "version": 7
}
```

This IDs-only body can safely use the album version across independent tag renames.
Album display/cover edits, policy changes or scans can invalidate that shared
precondition; refresh before a new command. Source mount loss alone does not block
indexed associations, but inactive/inconsistent source/library ownership fails closed.

`PUT /admin/albums/{album_id}/tags` requires the same Origin/CSRF/Idempotency-Key
rules as the catalog plus the exact album `If-Match`. The only body field is:

```json
{
  "tag_ids": ["opaque-existing-tag-id"]
}
```

Provide an explicit array of at most 100 unique existing IDs, each at most 64 UTF-8
bytes. The set is canonicalized in stable ID order; order-equivalent retries are the
same command. Unknown/duplicate/invalid IDs fail without partial writes. Missing or
null is not a clear. `{"tag_ids":[]}` clears **only this album's links**, never catalog
tags, source files, covers, creator associations or access restrictions. Tag edits
do not rotate account scopes or grant download/offline capability. Policy changes
remain separately authorized and can rotate scopes normally.

Real changes atomically commit links, the shared album version, a complete album
upsert including `tag_ids`, outbox, audit and actor receipt. No-op preserves the
version/business events but saves its receipt. The same canonical actor/key/target/
body/original precondition replays its historical IDs/version/ETag after later
clears, metadata edits or scans; current administrator authorization still applies.
A historical result is not current state. GET refreshes current IDs and version.

All current album-upsert producers share the complete canonical projection, so
metadata edits and healthy native scans/provable folder moves cannot omit retained
tag IDs. Separate access-policy projections and group deletion preserve links and
never make tags an authorization bypass. No new migration is needed beyond the
existing tag catalog schema; batch media-manager operations, management UI and
shared-normalized search/FTS/offline synchronization remain required work.
