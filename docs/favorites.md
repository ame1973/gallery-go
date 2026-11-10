# Personal album favorites

All paths below use `/api/v2`. These authenticated metadata endpoints manage only
the calling account's favorite membership and collection assignment. `GET /albums`
can filter the calling account's favorites across collections, within one direct
collection or in default/unfiled. Personal collections can be created, listed,
renamed and explicitly deleted with a two-step choice. Recursive subtree browsing,
hierarchy UI and offline operation replay remain separate required features.

## Read your state

`GET /favorites/{album_id}` returns an opaque strong `ETag` and:

```json
{
  "album_id": "album-id",
  "favorited": false,
  "collection_id": null,
  "updated_at": null,
  "version": 1
}
```

This is the initial state for an album this account has never favorited. After a
real change, `updated_at` is a server-generated UTC-millisecond instant and the
version increases. A cleared favorite also has `favorited:false`, but retains its
later version/time; clearing and re-adding never reuse an earlier revision.

Administrators use their own personal resource too, not another account's state.
No query parameters, account selectors, client time or public list of favoriting
accounts are accepted. Every request rechecks the actual session/account and shared
album visibility in the same transaction. Missing, hidden, inactive-library/source
or inconsistent albums return the same `404`; an expired/revoked/inactive account
receives `401`. Responses use `Cache-Control: private, no-store`.

A missing source mount or marker does not prevent authorized metadata changes.
The endpoint never probes or writes source files, grants original downloads or
offline access, changes group policy, or advances a shared album/rating version.
Another account's favorite and shared gallery edits do not invalidate your personal
favorite validator. Private changes do not appear as fabricated public counts.

## Create and rename collections

`POST /favorite-collections` accepts `{"name":"Trips"}` for a root, or
`{"name":"Summer","parent_id":"owned-parent-id"}` for a child. A null or omitted
`parent_id` means root; it does **not** create or represent the default/unfiled
favorite assignment. Send an `Idempotency-Key`, the configured HTTPS `Origin`, and
current CSRF token. A successful `201` includes a `Location`, strong `ETag`, and
`{id,parent_id,name,created_at,updated_at,version}`. It does not include album IDs
or counts. Missing and foreign parents both return `404`.

`GET /favorite-collections` lists all owned folders in album-independent ID order;
`?root=true` lists roots, and `?parent_id=<owned-id>` lists direct children. Use
only one selector, with optional `limit` and `cursor`; default limit is 30, maximum 100. Root accepts exactly `true`, and the parent ID must be nonempty and literal.
Other users' folders remain invisible, including to administrators. Cursors bind
account, scope, path and filters; these are live pages, not offline snapshots.
`GET /favorite-collections/{id}` retrieves one owned folder and its personal ETag.

To rename, send `PATCH /favorite-collections/{id}` with `{"name":"New name"}`,
the folder's exact `ETag` in `If-Match`, a new `Idempotency-Key`, `Origin`, and CSRF
token. Parent, children and assigned favorites do not change. Names are trimmed,
1..100 Unicode characters and at most 400 UTF-8 bytes, without control characters.
Exact SQLite binary names are unique **within the same parent**; case and Unicode
composition variants remain distinct. This is not NFKC/full-fold search. Duplicate
names return `409 COLLECTION_NAME_TAKEN`; new stale versions return
`409 VERSION_CONFLICT`. An identical name is a no-op except for its receipt.

A matching create retry returns its historical owned result without recreating a
subsequently removed folder or parent. Rename retries require current ownership and
return their historical result without restoring an obsolete name. Use GET to
refresh state. Real creation/rename co-commits private folder change/outbox, audit
and actor receipt; no album, favorite, grant or scope is changed. Reparenting via API
and recursive subtree album listing are not yet implemented.

## Confirm and delete a collection subtree

Deletion removes the selected folder **and all its descendant folders**. Choose one
explicit action for all their assigned favorites:

- `move_to_default` keeps each favorite and moves it to default/unfiled.
- `remove_favorites` clears each favorite while retaining its personal revision.

The choice also applies to this account's previously assigned favorites whose albums
are now hidden by changed grants. Preview responses do not disclose those album IDs
or hidden-item counts. Explain this warning before the user confirms in a UI.

