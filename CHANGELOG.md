# Changelog

All notable public changes are recorded here. This project uses Conventional Commits.

## Unreleased

### Fixed

- Run isolated Go package binaries sequentially during the race/coverage gate,
  retaining all tests, test-internal concurrency, production bcrypt and the
  15-minute per-package timeout. Allow enough total CI job time for the growing
  full suite and the remaining frontend/browser checks; no production budget changes.

- Prepare codec exec arguments/environment and seccomp filters before budgets, then
  use an allocation-free private Linux handoff for AS/filter/exec. Quiesce only the
  short-lived Go helper, avoiding GC/profiling allocations under codec limits.
  Default-budget pressure and real kernel/resource regressions retain the original
  AS/CPU/timeout/descriptor/permission restrictions and do not change the parent.

- Report target renames crossing either direction of a selected scope as deferred
  failed work, rather than silently succeeding when files move out of the target.

- Retry actual SQLite busy/locked contention in worker polling and heartbeats
  without cancelling a live transaction-bound publisher. Readiness still uses
  committed timestamps; genuine database, cancellation, and lease failures remain
  fail-closed. A real writer-timeout regression covers atomic completion.

### Added

- Indexed `popular` album keysets use actual shared retained-account view totals,
  descending with ID ties and zero-view albums last. All existing selectors and
  fresh ACLs apply before LIMIT; cursors freeze the public count without resolving
  a changed/deleted anchor. No source probes, content grants or home UI are added.

- Explicit server-timed album view events and private current-account view state,
  with actor receipts, shared retained-account totals and complete private/shared
  journals. Routine views are not added to the administrator audit.
  Deleting an account removes its contribution, including hidden albums, without
  source writes. Recent browsing, download statistics and UI remain pending.

- Direct personal favorite-folder and default/unfiled album selectors intersect
  current grants, existing filters and keyset orders. Folder IDs select direct live
  assignments only; cleared rows are excluded, and foreign/missing folders yield
  empty pages. Migration0029 adds a partial collection/album index without rewriting
  data. Recursive subtree browsing, collection deletion, UI and offline sync remain
  pending; selections never grant media access.

- Owner-private favorite collection create/list/detail/rename APIs support root and
  same-owner nested folders, bounded parent/root keysets, personal strong validators,
  actor receipts and atomic private change/outbox/audit. Name identity remains exact
  binary after validation. Album assignments and shared grants are unchanged;
  explicit collection deletion and recursive subtree album listing remain pending.

- Migration0028 prepares owner-private collection hierarchies independently of the
  management API. It preserves existing folders/favorites, adds version/time and
  owner-parent keysets, and fails atomically on foreign parents, cycles or invalid
  IDs. Database guards prevent owner/ID changes, cycles and destructive REPLACE;
  nested account cascades remain intact. Explicit deletion and reparenting through
  the API are still required.

- Optional personal `favorite=true|false` album selection intersects current grants,
  library/source/creator/tag filters and existing keyset orders. True spans owned
  collections; false includes cleared and never-favorited albums. An index-only
  migration excludes tombstones from sparse favorite ID pages. Pages remain live;
  recursive subtree browsing and full normalized search are not yet implemented.

- Personal favorite GET/PUT/DELETE APIs with independent account/album validators,
  default or exact owned collection assignment, retained clear revisions and
  historical actor receipts. Real changes co-commit private change/outbox, audit
  and receipt without altering shared album versions, averages or grants. The
  migration preserves valid memberships/times and enforces same-account collection
  FKs, explicit collection removal and atomic nested account cascades. Source
  outage/scan/move preservation remains required. Collection deletion, recursive
  subtree browsing, hierarchy UI and offline sync remain separate required work.

