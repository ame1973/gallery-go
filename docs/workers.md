# Scan worker operation and resource limits

GalleryGo runs a native scan consumer inside `serve` by default. To use a dedicated
process instead, set `worker.embedded: false` in the server's private YAML, then run:

```sh
./bin/gallerygo worker --config config.yaml
```

Both modes require an explicitly migrated, current database. The standalone worker
opens no HTTP listener and does not require built web assets. It consumes the same
queue and immutable targets as the embedded worker. Use the same local SQLite file
and consistent source names, paths, markers, and limits in every process. Do not
share a WAL database between hosts or put it on NFS/SMB.

## Start an actual scan

1. Declare read-only roots and existing regular mount markers in YAML.
2. Sign in over the configured HTTPS origin as an active administrator.
3. Create a destination library and register a declared root through the
   [source API](sources.md). A new library grants no ordinary-user access.
4. [Enqueue a scan](jobs.md) using `POST /api/v2/admin/scans` with the desired
   full/incremental mode and source, album, or all-source selection.
5. Follow the returned job and attempt history. `202` means accepted, not completed.
   Check terminal status and actual failed/skipped/cancelled counts.

Startup and source registration alone do not scan. Automatic scheduling is not yet
wired; enqueue explicitly. A worker claims only supported `scan` jobs. It rejects
legacy or malformed private intents rather than guessing roots or scopes. The
server can operate without an embedded consumer, but queued work will not progress
until a compatible standalone consumer starts.

The native pipeline performs bounded manifests and streamed hashes, JPEG/PNG/GIF
header metadata, and fenced indexed publication. Indexed resources and authorized
original downloads become available after publication. Header metadata is not a
full decode, EXIF/animation support, a generated preview, or a READY variant.
Unsupported/corrupt media remains explicit failed work. External codecs and variant
processing are still under development.

## Strict YAML policies

Omitted fields use the following defaults. Values are validated even when the
embedded consumer is disabled. Duration values require strings such as `5s`, `1m`,
or `6h`, not integer nanoseconds. Unknown/duplicate keys, null values, anchors,
aliases, merge keys, multiple documents, and excessive nesting are rejected.
Relative storage paths resolve against the configuration file, never the current
working directory.

```yaml
worker:
  embedded: true
  concurrency: 2
  lease: 30s
  heartbeat: 5s
  poll: 500ms
  max_attempts: 3
  retry_base: 1s
  retry_max: 5m
scanner:
  max_depth: 64
  max_files: 1000000
  max_file_bytes: 34359738368
  batch_size: 128
  timeout: 6h
inspection:
  max_pixels: 120000000
  max_dimension: 65535
  header_bytes: 8388608
  timeout: 15s
```

| Setting                    | Allowed values and meaning                                               |
| -------------------------- | ------------------------------------------------------------------------ |
| `worker.embedded`          | Boolean; controls `serve` only. `worker` always starts a consumer.       |
| `worker.concurrency`       | 1..16 concurrent jobs **per process**. Multiple processes add capacity.  |
| `worker.lease`             | 3s..10m live ownership window. Expired owners cannot publish.            |
| `worker.heartbeat`         | At least 500ms and at most one third of the lease.                       |
| `worker.poll`              | 100ms..5s idle queue polling. It is not source scheduling.               |
| `worker.max_attempts`      | 1..10 initial automatic attempts, with retained history.                 |
| `worker.retry_base`        | 100ms..1m initial bounded exponential backoff.                           |
| `worker.retry_max`         | At least `retry_base`, at most 1h.                                       |
| `scanner.max_depth`        | 1..256 source-relative directory levels.                                 |
| `scanner.max_files`        | 1..10,000,000 inventory paths per source.                                |
| `scanner.max_file_bytes`   | 1 byte..1 TiB per input; default 32 GiB. Oversize fails closed.          |
| `scanner.batch_size`       | 1..256 staged paths per batch; memory stays bounded.                     |
| `scanner.timeout`          | 1s..24h for one source's complete pipeline, including ownership waiting. |
| `inspection.max_pixels`    | 1..1,000,000,000 image pixels. No native pixel allocation.               |
| `inspection.max_dimension` | 1..1,000,000 pixels per dimension.                                       |
| `inspection.header_bytes`  | 512 bytes..32 MiB of native header inspection input.                     |
| `inspection.timeout`       | 1s..1m per native header inspection.                                     |

Source-specific mapping, minimum-file and scheduling policies remain API resources,
not arbitrary filesystem paths or commands in job payloads. Target/incremental
scans still verify the complete source before scoped reconciliation. Intermediate
sources cannot finalize a multi-source job; one bad source does not erase healthy
sources' committed work or turn all-source status into false success.

## Failure and shutdown

Missing roots/markers are never created. A failed source retains its old published
index and interactions. Safe source diagnostics, failed generation evidence, job
progress, events/outbox, and audit commit atomically under live lease/configuration
fences. Transient root/source/I/O failures use bounded retries and eventually `dead`;
minimum/resource/unsafe-entry guards fail without publishing that source.

Ctrl+C or SIGTERM stops new claims, cancels active handlers, and settles only still-
owned attempts with the configured retry budget. The embedded HTTP server and worker
stop together if either component fails; storage closes after both stop. Another
live source generation is waited for without invented progress. Real SQLite writer
contention is not counted as a successful heartbeat or new ownership.

Use read-only mounts and appropriate NAS/kernel I/O timeouts. Go contexts cannot
reliably interrupt a stalled kernel filesystem call; container supervisors may need
their configured final stop deadline. Never weaken lease/config fences or edit job
rows to recover a stalled source. Use the administrative cancel/retry APIs.

Current readiness still checks the metadata database only. Source/process readiness,
automatic scheduling, complete external metadata and indexed SIGKILL/reopened-DB
acceptance remain pending; a successful startup or native scan is not the complete
source-processing phase or a production-release claim.
