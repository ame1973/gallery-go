# Tag catalog administration

The current tag catalog is an online administrator-only metadata surface. It can
create, inspect, rename and remove **unassigned** tags without touching source files,
album associations, access policy or account scopes. Album-tag assignment, batch
media management, search/filter options, offline sync and management UI are separate
required work, not implied by these endpoints.

All routes below are relative to `/api/v2` and require a current administrator HTTPS
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
  remain intact. Association editing is separate required work; this catalog cannot
  remove those edges or invoke an implicit cascade, source cleanup or original deletion.

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