- Indexed high-rating album keysets (`top_rated`) using the same native SQL mean
  as public cards, DESC mean/ID ties and unrated last. Complete public float keys
  survive re-rated/deleted anchors without rounding, whole-catalog sorting or fake
  counts. Migration0025 backfills a nullable native-AVG projection and four indexes;
  transactional vote triggers cover inserts, updates, clears, moves and deletions,
  while existing rating/account-deletion writers own business versions and the
  single canonical journal. Fresh shared visibility, real100k query plans and
  actor-scoped cursor bindings remain mandatory. No source I/O or grant change.

- Personal album rating GET/PUT/DELETE APIs with one integer vote per account,
  independent identity-bound preconditions, actor receipts and retained clear
  revisions. Real changes refresh the live average and shared album version with
  complete shared/private journals, outboxes and audit in the same transaction.
  Account deletion uses that same canonical projector for all removed live votes,
  including currently hidden albums, without changing source files or preferences.
  Migration0024 preserves valid votes/times, enforces integer values, retains null
  tombstones and recreates the existing album-rating index. Source outages do not
  block metadata votes. Rating UI, interaction ranks and offline sync remain pending.

- Literal album name keysets (`name_asc`/`name_desc`) using the exact public
  effective title and BINARY/ID tuples in both directions. Case/composition/numeric
  distinctions remain explicit, never masquerading as normalized or locale search.
  Full bounded title keys survive empty/maximal/escaped values and renamed/deleted
  anchors; only album collections use the larger bounded cursor, with ordinary
  limits unchanged and no unfollowable output. Migration0023 adds four expression
  indexes maintained by existing transactional title producers, preserving history.
  No source I/O, permission change, new normalization dependency or fake ranks.

- Chronological album collection keysets (`sort=latest|oldest`) using public
  `indexed_at`, the immutable first-indexed server instant, with same-direction ID
  ties and frozen public time positions. No capture/NAS mtime/edit-time fallback or
  mutable-anchor lookup. Shared list/detail/PersonAlbums and canonical album events
  retain that key across all native/manual producers; historical rows/events are
  not rewritten. Migration0022 adds only four ordered indexes, with real100k query
  plans and atomic upgrade/rollback coverage. Name/interaction ranks, full search
  and browsing UI remain separate required work, not zero-value placeholders.

- Indexed album collection selection by exact library/source/effective-person/tag
  IDs, intersected in SQL with fresh shared visibility before bounded ID keysets.
  Unknown/hidden/inactive selectors return empty authorized pages; unsupported or
  ambiguous inputs fail explicitly. Counts/covers stay identical to album detail,
  without source I/O, global-label disclosure or new permissions. Migration0021 adds
  only the missing source+ID index; existing library/person/reverse-tag indexes,
  typed parameters and real SQLite/HTTPS cases cover the selection contract. Full
  text search, other album sorts and the browsing UI remain separate work.

- Indexed album-media kind/capture selection and ID/capture ordering in both
  directions, with explicit missing-time/tie rules, inclusive/exclusive proven-time
  bounds and bounded account/scope/album/filter-bound keysets. SQL predicates use
  fresh shared authorization before pagination; unknown/ambiguous queries fail
  rather than silently becoming unfiltered lists. Migration0020 adds only ordered
  indexes and preserves historical metadata; generated typed parameters and real
  SQLite/HTTPS regression cases cover the contract. No source I/O, complete search,
  offline snapshot, renderer/READY or download grant is implied.

- Bounded private Linux immutable-artifact reconciliation for one explicitly owned
  revision/finite-v1 static-preset cohort. Exact indexed pins, historical producer
  media/source bindings, every DB reference and matching live work fence deletion
  under exclusive cache exclusion. Entry/read-byte limits, full SHA/container/key
  validation, private inode/ancestor checks and basename-only unlink/fsync retain
  unknown/live/referenced/unsafe bytes and all shared directories. The two compiled
  maintenance families reuse existing atomic queue progress/audit/outbox without a
  new schema/ledger/consumer. Real owned SIGKILL/reopened recovery preserves cancelled
  unconfirmed units and fresh retry budgets. No public cleanup, recursive discovery,
  source writes, decoded-pixel/READY grant or renderer is enabled.

