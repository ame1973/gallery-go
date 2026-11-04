# Authentication and session API

GalleryGo has no default administrator, public registration, or environment-password
fallback. Run `gallerygo migrate`, then `gallerygo bootstrap-admin --username owner`
with the same `--config` file. The CLI reads and confirms the password without
terminal echo. `--password-stdin` is an explicit automation option; pass secrets
through a private pipe, never a command argument.

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

Bootstrap, login, logout/session revocation, and password changes have transactional
audit records. Business changes and durable outbox records share the same SQLite
transaction. Passwords, raw cookies, and CSRF values are not audit payloads.

Active administrators can create, inspect, update, disable, delete, and reset
accounts, inspect and revoke another account's sessions, and read the audit API.
See [administration.md](administration.md) for version preconditions, idempotent
POSTs, last-active-administrator protection, and immediate online session revocation.
Dedicated administrative screens, group/resource ACLs, and protected media routes
are not implemented yet.
