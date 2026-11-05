# Codec metadata sandbox

## Current scope

GalleryGo has two finite fixed metadata profiles: `/usr/bin/ffprobe` and
`/usr/bin/vipsheader`, both inside the same mandatory Linux boundary. The latter
returns only its default geometry summary, never raw all-fields/history output.
They support [controlled AVIF/video metadata](media.md); this is not full HEIF/TIFF/
RAW decoding, generated variants, a new API or a configurable command runner.
The current container does not bundle these tools.

The caller supplies an already-confined, read-only regular file descriptor, not a
source path. A private self-exec child starts before configuration, database,
listener, or web-asset initialization. It accepts only three bounded numeric
resource limits, a private trusted-installation epoch and inherited descriptor 3.
The parent self-execs `/proc/self/exe`, so replacing its pathname cannot switch the
Go helper image mid-generation. It never accepts a shell, executable,
URL, filename, environment override, field selector or arbitrary option from an API caller.

## Mandatory isolation

Only Linux amd64 and arm64 have an implementation. The child requires all of:

- Landlock ABI **3 or newer** and `no_new_privs`. The input receives read access
  only. Payload reads are exact canonical tool, ELF-loader, recursive dependency,
  selected HEIF-module/decoder files and optional `/etc/ld.so.cache` grants. The
  image profile may enumerate only selected trusted module folders: `READ_DIR`
  does not permit reading an unselected module's contents. No source root,
  database, cache, temp/home tree or broad library-file grant exists.
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

The image profile fixes concurrency to one, disables operation/file/memory caches
and emits only the default fixed-FD summary. It does not request `-a`, `-f`, history,
extension blocks or arbitrary metadata. Environment variables are replaced rather
than inheriting plugin paths or private credentials. Only pinned HEIF modules and
selected decoders may be read; unrelated Magick/PDF/OpenSlide modules are not granted.

Missing tools/dependencies, unavailable kernel features, denied sandbox syscalls,
or failed limit installation stop execution. Do not disable seccomp, grant
privileged mode, or run an unrestricted fallback to make an unsupported runtime
appear functional. Read-only source mounts remain mandatory defense in depth.

## Trusted runtime epoch

Before opening a metadata writer, the scanner independently pins both fixed
profiles' canonical tool/loader/ELF/module/cache file identities, sizes, modes and
modification/change times. Missing profiles retain an explicit unavailable pin.
The child calculates
that private epoch from the same `O_PATH` descriptors that receive Landlock grants.
A replaced installation cannot be used under an old generation policy. This is
an installation-identity fence for **trusted immutable system files**, not a full
content hash or permission to load libraries from a NAS. Stop/restart workers
around system tool/library upgrades; in-place modification of a supposedly
immutable trusted runtime is not a supported deployment workflow. Source media
still requires its independent full SHA-256 recheck after probing.

## Resource policy

These are trusted metadata-probe limits, separate from future transcoding policy.
The scan inspector uses the default budgets and its existing `inspection.timeout`
for wall time; other sandbox budgets are not public YAML/API options:

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

FFprobe additionally caps demuxer probe size at 8 MiB, analysis time at 5 seconds,
stream count at 64, per-stream probe packets at 256 and decoder pixels at one
billion. The inspector may impose a lower dimension/pixel/duration policy.

Address space includes the codec and shared-library mappings, not just image
pixels or resident memory. A syntactically valid smaller budget need not fit every
installed codec build. Budget exhaustion is failure, never partial acceptance.
The video inspector additionally enforces dimension/pixel/duration limits. Native
image frame/header limits are not an estimate of the number of video frames.

Parent cancellation, wall expiry, or pipe overflow kills the isolated process
group and waits for cleanup. Parent death requests `SIGKILL`. Process creation is
also denied inside the codec. The parent's limits are unchanged. Kernel-stalled
filesystem calls still depend on operating-system/NAS I/O timeouts; this sandbox
is not a promise of immediate cancellation of uninterruptible kernel I/O.

## Output and publication boundary

Only a narrow JSON projection is requested: stream identity/type/codec, encoded
geometry/duration, attached-picture disposition, whitelisted rotation/display-matrix/
capture tags and format identity/duration. The fixed `V:0` selector excludes covers
and chooses only the first real video stream. Raw diagnostics are bounded and
discarded, not returned or logged as API details. Failure returns no partial metadata.

The image summary parser allows at most 1 KiB, exactly one fixed `/proc/self/fd/3`
line, bounded positive geometry/bands, supported numeric pixel types and safe enum
identifiers. Extra lines, paths, field maps and unknown syntax are rejected. It
cross-checks native primary/grid geometry without publishing the summary itself.

Sandbox output is still **untrusted**. Before publication, the scan inspector
validates this projection, normalizes capture times to UTC milliseconds without
guessing timezones, enforces type/resource limits, rehashes the source, and binds
results to inspector/runtime policy, source configuration, generation, revision
and live job ownership. No codec or filesystem work may enter a SQLite writer.
The existing atomic authorization/outbox/publication fences remain mandatory.

## Regression checks

On Linux amd64/arm64, install trusted FFprobe and libvips tools at `/usr/bin/ffprobe`
and `/usr/bin/vipsheader`, with the system HEIF/AV1 loader/decoder dependencies, and
use a kernel/runtime permitting Landlock ABI 3+ and unprivileged seccomp filters:

```sh
go test -race ./internal/sandbox -count=3
go test -race ./cmd/gallerygo -run '^TestCompiled(PrivateCodecChild|CLIHTTPS)' -count=1
```

The tests use generated read-only PNGs, isolated canaries, actual FFprobe, actual
kernel denial/resource checks, and an actual compiled GalleryGo child with no
config/DB/assets. Test-only children separately exercise parent pipe bounds,
active cancellation, deadlines and safe error mapping; those controlled stubs are
not codec or ingestion evidence. Mandatory-feature denial is tested without an
unrestricted fallback. ARM64 cross-compilation is not ARM64 runtime validation.