- Bounded private Linux temporary-artifact reconciliation under exclusive cache
  exclusion, using the existing live queue/attempt budget and atomic progress/
  change/outbox/audit. Known terminal static jobs, exact media/source dependencies,
  old generated private single-link temp inodes and absence of every DB variant
  reference are required. Active source warnings stop deletion; unknown/live/
  referenced/unsafe entries, shared directories, sources and final variants remain.
  Scope work units, explicit enumeration limits, irreversible unlink versus later
  journal failure, retry and actual owned-process SIGKILL/reopened recovery retain
  truthful counts. No public enqueue, consumer, scheduled/full cache sweep, renderer
  or READY publisher is enabled; no new migration, dependency or secondary queue.

- Private Linux cache-wide staging/reconciliation exclusion using shared/exclusive
  advisory locks, not another queue or DB reservation. A closeable nonserialized
  stored-artifact capsule keeps shared exclusion through a future fenced publication;
  exclusive reconciliation must observe references/liveness while holding its gate.
  Canonical private cache, pinned regular single-link zero-byte gate inode, local
  filesystem allowlist, close-on-exec descriptors and cancellable acquisition fail
  closed on unsupported hosts/filesystems or unsafe identities. Actual independent-
  process contention/SIGKILL release, concurrent SQLite writer and immutable storage
  regressions accompany the protocol. No renderer, READY, deletion or reconciler is
  enabled; encoded storage evidence remains distinct from pixels and job leases.

- Private Linux read-only static input capsules bound to the exact leased media/
  current-owned revision/finite preset and creation-bound source dependencies.
  Full source SHA streaming and root/marker/named inode/stat/ctime/mode checks run
  outside SQLite writers; fresh indexed configuration/version/lease fences follow
  the I/O. Post-work rehash and DB-only publication guards retain
  those private pins without serializing source paths or lease credentials.
  Unsupported hosts fail closed; preparation never changes source/business/
  progress/READY rows. This is source preparation only, not an enabled consumer,
  public API, decoder/renderer, pixel proof or variant publication capability.

- Bounded durable album-cover preferences with explicit per-album selection/clear,
  frozen sticky projection and media version/revision pins. Rewrites, moves, changed
  eligibility or clear fallback reject rather than retarget. A shared DB-only manual
  projector/setter preserves names/tags/creators/policy and never creates READY
  pixels or content grants. Exactly two compiled tag/cover families share queue/
  lease/budget/outcome mechanics while independent typed0019cover tables retain
  immutable history; no old tag data/schema rewrite, second queue or dynamic SQL
  selectors. Actual four-mode native scan/cover apply/no-op/clear, both-family
  SIGKILL/reopened recovery, SQLite/HTTP rollback and typed HTTPS controls accompany
  the APIs. Reprocessing and full manager UI remain separate work.

- Durable administrative album-tag batches with frozen bounded selections,
  canonical actor receipts and historical target/result pages. Both worker modes
  recheck the original active administrator, target versions/source epochs and live
  lease; real links/version/complete album events/outbox/audit/results/progress
  commit together. Resolved targets never apply twice or overwrite a later edit.
  Per-attempt budgets/cumulative counters preserve failures, cancelled unfinished
  work and already-applied skips through retry. Migrations0017/0018 retain immutable
  job-owned intent and outcomes; source edits cancel bound work. Real SQLite late-
  rollback/authority tests, native four-mode TLS/CLI apply/no-op/clear, actual SIGKILL/
  closed-process/reopened recovery and typed HTTPS controls accompany the API.
  Enqueue remains acceptance, not completion; reprocessing and manager UI remain.

- Versioned album creator classification with explicit mapped/person/none modes,
  retained indexed provenance and same-source manual selection. Source scans preserve
  deliberate classification while frozen folder-rename plans use mapped identity,
  never the selected override. Real graph changes revoke affected old scopes and
  atomically publish album/version/change/outbox/audit/receipt without widening grants
  or writing source files. Migration0016 backfills provenance and enforces mode/FK
  invariants; native mapping/move/no-op/rollback and actual TLS/CLI/browser cases
  accompany the API. Management UI/batch/search/offline remain separate work.

