# Administrative account API

Account administration is currently available through the HTTPS API. The dedicated
user-management and audit screens are not implemented yet. Group grants, resource
ACLs, and media serving are separate features still under development; creating a
regular account does not grant access to a library.

Use an HTTPS client with cookie support. Sign in as an active administrator and
obtain the session-bound CSRF token as described in [authentication.md](authentication.md).
Send the configured `Origin` and `X-CSRF-Token` on every mutation. Do not place
passwords or cookie tokens in URLs, command-line arguments, or shell history.

All paths below are relative to `/api/v2`. Responses are private and `no-store`.
Only active administrators can use these endpoints. An ordinary user receives
`403 ADMIN_REQUIRED`; a missing, expired, revoked, or disabled session receives
`401 UNAUTHENTICATED`. Write transactions recheck the administrator's session and
role before changing data.

## Create an account

`POST /admin/users` accepts:

| Field               | Required | Meaning                                                                                                                              |
| ------------------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| `username`          | Yes      | Case-insensitive 3..64-character ASCII identifier; letters, digits, dots, underscores, and hyphens, starting with a letter or digit. |
| `password`          | Yes      | At least 15 Unicode characters, at most 72 UTF-8 bytes; never trimmed.                                                               |
| `role`              | No       | `user` by default; explicitly choose `admin` to grant management authority.                                                          |
| `active`            | No       | `true` by default; `false` creates an account that cannot sign in.                                                                   |
| `download_original` | No       | `false` by default, including for new administrators. This is the explicit original-download policy, not a library grant.            |

Supply a new `Idempotency-Key`, preferably a UUID, for each intended creation.
The response is `201` with the account JSON, a `Location` URL, and a strong `ETag`.
A duplicate username returns `409 USERNAME_TAKEN`.

Account responses contain `id`, `username`, `role`, `active`, `scope_id`, `version`,
and `download_original`. They never contain password hashes or session tokens.
The scope identifies an authorization epoch; it is not a login capability.

## List and inspect

- `GET /admin/users?limit=30` lists accounts in ascending stable ID order.
- `GET /admin/users/{user_id}` returns one account and its current `ETag`.
- List limits are 1..100, default 30. Follow `next_cursor` while `has_more` is true.
  Cursors are opaque and bound to the calling account's current scope and the
  collection URL. Do not edit them or reuse them for another account or collection.

There are no unbounded list responses or offset-based pages. These live keyset
pages are not an offline synchronization snapshot.

## Versioned policy changes

Read the account, then send its exact `ETag` as `If-Match` on:

- `PATCH /admin/users/{user_id}`
- `DELETE /admin/users/{user_id}`
- `POST /admin/users/{user_id}/reset-password`

Tags have the quoted form `"user:<id>:<version>"`. Wildcards, weak tags, multiple
tags, and tags for another account are rejected. Missing `If-Match` returns
`428 PRECONDITION_REQUIRED`; a stale version returns `409 VERSION_CONFLICT`.
Reload the account and resolve the change instead of silently overwriting it.

PATCH accepts one or more of `username`, `role`, `active`, and `download_original`.
Omit unchanged fields; explicit `null`, unknown fields, credentials, scope IDs, and
caller-supplied versions are rejected. A real change increments the version,
rotates the account scope, and revokes **all** of that account's sessions. Editing
your own account therefore signs you out. A no-op preserves the version and
sessions. Reactivating an account never revives its old sessions.

The last **active** administrator cannot be deleted, disabled, or demoted. Such a
request returns `409 LAST_ACTIVE_ADMIN`, including concurrent requests attempting
to remove the remaining administrators. Create and verify another active
administrator before retiring the current one. Disabled administrators do not
satisfy this invariant.

## Reset a password

`POST /admin/users/{user_id}/reset-password` accepts `{ "new_password": "..." }`,
with the same password policy as creation. Supply both `If-Match` and a fresh
`Idempotency-Key`. The target stays active or disabled as before.

The successful response contains the new account version and ETag. Resetting a
password rotates the scope and revokes every target session; resetting your own
password also expires your cookie. Sign in again using the new password. The
administrator does not receive a target session or impersonation capability.

## Retry administrative POSTs safely

Creation and password reset require a 16..128-character idempotency key containing
only ASCII letters, digits, underscores, or hyphens. Never use a credential as the
key. The replay window is **24 hours**, scoped to the calling administrator.

- Retry a lost response with the same operation, body, key, and original `If-Match`.
  The original result is returned without repeating the mutation or its events.
- A reset retry does not revoke sessions created after the original reset.
- Reusing a key for a different operation, body, or precondition returns
  `409 IDEMPOTENCY_CONFLICT`.
- Concurrent first attempts may return `409 IDEMPOTENCY_IN_PROGRESS` with
  `Retry-After: 1`. Retry the identical request with the same key.
- An expired key can be used for a new operation. After the replay window, inspect
  current account state before deciding to repeat a request.

Receipts are stored atomically with the mutation and are pruned on the next
successful administrative POST after expiry. Password equality on retries uses
bcrypt hashes at the production cost, never plaintext or a fast password digest.
No passwords, cookie capabilities, or CSRF tokens enter audit or change payloads.

## Delete an account

`DELETE /admin/users/{user_id}` requires its current `If-Match` and returns `204`.
This permanently removes the account, sessions, memberships, favorites, favorite
collections, ratings, playlists, playback progress, and its sync-operation
receipts. Deleting your own account expires your cookie.

Security audit history remains. Shared libraries, albums, media metadata, and
source files are **not** deleted. Use account deactivation instead if the person's
private application metadata must remain recoverable. An API client should obtain
explicit confirmation before submitting a deletion; the API does not silently
turn a stale version into a successful delete.

## Sessions and audit

- `GET /admin/users/{user_id}/sessions` lists that account's active sessions using
  the same bounded pagination. `current` identifies only the calling session.
- `DELETE /admin/users/{user_id}/sessions/{session_id}` revokes the specified
  session and records the administrator as actor. A session belonging to another
  account or an already revoked session returns 404. Revoking your current session
  expires its cookie.
- `GET /admin/audit` returns newest-first keyset pages. Events include account
  creation/changes/deletion, password resets, and administrative session revocation,
  in addition to authentication events. Each event has an opaque string `id`,
  `actor_user_id`, `event_type`, `resource_type`, `resource_id`, `detail`, and UTC
  `created_at`. Nullable references are explicit.

The business change, scope revocation, change log, durable outbox, audit record,
and successful POST replay receipt commit together. A failure rolls all of them
back. An online revocation does not promise deletion of already downloaded bytes
from an offline device; offline downloads are not enabled in this build.
