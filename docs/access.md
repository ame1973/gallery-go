# Groups, resource grants, and metadata API

Library and album access are deny-by-default and enforced in SQL, not by hiding
links in the frontend. The backend APIs described here are implemented; dedicated
library-browsing and permission-management screens and inventory scanning are not
yet available. Existing indexed metadata can be queried through the API.

## Authorization rules

Every request requires a valid server-side session and an active account. Resource
reads also require an active library and internally consistent source/library
relationships. Inactive libraries remain manageable through the administrative
API but are hidden from all content-facing reads, including administrators.

For a regular user, the rule is:

```text
allow = library_granted AND (not album_restricted OR matching_album_group)
```

A group membership can satisfy any matching library grant, and any membership can
satisfy an album restriction; the two matches need not come from the same group.
An album grant never replaces the mandatory library grant. A user without groups
cannot read a private library. A library without grants does not become public.

Administrators bypass resource group grants, but not inactive-library or source
consistency checks. Original download policy remains a separate requirement for
administrators as well as regular users. [Protected media streaming](media.md)
uses the same predicate for GET, HEAD, Range, and conditional requests; no public
media filesystem mount exists.

## Configure access safely

All paths below are relative to `/api/v2`. Sign in as an active administrator and
send the exact configured `Origin` and session-bound `X-CSRF-Token` on mutations.
See [authentication.md](authentication.md) and [administration.md](administration.md).
JSON is strict, UTF-8, and bounded to 16 KiB; null and unknown fields are rejected.

1. Create a library using `POST /admin/libraries` with `{ "name": "..." }` and an
   `Idempotency-Key`. It starts active unless `active:false` is supplied, but has
   **no grants**. Creating a library does not import any files.
2. Create groups using `POST /admin/groups` with `{ "name": "..." }` and a new
   idempotency key. Names are trimmed, 1..100 Unicode characters and at most 400
   UTF-8 bytes, without control characters. Group names are unique and case-sensitive.
3. Read `GET /admin/users/{user_id}/groups` and its user ETag. Replace memberships
   using `PUT` on the same URL with an explicit `{ "group_ids": [...] }` array and
   that tag in `If-Match`.
4. Read `GET /admin/libraries/{library_id}/access` and its library ETag. Replace the
   library grants using `PUT` on the same URL with `{ "group_ids": [...] }` and
   `If-Match`. An empty array denies every regular user.
5. Optionally read `GET /admin/albums/{album_id}/access` and use its album ETag to
   explicitly set additional restrictions with `PUT` on the same URL.

All group replacement sets are unique arrays of at most 100 existing group IDs.
Unknown groups, duplicate IDs, missing arrays, or null arrays are rejected. The
server sorts IDs consistently; sending the same set in a different order is a
no-op and does not rotate scope or increment the version.

### Album restriction states

| Request body                                   | Meaning                                                                  |
| ---------------------------------------------- | ------------------------------------------------------------------------ |
| `{ "restricted": false, "group_ids": [] }`     | Any user who already has the library grant may read the album.           |
| `{ "restricted": true, "group_ids": ["..."] }` | A library grant and at least one matching album group are both required. |
| `{ "restricted": true, "group_ids": [] }`      | No regular user may read the album; it remains explicitly restricted.    |

Both fields are mandatory. `restricted:false` with nonempty groups is rejected
instead of silently discarding contradictory policy data.

Deleting the last group on an album **does not** make it unrestricted. A durable
restriction flag stays set even when the relation table becomes empty. Only an
explicit authorized `restricted:false` policy write can clear that flag. Migration
backfills the flag for existing restricted albums, and new group edges latch it.

## Versions, retries, and revocation

Read the current resource ETag and send it unchanged as `If-Match` for every PATCH,
DELETE, and group/policy PUT:

- Groups use `"group:<id>:<version>"`.
- Memberships share the account's `"user:<id>:<version>"` tag.
- Library settings and grants share `"library:<id>:<version>"`.
- Album metadata and restrictions share `"album:<id>:<version>"`.

Missing preconditions return 428; malformed/foreign/weak/list/wildcard tags return
400; stale versions return `409 VERSION_CONFLICT`. A lost PUT response should be
reconciled by reading the current set and version, not by blindly overwriting it.

Group and library creation use the same actor-scoped, 24-hour idempotency ledger
as administrative account creation/reset. Reusing a key across different commands
returns `409 IDEMPOTENCY_CONFLICT`. Equivalent normalized names and omitted defaults
are treated as the same creation request. Use a new key for each intended command.

Membership and resource-policy changes rotate the authorization scope and advance
the affected account versions. They leave old-scope revoke events with durable
outbox rows, atomically with the policy and audit. Account scopes are opaque strings;
do not parse their encoding or length. Valid login sessions remain usable, but the
next resource request uses current grants. Old account/scope-bound pagination
cursors return `400 INVALID_CURSOR` after an epoch change.

This does not claim immediate deletion of already downloaded offline bytes.
Offline downloads are not enabled in this build; eventual offline access remains
bounded by its grant expiry, not by a promise of instant remote wipe.

## Group and library management

| Endpoint                               | Behavior                                                                     |
| -------------------------------------- | ---------------------------------------------------------------------------- |
| `GET /admin/groups`                    | Paginated groups in stable ID order.                                         |
| `GET /admin/groups/{group_id}`         | Group metadata and current ETag.                                             |
| `PATCH /admin/groups/{group_id}`       | Rename with `{ "name": "..." }`; grant edges do not change.                  |
| `DELETE /admin/groups/{group_id}`      | Remove memberships/grants transactionally; preserve album restriction state. |
| `GET /admin/libraries`                 | Paginated management list, including inactive libraries.                     |
| `GET /admin/libraries/{library_id}`    | Management metadata and current ETag.                                        |
| `PATCH /admin/libraries/{library_id}`  | Change `name` and/or `active`; active-state changes revoke affected scopes.  |
| `DELETE /admin/libraries/{library_id}` | Delete an empty library only; never delete source files.                     |

A library with sources or albums returns `409 LIBRARY_NOT_EMPTY` on deletion.
Deactivate it instead. Source reconciliation, quarantine, explicit metadata purge,
and source configuration are separate features; this API never erases indexed
content or original files just to satisfy a foreign key.

Group deletion advances versions of linked library/album policies, so a stale edit
cannot restore a removed group unnoticed. Audit events identify the actor, resource,
old/new grant sets for policy writes, and affected-account counts without passwords,
CSRF tokens, session capabilities, or source filesystem paths.

## Authorized metadata reads

- `GET /libraries`, `GET /libraries/{library_id}` return active, visible libraries.
- `GET /albums`, `GET /albums/{album_id}` return visible albums with display titles,
  library IDs, authorized media counts/covers, rating averages, and resource versions.
- Missing or unauthorized IDs return the same 404. Anonymous, disabled, expired,
  and revoked sessions receive 401. Administration requires the administrator role.

Lists use the standard `{items,next_cursor,has_more}` envelope, default limit 30,
maximum 100, and stable ID keyset order. Visibility is applied before LIMIT, and
counts/covers use the same media predicate. Invalid cross-source relationships or
a cover pointing to another album cannot leak an otherwise hidden media ID or count.
No absolute source or cache path is included.

These are live metadata pages, not immutable offline snapshots. Search, inventory,
media processing, the browsing UI, and offline synchronization are still being built.
All responses are private and `no-store`; no service worker globally caches the API.