- Versioned album-tag association replacement with bounded explicit ID sets,
  same-transaction administrator/album visibility and canonical historical receipts.
  Authorized public tag pages share library/source/album policy and never enumerate
  unrelated catalog names or claim an album-only ETag for independently renamed
  labels. Shared complete album upserts retain tag IDs across metadata edits and
  native scans/moves; policy events remain separate and preserve links. Clear removes
  only associations, never tags/files or restrictions. Atomic rollback, real TLS/CLI
  scan retention and typed browser flows accompany the API; batch/UI/search remain.

- Versioned administrative tag catalog with bounded scope-bound keysets, exact
  trimmed binary-label uniqueness, current-admin checks and actor-bound historical
  create/rename/delete receipts. Tag version/tombstone/change/outbox/audit/receipt
  writes are atomic; no-op saves only a receipt and assigned tags reject deletion
  without cascading album links. Migration
  0015 preserves existing identities/associations, adds positive versions and a
  reverse-edge lookup index. Search normalization, association/batch manager/UI and
  source/content capabilities remain separate work.

- Versioned administrative Person display-name GET/PATCH with current source-bound
  visibility, strong identity/version preconditions and actor-bound historical
  receipts. Explicit empty strings clear the override, while missing/null/unknown
  fields fail closed. Name/version/complete person event/outbox/audit/receipt commit
  atomically, including no-op, concurrency and late-rollback cases. Actual native
  scans retain overrides and logical identities across a provable folder move without
  source writes, association edits, permission changes or content grants.

- Versioned administrative album display/cover preference GET/PATCH with strong
  resource preconditions, current same-album/source cover validation and actor-bound
  historical command receipts. Empty strings explicitly clear overrides; null and
  uncontracted fields fail closed. Business metadata/version/complete album change/
  outbox/audit/receipt co-commit, including late-failure rollback and concurrent
  deduplication. Desired-cover latches survive scans/provable folder moves without
  source writes or implicit content authority. Tags/associations/reprocessing and
  management UI remain separate work.

- Source-bound Person list/detail/album metadata APIs with fresh-session shared
  SQL visibility, authorized counts/covers and bounded account/scope/collection
  keysets. Cross-source links, hidden/inactive/unclassified people and foreign
  covers fail closed; equal folder labels retain distinct identities. Full indexed
  paths and download capability are never exposed. Real mapping/no-op/mount-loss
  CLI checks and typed HTTPS permission-latch regressions accompany the read APIs;
  management UI, richer filters/search and offline acceptance remain separate.

- Administrative indexed exact-duplicate group/member APIs using current owned
  revision SHA-256 plus byte length, shared SQL visibility and filtered-before-count
  bounded keysets. Opaque representative revision IDs prevent an overwritten media
  from retargeting old member cursors. No raw fingerprint/path, source I/O,
  perceptual claim, deletion or download authority is added. A covering signature
  index, real SQLite/HTTP/privacy/current-role cases and compiled four-mode HTTPS
  ingestion/copy/rewrite checks accompany the API. Group UI and perceptual workflows
  remain separate work.

- Private bounded static artifact storage with file/parent-directory fsync,
  post-write hashing and pinned-directory atomic no-replace rename on supported
  Linux hosts. Replays preserve existing inodes; conflicting bytes, symbolic links,
  unsafe roots and writable shared directories fail closed. Concurrent observers
  and isolated SQLite checks distinguish private files from DB READY/download
  authority. This is not a renderer, production publisher or orphan-GC completion;
  source media and codec isolation policies remain unchanged.

