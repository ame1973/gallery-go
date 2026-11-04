# Changelog

All notable public changes are recorded here. This project uses Conventional Commits.

## Unreleased

### Fixed

- Retry actual SQLite busy/locked contention in worker polling and heartbeats
  without cancelling a live transaction-bound publisher. Readiness still uses
  committed timestamps; genuine database, cancellation, and lease failures remain
  fail-closed. A real writer-timeout regression covers atomic completion.

### Added

- Immutable per-path metadata staging with inspector-policy pins, streamed hash
  revalidation, explicit skipped/failed outcomes, atomic progress/issue events, and
  live metadata-ready publication fences. This does not yet publish indexed media
  or provide a runnable scan command.

- Bounded native JPEG/PNG/GIF header metadata with magic-based MIME, dimension/pixel
  guards, explicit invalid/unsupported diagnostics, and container-header fuzz tests.
  External codecs, EXIF, and animation timing remain unimplemented; no metadata
  inspection result is a READY variant or a full decode claim.

- Bounded root-confined inventory walking with streamed hashes, read-only regular
  inputs, cycle/escape/FIFO safeguards, source-change detection, and resource limits.
  Complete generation publication and runnable scan workers are not available yet.

- Fenced durable inventory generations, bounded immutable staging, co-committed
  job progress, safe issue history, and complete double-pass/minimum-file sealing.
  Indexed media reconciliation and runnable scan workers are not yet wired.

- Registry-selected source configuration services with independent policy epochs,
  configurable album/person folder depths, safe reachability checks, atomic scope
  revocation, and transaction-bound cancellation of stale resource jobs.
- Source activation guards and mandatory root/marker plus inventory-epoch pins for
  original bytes, preventing reachability probes from blessing stale inventories.
- Administrative source/root-name APIs, versioned updates, read-only checks, safe
  empty deletion, nested strict JSON, generated types, usage docs, and HTTPS tests.
  Inventory workers and scheduling are not yet wired.

- Durable SQLite job leases, heartbeats, bounded retries/dead letters, cooperative
  cancellation, restart reclamation, and transaction-fenced completion.
- Versioned administrative job services, atomic replay receipts and event/outbox
  updates, and cumulative/per-attempt success, failure, skipped, and cancelled counts.
- Bounded worker execution with independent job/process heartbeats, cancellation
  propagation, safe handler diagnostics, and graceful retry/dead-letter settlement.
- Administrator-only job/attempt history and versioned cancel/retry HTTPS APIs,
  filter/sort-bound cursors, generated types, and replay/rollback/authorization tests.
  App/CLI worker wiring and source scanning are not yet available.

- Session-protected media metadata, current-ready variant URLs, original-download
  policy, GET/HEAD/single-Range, strong ETag/conditional handling, and private errors.
- Root-confined read-only files, configured source/mount-marker checks, immutable
  variant keys, in-flight change detection, and bounded idle writes without a
  short metadata timeout or an original-policy fallback.
- Real HTTPS browser PNG decoding and cross-account/Range/source-outage tests;
  synthetic files and seeded rows remain confined to temporary test installations.

- Group/membership/library/album-policy APIs with shared SQL visibility, explicit
  deny-by-default library grants, and persistent album restriction state.
- SQL-authorized library/album metadata, counts, covers, and bounded keyset pages;
  inactive libraries and inconsistent cross-source relationships stay hidden.
- Bulk authorization-epoch revocation with complete durable policy/outbox events,
  2,000-account rollback tests, and real HTTPS last-group deletion regressions.

- Administrative user CRUD, password reset, cross-account session control, and
  bounded audit APIs with strong account ETags and scope-bound pagination.
- Transactional last-active-administrator protection, immediate session/scope
  revocation, safe private-metadata deletion, and 24-hour idempotent POST receipts.
- Per-test accounts provisioned through the real API, enabling parallel HTTPS
  browser tests without sharing mutable credentials.

- Authenticated login/logout/me/CSRF/password/session APIs with strict bounded JSON,
  exact Origin checks, persistent abuse limits, and session-bound CSRF tokens.
- Three-language HTTPS login/account-security pages, Radix confirmation/focus
  handling, and real-browser password/session rotation tests.
- Per-response CSP style nonces for accessible dialogs without unsafe-inline or
  unsafe-eval exceptions, plus a local TLS reverse-proxy example.
- One-time administrator bootstrap with hidden terminal or explicit stdin password
  input, bcrypt cost 12, hashed 256-bit sessions, persistent authentication rate
  limits, atomic security events, and bounded session lifetimes.

- Local SQLite WAL/FTS5 storage and atomic, ordered, checksummed migrations.
- Strict YAML configuration with separate source, database, cache, and web roots.
- Go CLI for migration, serving, database diagnosis, health checks, and version output.
- Public health/config APIs, Problem JSON errors, security headers, and protected
  static-file boundaries without a public media mount.
- React/TypeScript connection page with English, Traditional Chinese, and Simplified
  Chinese, self-hosted fonts, generated API types, and real-browser tests.
- Digest-pinned rootless Docker foundation, deployment instructions, and CI checks.

### Compatibility

- This is a new implementation. Old GalleryGo databases and API routes are not supported.
- A complete private gallery is not yet available; this is not a GA release.
- TypeScript is pinned to 6.0.3 because the selected ESLint toolchain does not yet
  accept TypeScript 7. All direct dependencies use exact versions and lockfiles.
