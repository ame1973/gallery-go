# Changelog

All notable public changes are recorded here. This project uses Conventional Commits.

## Unreleased

### Fixed

- Report target renames crossing either direction of a selected scope as deferred
  failed work, rather than silently succeeding when files move out of the target.

- Retry actual SQLite busy/locked contention in worker polling and heartbeats
  without cancelling a live transaction-bound publisher. Readiness still uses
  committed timestamps; genuine database, cancellation, and lease failures remain
  fail-closed. A real writer-timeout regression covers atomic completion.

### Added

- Actual native indexed-ingestion tests inside rootless/read-only containers for
  embedded and standalone consumers, with generated TLS/auth/API setup, private
  tmpfs metadata/cache and read-only source mounts. Exercise frozen SQLite plans,
  stable rename/revision IDs, protected originals/Range, outage retention/recovery,
  denied source writes and health probes; wired into the container CI job.

- Real compiled-worker SIGKILL recovery tests for native staging and partially
  published multi-source scans, with all old DB connections closed before reopened
  WAL recovery. Verify live replacement ownership, lease-expired attempt history,
  stable owned logical IDs/revisions, source-byte preservation, and duplicate-free
  versioned changes/outbox. No production checkpoint or test backdoor is added.

- Privacy-safe database/worker/source readiness with committed heartbeat freshness,
  bounded observed-source paging, degraded metadata availability during source
  outages, explicit metadata-only mode, and a listener-free CLI worker probe.
  Three-language status UI, typed contracts, stale/stopped/error tests, and an
  optional hardened Compose worker profile with the matching health check.

- Automatic initial/periodic full scans and incremental source polls in embedded and
  standalone consumers, with a strict manual-only switch, bounded transactional
  dispatch, shared manual/scheduled intent dedupe, immutable targets/dependencies,
  private poll deadlines and system audit. Failed full work never manufactures
  healthy evidence; concurrent schedulers and late rollback are tested.

- Embedded scan execution in `serve` and a standalone `gallerygo worker` command
  using the same queue, immutable roots, native handler, and strict YAML resource
  limits. Coordinated shutdown and real compiled CLI/HTTPS ingestion, repeat
  identity, source-outage and no-asset worker tests. Complete external codecs and
  metadata remain pending.

- A versioned-intent scan handler connecting durable execution to read-only inventory,
  native metadata, and indexed publication. Multi-source failures retain healthy
  results, diagnostics and progress commit atomically, live source owners are not
  stolen, and cancellation/configuration/lease fences remain mandatory. Native
  process-crash recovery is covered by compiled-worker tests.

- Administrator-only scan enqueue for full/incremental source, album-target, and
  all-source intents; bounded immutable configuration snapshots, distinct dedupe
  purposes, historical receipts, atomic dependency/audit events, explicitly nullable
  selection fields, generated contracts/types, and real HTTPS regressions. Accepted
  jobs require a compatible embedded or standalone worker; 202 is not completion.

- Durable private job-resource dependencies with creation-transaction-only binding,
  primary-resource backfill, and atomic source-change cancellation of multi-source
  parents while retaining history. Dependency binding alone does not execute jobs.

- An internal healthy-inventory publisher with fresh full-manifest revalidation,
  frozen unique rename matching, owned immutable media revisions, retained album/
  person overrides and interactions, source/path-bound people, scoped reconciliation,
  conservative missing/quarantine evidence, and atomic source/job/event completion.
  Native staging/partial-publication process-crash recovery is verified locally.

- Immutable per-path metadata staging with inspector-policy pins, streamed hash
  revalidation, explicit skipped/failed outcomes, atomic progress/issue events, and
  live metadata-ready publication fences. Staging alone does not publish indexed
  media or provide a runnable scan command.

- Bounded native JPEG/PNG/GIF header metadata with magic-based MIME, dimension/pixel
  guards, explicit invalid/unsupported diagnostics, and container-header fuzz tests.
  External codecs, EXIF, and animation timing remain unimplemented; no metadata
  inspection result is a READY variant or a full decode claim.

- Bounded root-confined inventory walking with streamed hashes, read-only regular
  inputs, cycle/escape/FIFO safeguards, source-change detection, and resource limits.
  These guards are shared by embedded and standalone native consumers.

- Fenced durable inventory generations, bounded immutable staging, co-committed
  job progress, safe issue history, and complete double-pass/minimum-file sealing.
  Staging alone never publishes indexed resources.

- Registry-selected source configuration services with independent policy epochs,
  configurable album/person folder depths, safe reachability checks, atomic scope
  revocation, and transaction-bound cancellation of stale resource jobs.
- Source activation guards and mandatory root/marker plus inventory-epoch pins for
  original bytes, preventing reachability probes from blessing stale inventories.
- Administrative source/root-name APIs, versioned updates, read-only checks, safe
  empty deletion, nested strict JSON, generated types, usage docs, and HTTPS tests.
  Automatic scheduling uses those stored source policies without filesystem I/O.

- Durable SQLite job leases, heartbeats, bounded retries/dead letters, cooperative
  cancellation, restart reclamation, and transaction-fenced completion.
- Versioned administrative job services, atomic replay receipts and event/outbox
  updates, and cumulative/per-attempt success, failure, skipped, and cancelled counts.
- Bounded worker execution with independent job/process heartbeats, cancellation
  propagation, safe handler diagnostics, and graceful retry/dead-letter settlement.
- Administrator-only job/attempt history and versioned cancel/retry HTTPS APIs,
  filter/sort-bound cursors, generated types, and replay/rollback/authorization tests.
  Native source scans are executable; complete media processing is still pending.

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
