# Codec metadata sandbox

## Current scope

GalleryGo contains a Linux isolation primitive for **one fixed metadata command**:
`/usr/bin/ffprobe`. It is not yet connected to the scan inspector. Existing scan
workers still use the [native metadata inspector](media.md); this primitive does
not add video/HEIF/AVIF/TIFF acceptance, decoding, generated variants, a new API,
or a configurable command runner. The current container does not bundle FFprobe.

The caller supplies an already-confined, read-only regular file descriptor, not a
source path. A private self-exec child starts before configuration, database,
listener, or web-asset initialization. It accepts only three bounded numeric
resource limits and inherited descriptor 3. It never accepts a shell, executable,
URL, filename, environment override, or arbitrary FFprobe option from an API caller.

## Mandatory isolation

Only Linux amd64 and arm64 have an implementation. The child requires all of:

- Landlock ABI **3 or newer** and `no_new_privs`. The input receives read access
  only. Other file grants are exact canonical FFprobe, ELF-loader, recursive ELF
  dependency files, and an optional `/etc/ld.so.cache`; no source root, database,
  cache, temporary directory, home directory, or broad library directory is granted.
- A seccomp allowlist applied with thread synchronization. Wrong-architecture and
  amd64 x32 calls are rejected. The default is denial, not unrestricted fallback.
  Network sockets, process/thread creation, ptrace, namespace/mount operations,
  filesystem mutation, arbitrary IPC, and io_uring are unavailable. Descriptor
  bookkeeping and read-only self-limit queries have argument-specific restrictions.
  `tgkill` can target only this child's own thread group, never the worker or another
  process; this supports trusted Go pre-exec preemption without cross-process signals.
- Cleared capabilities and hard child-only resource limits. Core dumps and regular
  file output are disabled. No codec-controlled subprocess or temporary-file tree
  is created.

Landlock ABI 3 is thread-local. The trusted setup goroutine stays locked to its OS
thread; no untrusted codec code runs until `exec` replaces all sibling Go threads.
The read-only input stays alive across that boundary. Other inherited descriptors
are only standard streams; the child receives no database or source-root handle.

FFprobe receives a fixed demuxer allowlist, `file,pipe` protocol allowlist, bounded
probe/analyze/allocation settings, and one decoder thread. The filesystem and
syscall rules remain necessary: protocol filtering alone does **not** prevent
malicious containers from referencing other local files. Environment variables are
replaced with fixed locale/timezone and single-thread numerical-library settings.
The parent uses `/` as the working directory, never a media directory.

Missing tools/dependencies, unavailable kernel features, denied sandbox syscalls,
or failed limit installation stop execution. Do not disable seccomp, grant
privileged mode, or run an unrestricted fallback to make an unsupported runtime
appear functional. Read-only source mounts remain mandatory defense in depth.

## Resource policy

These are internal metadata-probe limits, not public YAML/API options or the future
transcoding policy:

| Limit                         | Default               | Accepted bounds                  |
| ----------------------------- | --------------------- | -------------------------------- |
| Input regular-file size       | 32 GiB                | 1 byte..1 TiB                    |
| Address space, `RLIMIT_AS`    | 512 MiB               | 128 MiB..2 GiB                   |
| CPU, `RLIMIT_CPU`             | 10 s soft / 11 s hard | 1..60 s soft; hard is soft + 1 s |
| Wall time                     | 15 s                  | 1..60 s                          |
| Standard output               | 1 MiB                 | 1 KiB..8 MiB                     |
| Standard error                | 64 KiB, discarded     | Fixed                            |
| Open descriptors              | 64                    | Fixed                            |
| Core / regular-file output    | 0 bytes               | Fixed                            |
| FFprobe individual allocation | 64 MiB                | Fixed                            |

Address space includes the codec and shared-library mappings, not just image
pixels or resident memory. A syntactically valid smaller budget need not fit every
installed codec build. Budget exhaustion is failure, never partial acceptance.
The eventual inspector must also enforce its own pixel/frame/duration limits.

Parent cancellation, wall expiry, or pipe overflow kills the isolated process
group and waits for cleanup. Parent death requests `SIGKILL`. Process creation is
also denied inside the codec. The parent's limits are unchanged. Kernel-stalled
filesystem calls still depend on operating-system/NAS I/O timeouts; this sandbox
is not a promise of immediate cancellation of uninterruptible kernel I/O.

## Output and publication boundary

Only a narrow JSON projection is requested: stream identity/type/codec, encoded
geometry, duration/frame-rate/count, whitelisted rotation/capture tags, and format
identity/duration. Raw diagnostics are bounded and discarded, not returned or
logged as API details. Failure returns no partial metadata.

Sandbox output is still **untrusted**. Before scan integration can publish it, the
inspector must validate this projection, normalize capture times to UTC
milliseconds without guessing timezones, enforce type/resource limits, rehash the
source, and bind the result to inspector policy, source configuration, generation,
revision, and live job ownership. No codec or filesystem work may enter a SQLite
writer transaction. The existing atomic authorization/outbox/publication fences
remain mandatory.

## Regression checks

On Linux amd64/arm64, install a trusted system FFprobe at `/usr/bin/ffprobe` and use
a kernel/runtime that permits Landlock ABI 3+ and unprivileged seccomp filters:

```sh
go test -race ./internal/sandbox -count=3
go test -race ./cmd/gallerygo -run '^TestCompiledPrivateCodecChild' -count=1
```

The tests use generated read-only PNGs, isolated canaries, actual FFprobe, actual
kernel denial/resource checks, and an actual compiled GalleryGo child with no
config/DB/assets. Test-only children separately exercise parent pipe bounds,
active cancellation, deadlines and safe error mapping; those controlled stubs are
not codec or ingestion evidence. Mandatory-feature denial is tested without an
unrestricted fallback. ARM64 cross-compilation is not ARM64 runtime validation.