1. Read `GET /favorite-collections/{id}` and retain its exact root `ETag`.
2. Read `GET /favorite-collections/{id}/deletion-preview?action=move_to_default`
   or use `action=remove_favorites`. The private response includes:

   ```json
   {
     "action": "move_to_default",
     "confirmation_token": "opaque-64-lowercase-hex-characters",
     "includes_descendants": true,
     "hidden_items_may_be_affected": true
   }
   ```

   The illustrated token is a placeholder, not a valid value. Use the returned token
   unchanged; it is bound to the live session, account, scope, root, action, subtree
   and favorite revisions, and does not itself grant access. A client-known session
   cookie is not used as the secret key: a 32-byte installation-private key, generated
   with `crypto/rand`, protects the confirmation from offline guesses about hidden
   items. The first authorized preview initializes this key with a short database
   write; subsequent snapshot scans use read transactions. The key stays in the
   protected SQLite database, is never returned, and survives restart or independent
   server processes using the same database. Replacing or losing it invalidates old
   confirmations; obtain a new preview. Protect it along with database backups.

3. Send `DELETE /favorite-collections/{id}` with JSON containing that same `action`
   and `confirmation_token`, the root tag in `If-Match`, an actor-bound new
   `Idempotency-Key`, the configured HTTPS `Origin` and current CSRF token. Success
   returns an empty `204`. Missing and foreign collection IDs both return `404`.

If the scope, hierarchy, favorite assignments/revisions or selected action changes,
confirmation fails with `409 VERSION_CONFLICT`. Re-read the root and obtain a new
preview; do not silently reuse the old choice. Folder metadata version alone does not
freeze its contents. Concurrent deletion and assignment are serialized by the writer
transaction. A matching retry of the original request and key within 24 hours returns
its historical `204` even after the folder is gone, without repeating the operation;
a fresh active session and the same actor remain mandatory. A different request with
that key returns `409 IDEMPOTENCY_CONFLICT`.

Each affected favorite retains a monotonic personal revision and receives a complete
private upsert/delete event and outbox. Each removed folder gets a private tombstone;
one of these events grants access to hidden resources. One deletion audit records the
root, action and new root version without listing hidden items. Business changes,
events, outboxes, audit and receipt commit together; no shared album, other account,
authorization grant or source file is modified. This is an atomic SQLite operation,
not a resumable background job. A sufficiently large or busy subtree can exceed the
10-second metadata deadline; the transaction rolls back instead of reporting partial
success, and a retry is not guaranteed to finish. Historical private change/outbox
records remain stored after deletion and must be protected in databases and backups.

## Filter visible albums

`GET /albums?favorite=true` selects the current account's live favorites across all
owned collections, including default/unfiled. `favorite=false` selects visible
albums never favorited or subsequently cleared. Omit all favorite filters for all
visible albums; use exactly lowercase `true` or `false` once. No account ID is accepted.

`GET /albums?favorite_collection_id=<id>` selects only live favorites assigned
directly to that owned collection; it does **not** include child collections. A
missing or another account's collection ID returns the same empty authorized page.
`GET /albums?favorite_unfiled=true` selects live favorites assigned to the default,
not cleared or never-favorited albums. Use exactly `true` once. The two selectors
are mutually exclusive and cannot be combined with `favorite=false`; either may
be combined with `favorite=true`.

The filter can be combined with the documented library, source, creator and tag
selectors, all supported album sort orders, and bounded keyset pagination. Current
session and shared album visibility still apply before pagination; favorites never
grant access. Partial indexes skip cleared rows for favorite-only or direct-folder
ID-order pages; other orders use their existing indexes and an account/album point
lookup. Sparse filters may still traverse candidates in the requested order.

Cursors bind the account, scope, exact filters and sort. A continuation uses its
frozen position but reads current membership and access; adding, clearing or moving
items can change which records appear between live pages. These are not immutable
offline snapshots. Historical mutation receipts do not reactivate a cleared favorite.
Recursive subtree browsing and complete text/media search remain required.

## Favorite, move or clear

1. Read the state and retain its exact `ETag`.
2. Send `PUT /favorites/{album_id}` with `{}` or `{"collection_id":null}` to select
   default/unfiled, or `{"collection_id":"owned-collection-id"}` to select an
   existing collection owned by this account. Send the personal tag in `If-Match`,
   a new `Idempotency-Key`, the configured HTTPS `Origin` and current CSRF token.
3. Use `DELETE /favorites/{album_id}` with those headers and **no body** to clear
   membership and assignment. Both methods return `200`, the personal state and
   its validator; read again when current state is needed.

