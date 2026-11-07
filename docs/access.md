# Groups, resource grants, and metadata API

Library and album access are deny-by-default and enforced in SQL, not by hiding
links in the frontend. The backend APIs described here are implemented; dedicated
library-browsing and permission-management screens are not yet available. Existing
indexed metadata can be queried through the API; inventory scanning is documented
in [sources.md](sources.md).

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
- Album metadata and restrictions share `"album:<id>:<version>"`. Real personal
  rating changes and removal of an account's live votes also advance this shared
  version and journal the current average. Personal rating writes use a separate
  account-and-album-bound ETag; see [rating rules](ratings.md).

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
  library IDs, authorized media counts/covers, rating averages, first-indexed times,
  and resource versions.
- Missing or unauthorized single-resource IDs return the same 404. Anonymous,
  disabled, expired, and revoked sessions receive 401. Administration requires the
  administrator role.

### Album collection selection

`GET /albums` accepts these optional **single exact IDs**, combined by AND:

| Parameter    | Selection                                                               |
| ------------ | ----------------------------------------------------------------------- |
| `library_id` | The album's current active, visible library.                            |
| `source_id`  | Its current consistent active indexed source, not a root name/path.     |
| `person_id`  | Its effective creator, with a concrete indexed path in the same source. |
| `tag_id`     | An existing album-tag edge, not a label search or global catalog read.  |

For example, `GET /api/v2/albums?source_id=<id>&person_id=<id>&tag_id=<id>&limit=30`
returns the intersection in ascending album-ID order. Omitted selectors leave the
visible corpus unchanged. Each supplied value must be nonempty, at most 64 UTF-8
bytes, without leading/trailing whitespace, path separators, control or format
characters. IDs are literal: no trimming, case folding, label resolution or Unicode
normalization. Repeated, empty and unknown parameters return `400 INVALID_FILTER`;
unsupported `q` and `sort` values are not silently ignored. Use the documented
`limit` and `cursor` pagination parameters.

Unknown, unauthorized, inactive and contradictory selectors return `200` with
`{"items":[],"next_cursor":null,"has_more":false}`. This does not disclose whether
any global private resource exists. In contrast, single-resource routes and
`GET /people/{person_id}/albums` retain their missing/hidden-parent 404 behavior.
Person selection uses the effective creator ID, not the mapped fallback or display
label, and excludes cross-source or unclassified/root-path associations. Equal
creator labels do not merge identities. A tag selector never exposes unused/private
tag names or makes the administrative tag catalog available to ordinary users.

Every card retains the same title, version, authorized media count, cover and rating
average as album detail. Filters select albums, not a different media-count subset;
empty albums are retained and an invalid cover is null. Selection performs no
source filesystem probes or metadata/permission writes. Source reachability alone
does not hide otherwise authorized indexed metadata; inactive or inconsistent
sources still fail closed.

### Album chronological ordering

The common album DTO includes `indexed_at`: the immutable first-indexed **server**
instant in RFC3339 UTC, with millisecond precision (years 0001..9999). This is when
healthy inventory first published that logical album, not a camera capture time,
NAS folder/file mtime, or a recent display/tag/creator/cover edit. Proven folder
moves and no-op scans preserve it; list/detail/PersonAlbums use the same value.
The complete canonical album journal also retains it. Existing history is not
rewritten by the index-only migration.

`GET /albums` accepts optional `sort=latest` or `sort=oldest` together with any
of the resource selectors:

| Sort     | Primary order           | Tie-break order     |
| -------- | ----------------------- | ------------------- |
| omitted  | Album ID ascending      | ID is unique        |
| `latest` | `indexed_at` descending | Album ID descending |
| `oldest` | `indexed_at` ascending  | Album ID ascending  |

Chronological cursors freeze the last returned public UTC-millisecond key and
album ID. A continuation does not look up that anchor again, even if it was
removed or changed; both tuple components reverse together. First-index time is
always known for valid stored albums, so there is no synthetic unknown-time key
or capture/mtime fallback. ID-only cursors reject an unrelated time position, and
chronological cursors reject missing, noncanonical or out-of-range keys with
`400 INVALID_CURSOR`. Treat cursors as opaque rather than constructing their JSON.
Popularity-based album ordering and full text search are still required but not
implemented; unsupported values fail explicitly, never become fake ranks.

### Album literal name ordering