- Native static WebP artifact validation for the finite versioned 320/720/1920
  presets: orientation-aware no-upscale geometry, per-preset byte bounds, complete
  container checks, no metadata/animation/unknown output chunks, and immutable
  revision/preset/version/SHA keys. Source metadata compatibility is unchanged.
  This does not enable rendering, filesystem publication or DB READY; mandatory
  isolated decoding and transactional source/lease publication remain separate.

- Administrator-only paged per-entry scan diagnostics with fixed safe messages,
  validated nullable relative paths and historical attempt/source/configuration/
  generation context. Job-owned issue history survives source registration removal;
  private staging projection and pre-generation source failures co-commit with
  their existing lease/progress/event/audit fences. Available history is backfilled
  without inventing missing causes. Cursor/privacy/rollback/migration and actual
  compiled HTTPS source-outage coverage accompany the API; no source writes or purge.

- Controlled HEVC HEIF/HEIC primary still metadata through the shared bounded local
  item/grid/EXIF reader and mandatory isolated libvips header profile. Validate hvcC/
  VPS/SPS/PPS and independent one-picture NAL framing; slice count is not frame count.
  Ordered transforms, explicit-zone UTC-millisecond capture, generic brands and
  non-first primary collections retain native dimensions and unknown-null semantics.
  Inspector v11 refuses older pins. Owned independently decoded vectors, native
  malformed/resource/fuzz cases, SQLite rewrites and four 57-unit CLI modes cover
  the path. Original-only HEIF attachments do not expand inline READY MIME types.
  Full HEVC decoding, sequences, variants and container packaging remain pending.

- Controlled classic TIFF metadata through bounded native directories/local strip
  and tile extents plus the existing isolated libvips header profile. First-page
  dimensions and whitelisted orientation/explicit-zone UTC-millisecond EXIF are
  published; document pages never fabricate animation/timing. BigTIFF, recognized
  RAW/reduced/SubIFD and unknown profiles remain explicit unsupported work.
  Inspector v10 rejects historical pins. Owned endian/Deflate/tile/strip/multipage
  vectors, malformed/resource/fuzz cases, SQLite rewrites and four 48-unit CLI modes
  cover the path. TIFF originals retain attachment/extra-grant policy without
  expanding inline READY MIME types. Full decoding/variants/packaging remain pending.

- A second finite Linux codec profile using only fixed `vipsheader` geometry output,
  independent runtime pins and exact trusted HEIF/decoder files. Module-folder
  enumeration does not grant unselected module contents; both profiles retain
  Landlock/seccomp/resource/process-group boundaries. No raw metadata selector,
  arbitrary command/path or unrestricted fallback exists.
- AVIF generic-major and compatible tiled-grid still metadata with declared canvas,
  direct AV1 tile/configuration/coverage checks and isolated image-header geometry.
  Inspector v9 pins both runtimes and rejects older epochs. Owned grid pixels were
  independently decoded; real SQLite and four39-unit CLI modes verify stable
  metadata/revisions, protected originals/Range and source-outage retention. Other
  image profiles, full decoding/variants and container packaging remain pending.

- Controlled AVIF primary-item/sequence metadata with bounded ISO-BMFF item,
  property, local-extent and sample-table validation plus isolated AV1/dimension
  cross-checks. Ordered rotation/mirroring, primary-associated whitelisted EXIF,
  exact one-cycle timing and total edit-list playthroughs retain unknown-null
  semantics. Inspector v8 rejects older policy pins. Owned still/finite/infinite/
  EXIF/transform vectors, malformed/resource/fuzz tests, SQLite rewrite/publication
  and actual 33-unit CLI/HTTPS modes cover the initial path. Additional static
  generic-major/grid coverage is listed above; protected/external/
  ambiguous/unknown-essential layouts remain explicit unsupported work; this is not
  full AV1 pixel validation, READY variants or container codec packaging.