There is one membership per account/album. PUT replaces its collection rather
than duplicating a favorite. Null or an omitted `collection_id` means default;
an empty string is not a default alias. A nonnull ID is exact, at most 64 UTF-8
bytes, with no surrounding whitespace, slash/backslash, control or format
characters. Foreign and missing collections return the same `404 NOT_FOUND`.
The schema enforces same-account ownership as well as the service check.

An empty JSON PUT object is valid, but an absent/non-object JSON body, malformed
UTF-8, unknown/duplicate/case-folded fields, non-string collection values and
client-supplied identity/time/favorited fields are rejected. DELETE `{}` is not an
empty body. Missing `If-Match` returns `428`; malformed, weak, foreign-account,
foreign-resource, list or wildcard tags return `400`. A new request with a stale
personal revision returns `409 VERSION_CONFLICT`. Do not parse/synthesize a tag or
substitute the shared album's or rating's validator.

A real membership/assignment change updates the personal row/version/time and
co-commits a complete private `favorite` upsert/delete event in the actor's current
scope, its outbox, audit and command receipt. An identical state or repeated clear
keeps its version, timestamp and events; only a successful receipt is stored.
Clearing an unmaterialized initial state does not create an unnecessary row.
Private update times never move backward if the server clock regresses.

## Retry without restoring an obsolete favorite

Reuse the original method, equivalent body, key and original `If-Match` within
24 hours. Omitted/null collection values have the same default semantics. Keys
contain 16..128 ASCII letters, digits, underscores or hyphens; a random UUID is
suitable. Keys are scoped to the actor and shared with other receipted commands;
a different operation/resource/precondition/state with the same key returns
`409 IDEMPOTENCY_CONFLICT`.

A matching retry returns its **historical personal result**, without restoring a
subsequently cleared/reassigned favorite or editing a removed collection. A
historical result can refer to a collection that no longer exists; it is not a
current assignment or permission. GET refreshes current state. Fresh session and
album visibility are required even before historical replay, so revocation cannot
be bypassed with an old receipt. After expiry, inspect the current version before
attempting another write. Retained revisions survive receipt expiry.

## Persistence and deletion

Migration0026 preserves valid current memberships, assignments and original row
creation times, initializes their private revision to1 and update time to the
stored creation time, and adds retained false states. It refuses invalid
cross-account assignments atomically rather than silently moving/deleting them.
The required composite parent key and indexed child relation enforce ownership;
foreign-key checking stays enabled throughout migration and normal transactions.

Migration0028 prepares the existing collection hierarchy for later management. It
preserves collection IDs, owners, parents, names, creation times, and favorite rows;
adds a positive version and an update time initially equal to creation time; and
indexes owner/parent/ID pages. Every parent must belong to the same account and
all collections must be reachable from an owned root. Existing cross-account links,
cycles, null/empty/over-64-byte/NUL collection IDs, or incompatible stored creation
times cause migration to fail atomically rather than deleting or moving data. Back
up the database before upgrading; if validation fails, restore a verified backup
or inspect the affected data under controlled administration instead of starting a
new database or silently repairing it.

Subsequent SQL writes cannot change a collection's ID or owner, introduce a foreign
parent or cycle, or use `OR REPLACE` to erase an existing identity/name. Nested
account deletion still uses the existing same-statement foreign-key cascades. The
supported API creates, lists, reads, renames and explicitly deletes folder subtrees;
it does not yet provide reparenting. Legacy stored IDs, names and times that
are outside the public DTO's bounds fail safely rather than being trimmed, replaced
or exposed as misleading data. Migration0029 adds only a partial live-favorite
index for direct-folder and default ID pages; it does not rewrite personal rows or
historical events. Migration0032 adds an empty constrained singleton for the
installation-private confirmation key; it does not rewrite existing collections,
favorites or event history. The first authorized deletion preview initializes the
key using the process's cryptographic random source.

A referenced collection cannot disappear through implicit `ON DELETE SET NULL`.
The explicit subtree deletion above updates or clears every affected personal
membership before removing folders. Account deletion removes its live/cleared favorites
and nested collections together, with existing account revoke/audit boundaries,
without changing another account's favorites or shared gallery rows. Use deactivation
instead when private metadata must remain recoverable. Scan no-ops, source outages
and provable folder moves retain private favorite identity/state, just as they retain
ratings and shared preferences.

This is not a favorite browsing screen, hierarchy UI, offline sync contract, download
grant or complete library acceptance. Those product features remain required before
release.
