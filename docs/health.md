# Liveness and readiness

`GET /api/v2/health/live` only confirms that the HTTP process is alive. It returns
`{"status":"ok"}` even if the metadata database or workers are unavailable. It is
not an authorization, ingestion, codec, or NAS-health guarantee.

`GET /api/v2/health/ready` checks local committed dependency evidence under a two-
second context deadline. It performs **no source filesystem I/O** and never changes
source state, scan generations, indexed rows, or interactions. Responses are public
but contain no source counts/IDs, paths, marker names, pins, users, or worker IDs.
All responses use `Cache-Control: private, no-store`.

## Interpret a response

```json
{ "status": "ok", "database": "ok", "worker": "ok", "sources": "ok" }
```

- `database: ok`: the local SQLite connection is reachable and dependency queries
  completed. Startup separately requires exact current migrations.
- `worker: ok`: at least one running embedded or standalone worker has a committed
  heartbeat newer than the configured lease window. Stopped/stale records do not
  qualify. This is shared processing dependency evidence, not proof of a particular
  PID, free slots, job progress, compatible codecs, or every process's root registry.
  Keep all processes configured consistently.
- `worker: optional`: the operator explicitly selected metadata-only server mode;
  no healthy worker is being claimed. This setting does not start or stop consumers.
- `sources: ok`: every active registered source matches this server's private root
  pin and has a healthy observation newer than **twice that source's poll interval**.
  Inactive sources are not dependencies; no active registrations satisfies this
  check without probing unregistered YAML roots.
- `sources: degraded`: at least one active source is unknown, unavailable, in error,
  unconfigured, stale, or has a future-dated observation. Metadata service remains
  available; `status` becomes `degraded` with HTTP **200**. The connection page shows
  the warning instead of claiming everything is ready.

A missing required worker or failed dependency query returns HTTP **503** with the
standard private `NOT_READY` Problem response, not a partially successful status.
Clients cannot make a worker optional through request parameters or headers.

Source readiness is a **recent observation**, not an on-demand NAS probe or fixed
snapshot. A source can fail after its last successful check; original requests
still validate their live root/marker and indexed configuration. Long scans or
manual-only source policies can leave observations stale. Use the administrative
[source check](sources.md) and actual job history to investigate. Healthy
reachability never proves a completed/minimum-file-safe inventory, full decode,
READY variants, or authorization to reconcile/purge.

## Required and metadata-only workers

Default configuration requires processing capacity:

```yaml
worker:
  embedded: true
  required: true
```

For a standalone consumer, set `embedded: false`, leave `required: true`, and run
`gallerygo worker` against the same local database and consistent root registry.
The server reports 503 until a worker heartbeat is available.

For deliberate metadata-only/manual maintenance operation:

```yaml
worker:
  embedded: false
  required: false
scheduler:
  enabled: false
```

`embedded: true` with `required: false` is rejected. Metadata-only mode does not
invent scan results, authorize source bytes, or disable source warnings. Accepted
jobs remain queued without a consumer. Disable scheduling in any other consumers
as well if the installation must remain manual-only.

## CLI and containers

```sh
gallerygo healthcheck --config config.yaml
gallerygo healthcheck --worker --config config.yaml
```

The first command requests the HTTP readiness endpoint. The second opens the local
current-schema database and checks shared worker/source readiness without a listener
or web assets; it always requires a fresh heartbeat, even for metadata-only server
configuration. Both print the readiness JSON and succeed for `ok` or `degraded`.
A missing worker/database dependency fails with a nonzero exit code.

The source status queries themselves are database-only. CLI configuration loading
still validates/resolves canonical paths as at startup; filesystem timeout caveats
also apply to that validation, outside the two-second dependency query context.

Use the worker form for a listener-free worker container; do not inherit the image's
HTTP health check unchanged. These are dependency checks, not per-process identity
probes; the container supervisor separately observes the main process exit. No
context can reliably interrupt a stalled kernel NAS syscall. Keep read-only mounts,
verified NAS I/O timeouts, lease fences, and final supervisor stop deadlines.