- Controlled MP4/MOV/MKV/WebM metadata through the fixed Linux FFprobe sandbox:
  primary non-attached video codec, encoded geometry, exact declared duration,
  orthogonal display-matrix orientation and explicitly zoned creation evidence
  normalized to UTC milliseconds. Inspector v7 pins trusted-runtime installation
  epochs and refuses older/mixed policy generations. Native image codecs also
  appear as nullable authorized metadata; raw tags and diagnostics remain private.
  Owned vectors include classic MOV, multiple tracks and attached-cover negatives.
  Actual CLI/HTTPS modes verify stable publication, protected video Range and
  source-outage retention. Full decoding, external images, variants and container
  codec packaging remain pending.

- Shared strict JSON validation for HTTP requests and untrusted codec projections,
  preserving recursive duplicate/case/null/type/UTF-8/nesting rejection without
  introducing an HTTP dependency into the media worker.

- Fail-closed Linux amd64/arm64 FFprobe isolation primitive with a fixed command,
  read-only descriptor input, exact-file Landlock rules, argument-restricted
  seccomp, child-only resource budgets, bounded private output and process-group
  cancellation. Actual kernel/FFprobe and compiled-child regressions are separate
  from test-only supervision stubs. Video scan integration is listed above;
  complete external metadata and container codec packaging remain pending. No
  unrestricted fallback exists.

- Bounded original EXIF subseconds in the shared JPEG/PNG/WebP whitelist, with
  inline/offset ASCII handling, ambiguity/resource checks and UTC millisecond
  normalization before immutable staging. Missing date/timezone remains unknown;
  no host-time/mtime fallback or raw tag exposure. Inspector v6 rejects older pins
  and non-millisecond staging values; actual CLI/container fixtures retain fractional
  capture time through private APIs, unchanged originals and repeated scans.

- Bounded streaming WebP RIFF/VP8/VP8L/VP8X metadata, ANIM/ANMF frame/canvas/padding/
  bitstream checks, direct total plays/declared-cycle timing and private-safe EXIF
  orientation/UTC. Reserved/future fields are ignored per format, zero timing stays
  unknown, and strict resource/version5 pins prevent stale or mixed metadata.
  Synthetic real codec vectors, malformed/resource/fuzz/race and actual CLI/HTTPS/
  rootless ingestion tests cover the container boundary, not entropy/pixel decoding
  or READY variant generation. No runtime encoder dependency is added.

- Bounded PNG/APNG container inspection with CRCs, critical chunk/palette/IDAT order,
  shared animation sequence numbers, frame/default-image/count/control checks and
  direct total plays. Exact rational delays round once per cycle; zero timing stays
  unknown. Bounded PNG eXIf reuses the private-safe TIFF orientation/UTC whitelist.
  Inspector v4, historical-pin rejection, malformed/resource/fuzz/race tests and
  actual CLI/HTTPS/rootless APNG ingestion preserve readonly bytes and identity.
  This is not zlib/pixel decoding, generated variants or complete codec acceptance.

- Bounded streaming GIF frame counts, declared single-cycle duration and normalized
  total playthroughs, exposed as authorized nullable animation metadata. Missing/zero
  delays or user-input timing stay unknown; comments/application payloads stay private.
  Strict byte/frame/duration/time limits, inspector version 3, malformed/ambiguous/
  unsupported controls, fuzz/cancellation and immutable rewrite tests cover the parser.
  Actual CLI/HTTPS and rootless container ingestion include real GIF fixtures.
  This is not full LZW decoding or READY animation/static variants.

- Bounded, whitelisted JPEG EXIF orientation and explicitly zoned original capture
  time, normalized to UTC in authorized media detail/list responses. Unknown camera
  timezones stay null; no GPS/raw tags, source paths, host timezone or mtime fallback.
  Inspector-version pins, immutable publication/rewrite tests, malformed/resource
  bounds, fuzz tests and real compiled CLI/HTTPS ingestion cover the new metadata.
  Other EXIF containers and external codecs remain pending.

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
  Whitelisted JPEG EXIF and bounded GIF timing are supported; external codecs,
  complete EXIF and other animation timing remain unimplemented. No metadata
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
