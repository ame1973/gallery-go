# GalleryGo

A self-hosted, NAS-first private gallery for photographs, animations, and video.
Source files stay on your storage; GalleryGo keeps its metadata and generated
media separately.

**Development status:** this is not yet a production-ready private gallery.
Database migrations, health checks, one-time administrator bootstrap, secure login,
password changes, session management, and three-language account pages work.
Administrative account/session/audit, group grants, authorized metadata, source
configuration/reachability, protected media GET/HEAD/Range, durable scan enqueue,
and job history/cancel/retry APIs are available. Embedded/standalone scan workers can
publish read-only inventories, native JPEG/PNG/GIF dimensions, whitelisted JPEG/PNG
EXIF orientation/explicitly zoned capture time, and bounded GIF/APNG frame/cycle/loop
metadata. GPS/raw tags are not exposed and unknown timing stays null. See
[media metadata](docs/media.md). Management/browsing
screens, complete media processing, playback, and
offline downloads are still being built. Do not expose this build
to the Internet or connect sensitive media yet.

## Requirements

The repository pins and tests these toolchains:

- Go **1.26.2** (`.go-version`)
- Node.js **24.14.1** (`.nvmrc`)
- pnpm **11.25.0** (`package.json`)
- A local filesystem for SQLite WAL, not an SMB/NFS database mount
- Docker Engine and Compose v2 for the container deployment

SQLite and FTS5 are included through a CGo-free Go driver. No external database,
Redis, account service, or CDN is required. Frontend fonts are served locally.
The current container is a foundation build and does not yet include media codecs.

## Local build and run

Run from the repository root:

```sh
pnpm install --frozen-lockfile
make build
cp config.example.yaml config.yaml
./bin/gallerygo migrate --config config.yaml
./bin/gallerygo bootstrap-admin --config config.yaml --username owner
./bin/gallerygo doctor --config config.yaml
./bin/gallerygo serve --config config.yaml
```

Open `http://127.0.0.1:8080/status` for the non-sensitive connection check.
For login, put a trusted TLS reverse proxy in front of port 8080 and use the exact
HTTPS `base_url` (default `https://localhost:8443`). A local Caddy example is in
[deploy/Caddyfile](deploy/Caddyfile); certificate trust instructions are in
[deploy/readme.md](deploy/readme.md). Do not disable Secure cookies or Origin checks.

At `/login`, enter the bootstrapped account credentials. `/settings/security` lets
you change your password, inspect active sessions, revoke another session with a
confirmation dialog, and sign out. A password change rotates the current session
and signs out other sessions. The language selector supports English, Traditional
Chinese, and Simplified Chinese. `/status` remains available without signing in.

Run bootstrap only once on a fresh installation. Subsequent launches need only
`doctor` and `serve` after applying any new migrations.
No default administrator or password exists. After migration, create the first
administrator once, using a terminal with hidden password entry:

```sh
./bin/gallerygo bootstrap-admin --config config.yaml --username owner
```

Use a unique passphrase of at least 15 characters and at most 72 UTF-8 bytes.
Usernames contain 3..64 ASCII letters, digits, dots, underscores, or hyphens and
are case-insensitive. Automation may supply a single password line through stdin
with `--password-stdin`; never put passwords in command arguments, shell history,
or environment files. Bootstrap cannot be repeated on an initialized installation.
See [docs/authentication.md](docs/authentication.md) for the cookie, CSRF, session,
rate-limit, and password contracts. Administrators can manage accounts, reset
passwords, revoke sessions, and inspect audit events through the versioned API;
see [docs/administration.md](docs/administration.md). New regular accounts have no
library grants, and the last active administrator cannot be removed or disabled.
[Job controls](docs/jobs.md) describe administrator-only source/all-source/album
scan enqueue, durable history, truthful attempt counts, versioned cancellation, and
safe retries. Enqueue returns durable queued acceptance, not ingestion. `serve`
starts a native consumer by default; [worker operation](docs/workers.md) describes
standalone execution, validated limits, real status, and the remaining codec gates.

For frontend development, keep the Go server running and run `pnpm dev` in another
terminal. Vite serves `http://127.0.0.1:5173` and proxies `/api` to port 8080.
The production build is served by Go, not Vite preview. To exercise authentication
in Vite, route your trusted HTTPS development proxy to Vite and set `base_url` to
that same origin; keep Vite's `/api` proxy pointed at Go.

### CLI

