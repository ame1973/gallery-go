# Authentication and session API

GalleryGo has no default administrator, public registration, or environment-password
fallback. Run `gallerygo migrate`, then `gallerygo bootstrap-admin --username owner`
with the same `--config` file. The CLI reads and confirms the password without
terminal echo. `--password-stdin` is an explicit automation option; pass secrets
through a private pipe, never a command argument.

## Known limitations

This development build has unresolved authentication-ordering defects. When
accounts are switched in the same browser, a delayed login or password-change
response can replace the newer account's session cookie; a delayed logout response
can clear it. A stale page can also perform an action against a different current
account. The problem is not limited to account switches: after signing in again to
the same account, a delayed login response can replace the newer session with a
different session, and a delayed logout response can sign out the browser while the
newer server-side session remains valid. Account and scope identifiers alone do not
distinguish two sessions of the same account.

These behaviors were reproduced with isolated accounts over HTTPS; delayed login and
logout were also tested with native browser `fetch`. They do not by themselves
demonstrate remote intrusion. Do not use this build with sensitive accounts or media,
or expose it to the Internet.

Session deadlines still depend on the server clock. A successfully persisted
expiry observation now revokes that token across a later clock rollback or database
reopen, but the server cannot infer unobserved elapsed time across arbitrary clock
rollback or downtime. Failed persistence cannot promise durable expiry. See
[observed expiry](#observed-expiry) for the bounded guarantee. This does not fix the
authentication-ordering defects above; cookie flags and CSRF do not fix them either.

## Passwords and usernames

Passwords use bcrypt cost 12. New passwords require at least 15 Unicode characters
and at most 72 UTF-8 bytes. Passwords are never trimmed. Oversized login passwords
are rejected instead of relying on bcrypt's comparison behavior beyond 72 bytes.
Usernames are case-insensitive ASCII identifiers of 3..64 letters, digits, dots,
underscores, or hyphens, starting with a letter or digit.

## Browser flow

1. Use the exact HTTPS origin in `base_url` through a trusted TLS reverse proxy.
2. `POST /api/v2/auth/login` with JSON `{username,password}`. The request must have
   that same `Origin`; login does not require an existing session or CSRF token.
3. The response sets `__Host-gallerygo_session` with Secure, HttpOnly, SameSite=Lax,
   Path=/, no Domain, and a random 256-bit value. The database stores only a hash.
4. Read `GET /api/v2/auth/me` to validate the current server-side account.
5. Read `GET /api/v2/auth/csrf`; send its `token` as `X-CSRF-Token` on all subsequent
   authenticated unsafe requests, together with the same-origin browser request.
6. `POST /api/v2/auth/logout` revokes the current session and expires its cookie.

The CSRF token is session-bound, stable until session rotation, and never saved in
localStorage. Session IDs returned by the management API are separate from cookie
tokens and cannot authenticate a request. Duplicate session cookies are rejected.

Every request rechecks active status, role, revocation, idle expiry, and absolute
expiry. Defaults are a rolling 24-hour idle limit and a 30-day absolute limit.
Administrators may shorten them with `auth.session_idle_timeout` and
`auth.session_absolute_timeout`; neither can exceed the default maximum.

## Observed expiry

An authentication check that observes an idle or absolute deadline first aborts any
pending business transaction, then rechecks the token and current account in a
separate SQLite writer transaction. If still expired at the observed instant, it
atomically revokes the session and records a session-delete change, durable outbox
entry, and `session_expired` audit with the reason `idle` or `absolute`. It returns
401 only after that revocation commits. Existing invalid or revoked tokens also
return 401, without creating another expiry journal.

Concurrent activity can extend the idle deadline after an older read snapshot was
opened. If the fresh check sees that extension, it leaves the valid session alone
and returns 409 `VERSION_CONFLICT`; the interrupted request does not continue with
stale authorization. Refresh current state before deciding whether to repeat an
operation. Business writes from the aborted transaction are never committed just
to preserve an expiry marker.

Database failures return a private 500 error, and context cancellation/deadline
failures return 503, rather than reporting successful expiry persistence or allowing
the requested action. No response in this expiry path sets or clears the browser's
session cookie. A successfully committed revocation survives a later clock rollback
and database reopen; a failed write has no such guarantee. This does not infer real
elapsed time that was never observed, change the configured limits, or solve delayed
authentication-response ordering across browser accounts.

## Password changes and sessions

- `POST /api/v2/auth/password` accepts `{old_password,new_password}`. It rotates
  the current cookie, CSRF token, and account scope and revokes other sessions.
  It does not extend the original absolute expiry. If the successful response is
  lost, sign in with the new password to recover.
- `GET /api/v2/auth/sessions?limit=30` returns only this account's active sessions.
  Follow `next_cursor` for subsequent pages. Limits are 1..100; cursors are bound
  to the account and its current scope.
- `DELETE /api/v2/auth/sessions/{session_id}` revokes one owned session. Hidden or
  nonexistent sessions return 404; revoking the current session expires its cookie.

All JSON request bodies are bounded to 16 KiB and reject unknown fields, duplicate
keys, incorrect field casing, explicit null fields, trailing JSON, and excessive
nesting. API responses are private and no-store. No CORS origins are enabled.

## Abuse controls and audit

Failed attempts are limited in persistent 15-minute windows by username (10) and
direct peer IP (50). Four expensive password operations may run concurrently per
process. Successful authentication releases its attempt reservation. Rate-limited
requests return 429 and Retry-After. Unknown and inactive accounts use the same
credential error as a wrong password; unknown accounts still perform bcrypt work.

Forwarded IP headers are not trusted. Behind a reverse proxy, the peer-IP budget
is shared by its clients; per-username limits still apply. Configure trusted-proxy
support only when its explicit boundary is implemented, not by spoofable headers.

Bootstrap, login, logout/session revocation, confirmed expiry, and password changes
have transactional audit records. Business changes and durable outbox records share
the same SQLite transaction. Passwords, raw cookies, and CSRF values are not audit payloads.

Active administrators can create, inspect, update, disable, delete, and reset
accounts, inspect and revoke another account's sessions, and read the audit API.
See [administration.md](administration.md) for version preconditions, idempotent
POSTs, last-active-administrator protection, and immediate online session revocation.
Group/resource ACLs and authorized metadata reads are documented in
[access.md](access.md). Grant changes rotate scope without signing out valid
sessions; account-policy and password changes revoke sessions as described above.
Dedicated administrative screens remain under development. Protected media
GET/HEAD/Range and original-download policy are described in [media.md](media.md).
