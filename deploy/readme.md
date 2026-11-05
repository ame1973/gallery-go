# Container deployment and operations

> This is an authentication/access/content-serving development build, not the
> complete gallery. Keep it on a trusted development machine; native read-only scan
> workers and readiness are available; complete codec processing, gallery/player
> screens and offline downloads remain pending.

## Build and initialize

From the repository root:

```sh
docker compose -f deploy/compose.yaml config --quiet
docker compose -f deploy/compose.yaml build
docker compose -f deploy/compose.yaml run --rm app migrate --config /app/config/config.yaml
docker compose -f deploy/compose.yaml run --rm app bootstrap-admin --config /app/config/config.yaml --username owner
docker compose -f deploy/compose.yaml up -d
docker compose -f deploy/compose.yaml exec app doctor --config /app/config/config.yaml
```

Browse `http://127.0.0.1:8080/status` for the public connection check only. Login
requires the configured HTTPS origin; see TLS below. Bootstrap is interactive and
must run only once after initial migration. The image uses
UID/GID 65532. Fresh named volumes inherit the image directories' ownership.
If using bind mounts instead, provision private directories with matching ownership
before starting; do not run the application as root to hide a permission problem.

Only `/data` (SQLite), `/cache` (private variants), and bounded `/tmp` are writable.
The root filesystem is read-only. Capabilities are dropped and privilege elevation
is disabled. Process, CPU, and memory limits are explicit in `compose.yaml`.
Do not put `/data` on an SMB/NFS mount. Do not share it between different hosts.

The command below creates a new throwaway container with tmpfs storage, validates
migrations and HTTP/static assets, and removes that container. It does not touch
Compose volumes or any NAS path:

```sh
python3 -I tests/integration/docker_smoke.py gallerygo:local
python3 -I tests/integration/docker_ingest.py gallerygo:local
```

The second command requires host OpenSSL and validates actual native indexed
publication with both embedded and standalone consumers inside separate throwaway
runs. It uses a generated TLS reverse proxy, random bootstrap/stdin secret, real
administrative APIs, read-only bind-mounted PNG/GIF/APNG/WebP fixtures, and private tmpfs
metadata/cache. It verifies EXIF/UTC and image frame/cycle/total-play metadata,
plus SQLite's temporary rename/publication plans under the hardened runtime,
protected original/Range bytes, stable logical/revision/album IDs, failed-source
retention/recovery and denied source writes. Only its own fixture host directory is
renamed/modified to simulate an external source change; no real NAS or Compose volume
is read. This is native header/index execution, not full codecs or physical NAS proof.

## Custom configuration

Copy `deploy/config.container.yaml` to an ignored local configuration, edit it,
and bind-mount it read-only at `/app/config/config.yaml` using a local Compose
override. Set `base_url` to the eventual HTTPS browser origin. Keep source mounts
read-only and separate from `/data`, `/cache`, and `/app/web`.

Source `name`, `root`, and `marker` are accepted configuration fields. The marker
must be provisioned by the storage owner inside the actual mounted filesystem,
not auto-created by GalleryGo. The application validates these fields and root
separation; original reads also require the indexed source name/root to match the
configured allowlist, the private path/marker pin and current inventory epoch to
match, and a readable live marker. Register declared names and inspect reachability
through the [source API](../docs/sources.md); no API accepts arbitrary root paths
or creates a missing mount. Mount sources read-only, with appropriate
`nodev`/`nosuid`/`noexec` policy and verified NAS I/O timeouts. Root confinement does
not make stalled kernel filesystem calls cancellable. See [protected media](../docs/media.md).
The image includes native scan execution: `serve` starts its embedded consumer by
default, and `gallerygo worker --config ...` can consume without a listener or web
assets. See [worker operation](../docs/workers.md) for limits and a dedicated-process
configuration. Keep the DB on the same host and configure every process with the
same source pins. Enabled consumers schedule active registrations automatically;
set `scheduler.enabled: false` in every consumer for manual-only scans. Source API
calls do no synchronous scanning or filesystem creation. The host build supports
[controlled AVIF/video metadata](../docs/media.md) only when a trusted system FFprobe and
mandatory Linux isolation are available. Static grid/generic AVIF and controlled
classic TIFF metadata also require the fixed libvips header profile with its trusted
loader dependencies. HEVC-coded HEIF/HEIC stills additionally require the trusted
libheif HEVC decoder; AVIF requires its AV1 decoder. Neither header capability
establishes full codec or preview support. This image
still lacks those codec packages;
affected paths report explicit failed/unsupported work, not successful decoding.
Do not disable isolation or add a public original-file fallback to make it work.

