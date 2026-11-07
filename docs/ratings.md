# Personal album ratings

All paths below use `/api/v2`. These are authenticated metadata APIs; rating UI,
popularity/view statistics and offline operation replay are not enabled yet.

## Read your vote

`GET /albums/{album_id}/rating` returns only the calling account's state and an
opaque strong `ETag`. Administrators use the same personal resource; they cannot
supply another account ID to inspect or overwrite a vote through this endpoint.
Query parameters are rejected, not treated as a target-account selector.

```json
{
  "album_id": "album-id",
  "rating": null,
  "updated_at": null,
  "version": 1
}
```

This is the state of an album that this account has never rated. After a real
change, `updated_at` is a server-generated UTC-millisecond instant. A cleared
vote is also `null`, but retains its later version and last update time.

Every request rechecks the actual session/account and shared active-library,
source-consistency and additional-album visibility in the same transaction.
Missing/hidden albums return the same `404`; expired, revoked or inactive accounts
receive `401`. A reachable source is not required for metadata votes, and no
source files are opened. A vote never grants media, original-download or offline
access. Responses remain `Cache-Control: private, no-store`.

## Set or clear a vote

1. Read the personal state above, retaining its exact `ETag`.
2. Send `PUT /albums/{album_id}/rating` with `{"rating":5}`, that tag in `If-Match`,
   a fresh `Idempotency-Key`, the configured HTTPS `Origin` and current CSRF token.
3. Use `DELETE /albums/{album_id}/rating` with the same headers and **no body** to
   clear the vote. Both successful methods return `200`, the personal state and
   its validator. Read again when current state is required.

Only integers `1..5` are accepted. Null, booleans, strings, fractional/exponent
JSON numbers, duplicate/case-folded/unknown fields and client-supplied identity or
time fields are rejected. An empty PUT object or out-of-range integer is a
validation error; an empty DELETE body is not interchangeable with `{}`.

The personal ETag binds both account and album. Do not synthesize or parse it,
reuse another account's tag, or substitute the shared album's ETag. Another
voter's change does not invalidate this personal precondition. Missing `If-Match`
returns `428`; malformed/weak/foreign/list/wildcard tags return `400`; a new
request with a stale personal version returns `409 VERSION_CONFLICT`.

There is exactly one vote per account/album. A real replacement or clear advances
the personal version. Cleared rows retain a nullable vote tombstone, so rating
again never reuses a historical revision. Replacing a vote with itself or clearing
an already empty state preserves the version, timestamp, shared album and events;
only its successful command receipt is stored. An account that never voted does
not need a stored tombstone just to clear an empty initial state.

## Retrying a response you did not receive

Use the identical method/body, key and original `If-Match` within the 24-hour
receipt window. Keys are scoped to the actor and shared with other receipted
commands. Reusing a key with a different request returns `409 IDEMPOTENCY_CONFLICT`.
Keys must contain 16..128 ASCII letters, digits, underscores or hyphens; a random
UUID is suitable, never a credential.

A matching retry returns its **historical personal result**, without replaying the
vote or restoring a subsequently cleared/replaced value. `GET` refreshes current
state. Every retry still requires a current session and current album visibility;
losing a grant blocks historical replay through this route. After expiry, inspect
the current state and use its version rather than blindly repeating an old write.
The monotonic vote revision survives receipt expiry.

## Shared averages and deletion

Authorized album list/detail/creator-album cards use the same transactionally
maintained native SQLite `AVG(rating)` over the album's votes. A cleared null vote
contributes nothing; no remaining votes means a null average, not zero. This is
an aggregate, not a public list of voters. Votes belong to accounts, not sessions,
so logout or account deactivation does not silently discard them. The
[high-rating order](access.md#album-high-rating-ordering) uses this same complete
public mean, with no hidden vote-count weighting.

A real vote change advances the shared album version and writes the complete
canonical album event with the current average. It also writes a private
`album_rating` upsert or clear tombstone in the actor's current scope. Business
rows, both change/outbox pairs, audit and receipt commit together; a late failure
rolls them all back. Shared album metadata/preference edits do not change the
personal vote revision. Conversely, rating changes can invalidate a previously
read shared album metadata precondition; refresh before editing.

Deleting an account removes its live votes and null tombstones. Each affected
live-vote album receives a new shared version and the **same canonical album
projection**, including albums the account can no longer see. Empty tombstones
do not invent a rating change. Account deletion, aggregate events, existing
account revoke/tombstone and audit are one transaction. Shared identities, display
or cover preferences and source files are preserved. Use deactivation instead
when private application metadata must remain recoverable.

Migration0024 preserves existing valid integer votes/timestamps with initial
version1 and recreates the existing `idx_ratings_album`. The rebuilt leaf table
retains enforced user/album foreign keys. An invalid historical fractional vote
fails migration atomically rather than being rounded or silently deleted.
Migration0025 adds a nullable native-AVG projection, maintained by transactional
vote triggers, and four high-rating album indexes. It does not change personal
revisions or introduce a second vote index, queue or journal. Stop consumers and
use the normal backup/migrate/start procedure; never alter real source files to
repair a metadata problem.

This does not enable offline conflict resolution, popularity statistics or the
rating/home UI. Future sync must preserve these personal versions, account scopes,
fresh grants, idempotent outcomes and explicit conflicts instead of last-write-wins.