`sort=name_asc` and `sort=name_desc` compare the **exact public title**, followed by
album ID in the same direction. The title is the nonempty display override, or the
indexed source name when no override is set. SQLite `BINARY` compares its UTF-8 bytes:
uppercase/lowercase, composed/decomposed Unicode and leading/trailing source-name
spacing stay distinct; `10` precedes `2`. This is not a language-aware, natural
numeric, case-insensitive, NFKC or full-fold comparison. It does not implement or
substitute for the separately required normalized online/offline search behavior.

Public titles are valid UTF-8 and bounded to 4096 bytes, including empty stored
source-title values. Display overrides retain their existing smaller 100-character/
400-byte write bound. The larger public bound preserves source mapping's path-based
limit instead of silently assuming a 255-byte filesystem component on every host.
Names are never cropped or hashed into a different lexical order.

Name cursors freeze the **entire public title** and ID. An internal canonical
encoding distinguishes empty titles from missing positions and prevents JSON
control/HTML escaping from making a valid maximal key unfollowable. Only `GET /albums`
uses the dedicated `AlbumCursor` limit of 8192 characters; other list endpoints keep
512-character cursors and 32-byte sort positions. Invalid, noncanonical or overlong
keys fail with `400 INVALID_CURSOR`. No response emits a continuation beyond its
endpoint's bound. Treat all cursors as opaque, not URLs or download capabilities.
Reverse proxies must allow at least 16 KiB request lines/headers for maximal album
continuations plus selectors; do not configure a shorter URI limit that rejects
valid protected API pages.

Expression indexes use the same effective-title/BINARY tuple as the public card.
Real metadata override/clear and source rename transactions update those keys
atomically without a separate cache or source probe. A returned continuation does
not reread its anchor after a rename/deletion. These remain live pages: concurrent
title changes can move records across the boundary or repeat/skip a record compared
with an earlier view; immutable offline snapshots are not enabled.

### Album high-rating ordering

`sort=top_rated` compares the same public native SQLite arithmetic mean as
`rating_avg`, descending, then album ID descending. Unrated nulls come last; their
internal zero seek key is never exposed as a real zero average. Equal means tie
by ID even when their voter counts differ. There is no Bayesian score, hidden
vote-count weight, display rounding or popularity surrogate.

Migration0025 backfills a nullable native-AVG projection without changing shared
resource versions, indexed times, preferences or historical events. Three SQL
triggers maintain it within actual vote insert/update/clear/move/delete transactions,
including account cascades. The existing rating and account-deletion services still
own business versions and the single complete album journal; triggers do not invent
separate audit/change writers. All readers and the journal use the same mean.

Four expression indexes share the exact `COALESCE(rating_avg,0),id` comparator,
with library/source/person prefixes. Shared authorization and literal selectors
still run in SQL before LIMIT; tag selection uses its indexed edge. No whole-
catalog sort, temporary rank table, asynchronous stale result or source probe is
needed. Average recomputation reads the indexed votes of affected albums only.

Cursors freeze the entire published float64 mean and ID, using a canonical
round-trippable position, not rounded star text or a mutable anchor lookup. Null
has one encoding; nonfinite, noncanonical, out-of-range, missing and overlong keys
return `400 INVALID_CURSOR`. Rating positions fit the existing 32-byte numeric
bound; the album-only 8192-character envelope and other endpoints' 512-character
limits are unchanged. Real re-rating, clearing and account deletion can move
records across live pages; an issued anchor remains frozen, but this is not an
immutable offline snapshot. Popularity/view statistics remain separate work.

Lists use the standard `{items,next_cursor,has_more}` envelope, default limit 30,
maximum 100, and stable indexed keyset order. Visibility and selection run in SQL
before LIMIT, using finite library/source/person/time/title/rating indexes and tag
edges rather than loading/sorting the whole catalog in memory. Cursors bind the current
account, scope, collection and literal selectors/sort; query key order/URL encoding
is canonicalized, but changing a selector or sort requires a new first page. Every
continuation rechecks current grants.
Counts/covers use the same media predicate. Invalid cross-source relationships or
a cover pointing to another album cannot leak an otherwise hidden media ID or count.
No absolute source or cache path is included.

These are live metadata pages, not immutable offline snapshots; concurrent edits
can move records between pages. Full text search, alternative album orderings,
media processing, the browsing UI, and offline synchronization are still being built.
All responses are private and `no-store`; no service worker globally caches the API.