| Command                                                           | Behavior                                                               |
| ----------------------------------------------------------------- | ---------------------------------------------------------------------- |
| `gallerygo migrate --config config.yaml`                          | Validate migration history and atomically apply pending migrations.    |
| `gallerygo serve --config config.yaml`                            | Serve HTTP/assets and consume scans unless `worker.embedded: false`.   |
| `gallerygo worker --config config.yaml`                           | Consume scans without a listener or web asset dependency.              |
| `gallerygo doctor --config config.yaml`                           | Validate the migration history and run SQLite `quick_check`.           |
| `gallerygo healthcheck --config config.yaml`                      | Request HTTP database/worker/observed-source readiness.                |
| `gallerygo healthcheck --worker --config config.yaml`             | Check shared worker/source readiness from local SQLite, without HTTP.  |
| `gallerygo bootstrap-admin --config config.yaml --username owner` | Create the first administrator using hidden password input; runs once. |
| `gallerygo version`                                               | Print the build version.                                               |

`serve` and `worker` never silently run migrations. A missing, altered, or future
migration blocks startup. Stop with Ctrl+C or SIGTERM for coordinated graceful shutdown.

## Configuration

Copy `config.example.yaml`; do not commit your `config.yaml`. YAML fields are
strict: unknown/duplicate keys, nulls, anchors/aliases/merges, multiple documents,
excessive nesting, and invalid values fail startup. Relative paths resolve from
the configuration file's directory.

| Field        | Meaning                                                                    |
| ------------ | -------------------------------------------------------------------------- |
| `listen`     | Internal HTTP address. Default: `127.0.0.1:8080`.                          |
| `base_url`   | Exact HTTPS browser origin, without credentials, path, query, or fragment. |
| `database`   | Local SQLite file; parent directory is private application storage.        |
| `cache_root` | Separate writable location for generated media.                            |
| `web_root`   | Production Vite output containing `index.html`.                            |
| `sources`    | Source names, filesystem roots, and root-relative mount-marker filenames.  |

`auth.session_idle_timeout` defaults to `24h` and can be shortened to at least
`1m`. `auth.session_absolute_timeout` defaults to `720h`, must not exceed that
maximum, and must be at least the idle timeout. Sessions never outlive either limit.

Database storage, cache, web assets, and source roots must not overlap, including
through symlinks. Source names are unique labels of 1..64 UTF-8 bytes without
control characters; mount markers are normalized root-relative paths. GalleryGo
does not create source roots or mount markers.
Declare roots in YAML, then use the [source API](docs/sources.md) to register a
library/mapping, inspect reachability, or deactivate a source. These operations do
not scan or purge files synchronously. Enabled consumers automatically schedule
active registered sources for initial/periodic full scans and intervening polls.
Native publication still requires healthy manifest/metadata/configuration/lease
validation. Set `scheduler.enabled: false` in every consumer for manual-only operation.
See [workers and resource limits](docs/workers.md) for `worker`, `scanner`,
`inspection`, and `scheduler` YAML settings and supported native capabilities.
`.env.example` contains only a Compose port setting. The application reads YAML,
not arbitrary environment variables.

## Container deployment

See [deploy/readme.md](deploy/readme.md) for exact commands, storage permissions,
TLS guidance, health behavior, and recovery precautions.

```sh
docker compose -f deploy/compose.yaml build
docker compose -f deploy/compose.yaml run --rm app migrate --config /app/config/config.yaml
docker compose -f deploy/compose.yaml run --rm app bootstrap-admin --config /app/config/config.yaml --username owner
docker compose -f deploy/compose.yaml up -d
docker compose -f deploy/compose.yaml logs -f app
```

The container runs as UID/GID **65532**, with a read-only root filesystem, dropped
capabilities, process/memory/CPU limits, and dedicated metadata/cache volumes.
The host port binds to loopback by default. The image's Go, Node, and Debian bases
are pinned by digest. No image is pushed or published by the included CI.

## API

The public contract is [contracts/openapi.yaml](contracts/openapi.yaml).
Only implemented endpoints are advertised:

- `GET /api/v2/health/live`: HTTP process is alive.
- `GET /api/v2/health/ready`: database, required committed worker heartbeat, and
  recent observed source readiness; source warnings degrade without disabling metadata.
- `GET /api/v2/public/config`: non-sensitive version and capability information.
- `POST /api/v2/auth/login`, `/auth/logout`, `/auth/password`: authentication and
  password changes with strict JSON, Origin, and session-bound CSRF protections.
- `GET /api/v2/auth/me`, `/auth/csrf`, `/auth/sessions`: current account, CSRF token,
  and paginated owned active sessions.
- `DELETE /api/v2/auth/sessions/{session_id}`: revoke an owned session.
- `GET/POST /api/v2/admin/users` and `GET/PATCH/DELETE /admin/users/{user_id}`:
  bounded account listing and versioned administrative account management.
- `POST /api/v2/admin/users/{user_id}/reset-password`: reset a password and revoke
  all target sessions. Creation and reset require an `Idempotency-Key`.
