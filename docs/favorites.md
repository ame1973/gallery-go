# Personal album favorites

All paths below use `/api/v2`. These authenticated metadata endpoints manage only
the calling account's favorite membership and collection assignment. `GET /albums`
can filter the calling account's favorites across collections. Collection management,
collection-specific browsing, hierarchy UI and offline operation replay remain
separate required features.

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

## Filter visible albums

`GET /albums?favorite=true` selects the current account's live favorites across all
owned collections, including default/unfiled. `favorite=false` selects visible
albums never favorited or subsequently cleared. Omit the parameter for all visible
albums; use exactly lowercase `true` or `false` once. No account ID is accepted.

The filter can be combined with the documented library, source, creator and tag
selectors, all supported album sort orders, and bounded keyset pagination. Current
session and shared album visibility still apply before pagination; favorites never
grant access. A partial index skips cleared rows for favorite-only ID-order pages;
other orders use their existing indexes and an account/album point lookup. Sparse
filters may still traverse candidates in the requested order.

Cursors bind the account, scope, exact filters and sort. A continuation uses its
frozen position but reads current membership and access; adding, clearing or moving
items can change which records appear between live pages. These are not immutable
offline snapshots. Historical mutation receipts do not reactivate a cleared favorite.
For collection-specific browsing and complete text/media search, the remaining
collection and search APIs are still required.

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
account deletion still uses the existing same-statement foreign-key cascades. This
schema preparation does not add collection-management or collection-deletion APIs.

A referenced collection cannot disappear through implicit `ON DELETE SET NULL`.
Collection-deletion management must explicitly choose moving favorites to default
or removing them; that management API is not implemented here. Account deletion
removes its live/cleared favorites and nested collections together, with existing
account revoke/audit boundaries, without changing another account's favorites or
shared gallery rows. Use deactivation instead when private metadata must remain
recoverable. Scan no-ops, source outages and provable folder moves retain private
favorite identity/state, just as they retain ratings and shared preferences.

This is not a favorite browsing screen, collection creator, offline sync contract,
download grant or complete library acceptance. Those product features remain
required before release.