For a port change, use the provided non-secret environment example:

```sh
cp .env.example .env
docker compose --env-file .env -f deploy/compose.yaml up -d
```

Do not mount a directory containing `.env`, TLS keys, metadata databases, or NAS
originals as `web_root`.

## Optional standalone worker

The `worker` Compose profile adds a listener-free consumer using the same hardened
runtime settings and local metadata/cache volumes:

```sh
docker compose -f deploy/compose.yaml --profile worker up -d
docker compose -f deploy/compose.yaml exec worker healthcheck --worker --config /app/config/config.yaml
```

By default this adds capacity alongside the embedded consumer. To dedicate processing
to the worker, mount a consistent private config into **both services**, set
`worker.embedded: false`, and keep `worker.required: true`. Mount the same declared
sources read-only into both. The server returns 503 until a committed worker
heartbeat is present. For deliberate metadata-only operation without any consumer,
set both worker flags false and disable scheduling in every remaining consumer.

The profile overrides the image's HTTP health check with the database-backed worker
probe. It reports shared dependency readiness, not a particular PID or codec proof;
the supervisor separately observes the main process. See [health semantics](../docs/health.md).

## TLS and reverse proxy

The application listens on internal HTTP and expects TLS to terminate at a trusted
reverse proxy. Bind the backend to loopback or a private container network. The
proxy must set HSTS on HTTPS responses and must not publicly cache `/api` or any
authenticated media route. Preserve request methods, Origin, cookies, ETag,
Range, If-Range, and streaming response bodies. Do not add a public static alias
for source or variant storage.

For local development, install Caddy through your operating system's trusted
package source, then run the provided proxy in a separate terminal:

```sh
caddy run --config deploy/Caddyfile
```

Trust only the CA created by your own local Caddy instance. Where appropriate,
`caddy trust` installs that local CA and may require administrator permission;
verify its identity before approving. Then open `https://localhost:8443/login`.
For a real NAS hostname, configure your trusted reverse proxy/certificate instead
and set GalleryGo's `base_url` to the exact same HTTPS origin. Do not use browser
certificate-bypass flags in production.

The public connection page can be smoke-tested over loopback HTTP at `/status`.
Authenticated mutations intentionally reject an HTTP or different Origin. Cookies
remain Secure/HttpOnly/host-only. The application shell receives a fresh style
nonce per response so Radix's scroll locking works without unsafe-inline or
unsafe-eval CSP exceptions; never cache HTML independently of its CSP header.

## Health and logs

```sh
docker compose -f deploy/compose.yaml ps
docker compose -f deploy/compose.yaml logs --tail=100 app
docker compose -f deploy/compose.yaml exec app healthcheck --config /app/config/config.yaml
```

- `/api/v2/health/live` only proves the process is alive.
- `/api/v2/health/ready` checks the DB, required committed worker heartbeat and
  recent source observations. Source warnings return 200/degraded without disabling
  metadata; missing required workers or failed checks return 503.
- `healthcheck --worker` checks shared worker/source readiness without HTTP or assets.
- `doctor` checks migration history and SQLite integrity.
- Logs are structured JSON on stderr. API failures include a request ID.

Source observations are bounded database reads, not live NAS I/O in HTTP or proof
of completed inventory. CLI config validation still resolves filesystem paths.
Complete codecs, disk-budget and operational dashboards remain release work; do not
infer them or physical NAS/device acceptance from a successful health response.

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
