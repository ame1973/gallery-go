# GalleryGo

A self-hosted, NAS-first private gallery for photographs, animations, and video.
Source files stay on your storage; GalleryGo keeps its metadata and generated
media separately.

**Development status:** this is not yet a production-ready private gallery.
Database migrations, health checks, one-time administrator bootstrap, secure login,
password changes, session management, and three-language account pages work.
Administrative account/ACL management, scanning, media processing, browsing,
playback, and offline downloads are still being implemented. Do not expose this
build to the Internet or connect sensitive media yet.

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
rate-limit, and password contracts.

For frontend development, keep the Go server running and run `pnpm dev` in another
terminal. Vite serves `http://127.0.0.1:5173` and proxies `/api` to port 8080.
The production build is served by Go, not Vite preview. To exercise authentication
in Vite, route your trusted HTTPS development proxy to Vite and set `base_url` to
that same origin; keep Vite's `/api` proxy pointed at Go.

### CLI

| Command                                                           | Behavior                                                               |
| ----------------------------------------------------------------- | ---------------------------------------------------------------------- |
| `gallerygo migrate --config config.yaml`                          | Validate migration history and atomically apply pending migrations.    |
| `gallerygo serve --config config.yaml`                            | Require a current database, then serve HTTP and built web assets.      |
| `gallerygo doctor --config config.yaml`                           | Validate the migration history and run SQLite `quick_check`.           |
| `gallerygo healthcheck --config config.yaml`                      | Request the running server's readiness endpoint; fail if unavailable.  |
| `gallerygo bootstrap-admin --config config.yaml --username owner` | Create the first administrator using hidden password input; runs once. |
| `gallerygo version`                                               | Print the build version.                                               |

`serve` never silently runs migrations. A missing, altered, or future migration
blocks startup. Stop with Ctrl+C or SIGTERM for graceful shutdown.

## Configuration

Copy `config.example.yaml`; do not commit your `config.yaml`. YAML fields are
strict: unknown keys, duplicate keys, multiple documents, and invalid values fail
startup. Relative paths resolve from the configuration file's directory.

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
through symlinks. GalleryGo does not create source roots or mount markers.
Source configuration is validated now; inventory scanning is not implemented yet.
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
- `GET /api/v2/health/ready`: metadata database is reachable.
- `GET /api/v2/public/config`: non-sensitive version and capability information.
- `POST /api/v2/auth/login`, `/auth/logout`, `/auth/password`: authentication and
  password changes with strict JSON, Origin, and session-bound CSRF protections.
- `GET /api/v2/auth/me`, `/auth/csrf`, `/auth/sessions`: current account, CSRF token,
  and paginated owned active sessions.
- `DELETE /api/v2/auth/sessions/{session_id}`: revoke an owned session.

HEAD is supported for these GET endpoints. API responses use
`Cache-Control: private, no-store`. Errors use `application/problem+json` with a
stable `code` and `request_id`; internal paths and SQL are not disclosed.
Currently readiness does **not** claim NAS, worker, or codec readiness.

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
```

E2E starts an isolated HTTPS proxy on `127.0.0.1:4318` and a Go server on
`127.0.0.1:4319`; both ports must be free, and OpenSSL must be installed. The harness
generates a temporary TLS certificate and random bootstrap password, builds a
temporary binary/database, and runs real desktop and mobile-sized Chromium. Only
the test browser ignores the self-signed certificate; Secure cookies and Origin
checks remain enabled. Tests run serially while they share one bootstrap account.
Screenshots/traces live in ignored test-output directories. Tests never use private
source media or an existing user database.

Tests cover migration rollback/checksum/future-schema detection, concurrent
migration, per-connection pragmas, FTS5, foreign keys, config/path rejection,
HTTP errors/security headers, symlink escape, production assets, language
persistence, network failure/retry, graceful shutdown, bootstrap races, session
hashing/expiry/revocation, password rotation, CSRF, login rate limits, strict JSON,
transactional security audit/outbox, and CSP nonce matching. Real HTTPS E2E covers
login, session confirmation/focus, password changes, cookie flags, and logout.
Container smoke tests
use only ephemeral tmpfs storage and remove only their own container.

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
internal/auth/        Passwords, sessions, CSRF binding, and login abuse controls
internal/http/        HTTP endpoints and security headers
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