- `GET /api/v2/admin/users/{user_id}/sessions` and
  `DELETE /admin/users/{user_id}/sessions/{session_id}`: inspect/revoke target sessions.
- `GET /api/v2/admin/audit`: private, newest-first security audit pages.
- `GET/POST /api/v2/admin/groups`, `GET/PATCH/DELETE /admin/groups/{group_id}`:
  versioned group management; deletion never clears an album restriction.
- `GET/PUT /api/v2/admin/users/{user_id}/groups`: explicit memberships with user
  version preconditions and scope revocation.
- `GET/POST /api/v2/admin/libraries`, `GET/PATCH/DELETE /admin/libraries/{library_id}`:
  library configuration; only empty libraries can be deleted.
- `GET/PUT /api/v2/admin/libraries/{library_id}/access` and
  `/admin/albums/{album_id}/access`: fail-closed grants and additive restrictions.
- `GET /api/v2/libraries`, `/libraries/{library_id}`, `/albums`, `/albums/{album_id}`:
  SQL-authorized metadata, counts, and covers. See [docs/access.md](docs/access.md).
- `POST /api/v2/admin/scans`: snapshot/deduplicate a full, incremental, source,
  all-source, or album-target scan intent into a durable queued job. No files are
  probed by enqueue; a compatible embedded or standalone worker executes the job.
- `GET /api/v2/admin/jobs`, `/admin/jobs/{job_id}`, `/admin/jobs/{job_id}/attempts`
  and `POST /admin/jobs/{job_id}/cancel`, `/retry`: administrator-only durable controls.
- `GET /api/v2/albums/{album_id}/media`, `/media/{media_id}`, and
  `/media/{media_id}/variants`: visible media and latest ready current-revision presets.
- `GET/HEAD /api/v2/media/{media_id}/variants/{variant_id}/content` and
  `/media/{media_id}/original`: protected read-only bytes, single Range, strong ETags,
  and conditional requests. Originals require explicit download policy, even for
  administrators. See [docs/media.md](docs/media.md).

HEAD is supported for these GET endpoints. API responses use
`Cache-Control: private, no-store`. Errors use `application/problem+json` with a
stable `code` and `request_id`; internal paths and SQL are not disclosed.
Readiness exposes no private source/worker details and performs no NAS I/O in the
HTTP request. Missing required workers return 503; unknown/stale/unavailable source
observations return 200 with `status: degraded`. Explicit metadata-only mode uses
`worker.embedded: false` and `worker.required: false`. See [health semantics](docs/health.md).
Neither a heartbeat nor reachability proves inventory, codec, or physical NAS acceptance.

```sh
pnpm api:lint
pnpm api:generate
pnpm api:check
```

The frontend uses generated OpenAPI types and `openapi-fetch`. A Go contract test
checks every registered API operation against the schema. Never hand-edit
`web/src/lib/api/schema.d.ts`.

## Testing

```sh
make check                         # Go race/vet, formatting, API, TS, lint, unit tests, build
CGO_ENABLED=0 go test ./...         # Pure-Go deployment path
pnpm exec playwright install chromium
make e2e                           # Real Go process + temporary SQLite + production JS
python3 -I tests/integration/docker_smoke.py gallerygo:local
python3 -I tests/integration/docker_ingest.py gallerygo:local
```

E2E starts an isolated HTTPS proxy on `127.0.0.1:4318` and a Go server on
`127.0.0.1:4319`; both ports must be free, and OpenSSL must be installed. The harness
generates a temporary TLS certificate and random bootstrap password, builds a
temporary binary/database, and runs real desktop and mobile-sized Chromium. Only
the test browser ignores the self-signed certificate; Secure cookies and Origin
checks remain enabled. Authentication tests provision an isolated account per test
through the real administrative API and run with two parallel workers. Each worker
has its own administrator session; tests never mutate the bootstrap credentials.
Before serving, Node's built-in SQLite module seeds only the newly created temporary
DB with project-isolated ACL/media metadata and real generated PNG fixtures. Source
fixtures are read-only files; a separate missing-marker fixture verifies outages.
This uses Node's test-only built-in SQLite module, which may emit an experimental
warning; there is no production seed command or backdoor route.
Screenshots/traces live in ignored test-output directories. Tests never use private
source media or an existing user database. These control/seeded-content cases
explicitly disable the embedded consumer using supported configuration. A separate
compiled CLI integration test drives real HTTPS login/library/source/scan APIs and
both worker modes against fresh databases/read-only PNGs, EXIF JPEGs, GIFs and APNGs,
including PNG EXIF, encoded dimensions, UTC capture/orientation, frame/cycle/total
loop counts, null unknown metadata, unchanged protected originals, repeat identity,
safe source outage and graceful process shutdown.
A separate real SIGKILL regression interrupts native staging and partially published
multi-source ingestion, closes all original DB handles, then reopens the server and
worker. It verifies expired-attempt history, live replacement ownership, unchanged
logical IDs/revisions, duplicate-free events and protected original bytes using
isolated read-only PNGs plus bounded-memory sparse regular inputs. It does not
represent physical NAS, power loss, complete codecs, or container ingestion.

