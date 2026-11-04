# Container deployment and operations

> This build is the executable foundation, not the complete gallery. Keep it on a
> trusted development machine. It has no authentication or media ingestion yet.

## Build and initialize

From the repository root:

```sh
docker compose -f deploy/compose.yaml config --quiet
docker compose -f deploy/compose.yaml build
docker compose -f deploy/compose.yaml run --rm app migrate --config /app/config/config.yaml
docker compose -f deploy/compose.yaml up -d
docker compose -f deploy/compose.yaml exec app doctor --config /app/config/config.yaml
```

Browse `http://127.0.0.1:8080` for the public connection check only. The image uses
UID/GID 65532. Fresh named volumes inherit the image directories' ownership.
If using bind mounts instead, provision private directories with matching ownership
before starting; do not run the application as root to hide a permission problem.

Only `/data` (SQLite), `/cache` (future variants), and bounded `/tmp` are writable.
The root filesystem is read-only. Capabilities are dropped and privilege elevation
is disabled. Process, CPU, and memory limits are explicit in `compose.yaml`.
Do not put `/data` on an SMB/NFS mount. Do not share it between different hosts.

The command below creates a new throwaway container with tmpfs storage, validates
migrations and HTTP/static assets, and removes that container. It does not touch
Compose volumes or any NAS path:

```sh
python3 -I tests/integration/docker_smoke.py gallerygo:local
```

## Custom configuration

Copy `deploy/config.container.yaml` to an ignored local configuration, edit it,
and bind-mount it read-only at `/app/config/config.yaml` using a local Compose
override. Set `base_url` to the eventual HTTPS browser origin. Keep source mounts
read-only and separate from `/data`, `/cache`, and `/app/web`.

Source `name`, `root`, and `marker` are accepted configuration fields. The marker
must be provisioned by the storage owner inside the actual mounted filesystem,
not auto-created by GalleryGo. The foundation validates these fields and root
separation but does not scan sources. Media codec tooling and worker commands
are not part of this image yet.

For a port change, use the provided non-secret environment example:

```sh
cp .env.example .env
docker compose --env-file .env -f deploy/compose.yaml up -d
```

Do not mount a directory containing `.env`, TLS keys, metadata databases, or NAS
originals as `web_root`.

## TLS and reverse proxy

The application listens on internal HTTP and expects TLS to terminate at a trusted
reverse proxy. Bind the backend to loopback or a private container network. The
proxy must set HSTS on HTTPS responses and must not publicly cache `/api` or any
future authenticated media route. Preserve request methods, Origin, cookies,
Range, If-Range, and streaming response bodies. Do not add a public static alias
for source or variant storage.

The current connection page can be smoke-tested over loopback HTTP because it
contains no private data. That does not waive the HTTPS-only session requirements
for future authenticated builds.

## Health and logs

```sh
docker compose -f deploy/compose.yaml ps
docker compose -f deploy/compose.yaml logs --tail=100 app
docker compose -f deploy/compose.yaml exec app healthcheck --config /app/config/config.yaml
```

- `/api/v2/health/live` only proves the process is alive.
- `/api/v2/health/ready` currently checks the metadata database.
- `doctor` checks migration history and SQLite integrity.
- Logs are structured JSON on stderr. API failures include a request ID.

NAS, worker-heartbeat, codec, disk-budget, and operational dashboard checks are
still release work, not capabilities to infer from a successful health response.

## Upgrades and recovery

1. Preserve a consistent database backup and the private configuration first.
2. Stop the application before replacing a foundation build.
3. Build the new image, run explicit `migrate`, and start it again.
4. Check `doctor`, readiness, logs, and the browser connection page.

Never edit an applied migration to make its checksum pass. A future or altered
schema deliberately blocks startup. Restore a matching application/database pair.
There is no destructive automatic rollback and no legacy GalleryGo migration.

Do not copy only `gallerygo.db` while it is open in WAL mode. For now, stop all
processes using the database before taking a filesystem backup of the complete
metadata directory and private config. Online backup and a measured clean-environment
restore drill are not yet implemented or verified. Retain the original backup;
do not overwrite it with the restored working copy.

`docker compose down` stops the deployment without removing named volumes.
Do not add `--volumes` unless you explicitly intend to destroy your local metadata
and cache. Source media must never be part of such a cleanup operation.

## Verification boundaries

Linux amd64 image build and rootless/read-only smoke tests are automated. An ARM64
cross-build does not prove ARM64 runtime behavior. Physical NAS failures and actual
Android/iOS browser storage/seek behavior require separate device evidence before
a production release.