Tests cover migration rollback/checksum/future-schema detection, concurrent
migration, per-connection pragmas, FTS5, foreign keys, config/path rejection,
HTTP errors/security headers, symlink escape, production assets, language
persistence, network failure/retry, graceful shutdown, bootstrap races, session
hashing/expiry/revocation, password rotation, CSRF, login rate limits, strict JSON,
transactional security audit/outbox, CSP nonce matching, last-active-administrator
races, safe account deletion, ETag conflicts, scope-bound pagination, and idempotent
administrative POSTs. Real HTTPS E2E covers login, session confirmation/focus,
password changes, cookie flags, logout, administrative deactivation/reactivation,
reset/deletion, cross-account session revocation, audit privacy, mandatory library
grants, additive album restrictions, safe last-group deletion, and scope/cursor
invalidation. SQLite/race tests also cover bulk revocation of 2,000 accounts and
transactional policy-version/outbox rollback. Protected media tests cover actual
browser PNG decoding, current-revision ownership, original-policy denial for users
and admins, Range/HEAD/ETag/If-Range/304/412/416, traversal/symlinks/FIFOs, source
outages, in-flight changes, and idle-write cancellation. These are serving tests,
not full codec/player or physical-NAS evidence. Container smoke tests use only
ephemeral tmpfs storage and remove only their own container. The separate ingestion
harness runs both embedded and standalone CLI consumers in rootless/read-only
containers over real TLS/auth APIs, read-only generated PNG/GIF/APNG mounts, and temporary
SQLite/cache storage. It verifies PNG EXIF and GIF/APNG frame/cycle/loop evidence, frozen SQLite
publication plans, native
metadata, original/Range authorization, stable rename/revision IDs, source outage
retention/recovery, write denial and dependency probes. Host-side fixture changes
simulate an external rename/outage; no real NAS path or Compose volume is used.

The included GitHub Actions workflow runs these checks and dependency auditing.
A checked-in workflow is not evidence that a remote CI run has passed.

## Security and current limits

Read [SECURITY.md](SECURITY.md) before deploying. In particular:

- No private media path is publicly mounted.
- Original NAS media must always be mounted read-only.
- Do not copy an open SQLite database file alone as a backup; its WAL may contain
  committed data. Tested online backup/restore tooling is a later release gate.
- Browser storage is not a vault. Offline functionality is not yet enabled; the
  completed product must disclose grant expiry, device eviction, and delayed
  offline revocation rather than promise permanent availability or immediate wipe.
- Physical NAS behavior, ARM64 runtime, Android/iOS offline media, accessibility
  assistive technology, and large-library performance have not been verified yet.
- A development-only transitive `minimatch` advisory (GHSA-g3hv-75wg-m249,
  moderate) remains in OpenAPI/ESLint tooling. Do not feed untrusted glob patterns
  to development tools. No high/critical npm advisory was reported in the initial
  audit; rerun `pnpm audit` because advisories change.
- Go vulnerability scanning must pass before release. The development environment
  requires explicit approval to execute or integrate the official Go vulnerability
  scanner. It is not currently enabled in CI and remains an unverified release gate.

## Repository layout

```text
cmd/gallerygo/         CLI entry point
internal/app/         Configuration and process lifecycle
internal/access/      Groups, grants, authorization scopes, and shared SQL visibility
internal/auth/        Passwords, sessions, CSRF binding, and login abuse controls
internal/http/        HTTP endpoints and security headers
internal/media/       Authorized metadata, immutable variant keys, and read-only content
internal/store/       SQLite and migration runner
migrations/           Embedded, checksummed SQL migrations
contracts/            Public OpenAPI contract
web/src/              Strict TypeScript frontend and generated API types
tests/e2e/            Real-browser integration tests
tests/integration/    Container smoke checks
deploy/               Docker and deployment configuration
```

Private specifications, local configuration, runtime data, and development memory
are deliberately excluded from Git and Docker build contexts.

## License and contributions

[MIT](LICENSE). See [CONTRIBUTING.md](CONTRIBUTING.md). Comments, public
documentation, and Conventional Commit messages use English. The interface supports
English, Traditional Chinese, and Simplified Chinese.
