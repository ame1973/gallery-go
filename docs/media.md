# Protected media API

Media metadata and read-only content endpoints use the same SQL visibility rules as
[album and library reads](access.md). There is no public source/cache filesystem
mount, fallback credential, anonymous thumbnail URL, or original-download bypass.

**Current scope:** protected serving, native image inventory and controlled video
metadata publication are implemented. Native JPEG/PNG/GIF/WebP dimensions,
whitelisted image EXIF, GIF/APNG/WebP frame/cycle/loop metadata, controlled AVIF
primary-item/sequence metadata, HEVC HEIF/HEIC primary stills, classic TIFF first-page
dimensions/EXIF and sandboxed MP4/MOV/MKV/WebM geometry/duration/creation evidence are available.
External image capabilities, normal variant publication, gallery/player screens,
and offline manifests remain under development. A fresh installation has no indexed
media until a configured source is scanned; test fixtures are not production seed
data. Video inspection requires a trusted system FFprobe and mandatory Linux
isolation; the current container still does not bundle external codecs. Metadata
and original downloads do not establish full image/video decoding or playback.

All paths below are relative to `/api/v2` and require a valid HTTPS session cookie.
Original download also requires the actual account's `download_original:true`,
**including for administrators**. Lack of resource visibility or download policy
is hidden as 404. Anonymous, expired, disabled, and revoked sessions receive 401.
The server rechecks these conditions before GET, HEAD, Range, or a conditional 304.

## Metadata and variants

- `GET /albums/{album_id}/media` lists visible media with indexed kind/capture
  filters and ID/capture sorting. An empty authorized selection returns an empty
  page; an unauthorized or missing album returns 404, even when no filter matches.
- `GET /media/{media_id}` returns ID, album ID, kind, display basename, nullable
  codec/dimensions/duration/animation/orientation/capture time/current revision, and offline
  capability. No absolute path, cache storage key, EXIF GPS field, password, or
  session capability is returned.
- `GET /media/{media_id}/variants` lists the latest **ready** preset versions for
  the current owned media revision. Failed, retired, and other-revision variants
  are not advertised. Lists use default limit 30, maximum 100, and scope/collection-
  bound cursors with `{items,next_cursor,has_more}`.

A null revision means that no valid current revision is available yet. A ready
variant includes `revision_id`, `preset_key`, `preset_version`, MIME, byte length,
SHA-256, nullable dimensions/duration, and `content_url`. The URL is a stable
same-origin cookie-protected path, not a bearer capability or an expiring URL.
Treat IDs, revisions, and account scopes as opaque.

`offline_capable` remains false until explicit scoped offline manifests are
implemented. A stable URL or a browser HTTP cache is not an offline grant.

### Album-media selection

`GET /albums/{album_id}/media` accepts only these optional parameters, each at most
once and nonempty when supplied:

| Parameter         | Meaning                                                                    |
| ----------------- | -------------------------------------------------------------------------- |
| `kind`            | `image` or `video`; image includes indexed animations                      |
| `captured_from`   | Inclusive proven capture instant                                           |
| `captured_before` | Exclusive proven capture instant, later than `captured_from` if both exist |
| `sort`            | `id_asc` (default), `id_desc`, `captured_asc` or `captured_desc`           |
| `limit`           | Default 30, maximum 100                                                    |
| `cursor`          | Opaque continuation from the same selection/account/scope/album            |

Capture bounds require RFC3339 with an explicit known UTC offset (`Z` is preferred),
exact millisecond precision, and years 0001..9999 after UTC conversion. Unknown
`-00:00`, timezone-free dates, sub-millisecond values and reversed/empty intervals
are rejected. Unknown capture values never match a date interval; source mtime,
index creation time and host timezone are not substitutes.

Capture order compares the indexed capture instant and then media ID, both in the
requested direction. Missing capture sorts first ascending and last descending;
no epoch date is returned for it. Equal timestamps retain deterministic ID ties.
Rows with a null current revision can still be listed as processing metadata;
selection does not grant READY, codec support, offline access or original download.

Filters and keysets run in SQL before the bounded `limit+1` page, under a fresh
session and the shared album/media visibility predicates. Migration0020 adds the
ordered kind/capture indexes without rewriting old metadata. There is no full-result
Go sort/load or offset pagination, and metadata selection does not open source files.

Continue with the same literal filter/sort values. Query-key order and URL encoding
are canonicalized; changing even an equivalent timestamp spelling invalidates the
cursor. Changing only page size is allowed. Capture cursors freeze the last public
millisecond/missing key rather than resolving a mutable anchor row on the next
request. These are **live keyset pages**, not an inventory/offline snapshot: concurrent
changes can move records across a boundary. Unknown or repeated parameters fail
with `400 INVALID_FILTER` (pagination syntax uses `INVALID_PAGINATION`); malformed,
foreign-account/scope/album or differently filtered cursors use `INVALID_CURSOR`.
Filename/text search, broader library/person/tag/favorite selection and full browsing
UI remain separate required work; this endpoint does not claim complete search.

### Static image preset contract

Internal artifact validation recognizes only version 1 of these static WebP
recipes. Production rendering and atomic variant publication are not enabled yet;
this validation does not create a READY row or make bytes downloadable.

| Preset         | Maximum long edge | Encoded output byte limit |
| -------------- | ----------------- | ------------------------- |
| `thumb_320`    | 320 px            | 1 MiB                     |
| `thumb_720`    | 720 px            | 4 MiB                     |
| `display_1920` | 1920 px           | 8 MiB                     |

All three fit the orientation-normalized source without enlargement. Quarter-turn
orientations swap the axes; dimensions round to the nearest positive pixel once.
Unknown orientation preserves encoded axes rather than inferring camera evidence.
Output must have exactly the expected geometry, a complete bounded static WebP
container, one frame, and no animation/timing or ICC/EXIF/XMP/unknown chunks. The
input metadata reader remains intentionally compatible with those source chunks;
accepting an original's metadata is not sufficient validation for a derived output.

The validator computes output length/SHA-256 and the existing immutable
revision/preset/version/hash storage key. It does not decode entropy-coded pixels,
prove source identity, write files, enqueue work, grant access, or publish READY.
A producer must still complete isolated decoding, rehash the source, check its
current root/configuration/revision and live lease, fsync/rename private output,
and commit READY with change/outbox/progress. Static output never claims to retain
source animation, and no original is copied or symlinked into a preview mount.

The Linux amd64/arm64 worker-side private artifact store can write validated bytes
under `tmp/{job_id}`, fsync and rehash the temporary file, and atomically rename to
the immutable key **without replacing an existing file**. Both directory handles
are confined before the basename-only rename. New directories are private and
parent directory entries are synced. Identical retries reuse the existing inode;
a corrupt, mismatched, non-private or symbolic-link target fails rather than being
overwritten. Group/world-writable directories, source/cache overlap and cache-root
aliases are refused. Missing cache roots are not created by this primitive.

Private staging also acquires a shared Linux `flock` on the persistent zero-byte
`cache_root/.gallerygo-artifacts.lock` before writing. The returned private
stored-artifact capsule retains that lock until it is closed; a future publisher
must keep it through its final source/revision/live-lease-fenced DB transaction.
Concurrent shared writers may coexist. Reconciliation must obtain an
exclusive gate **before** checking references/job liveness and retain it through
filesystem actions, rather than deleting from a stale DB absence observation.
Lock opening/waiting and filesystem checks never belong inside a SQLite writer.

The gate requires a private regular, mode-0600, single-link file with the same
pinned named inode, close-on-exec descriptors and a local Linux ext-family, Btrfs,
XFS, tmpfs or OverlayFS cache filesystem. Unknown/network filesystems fail closed;
NFS/SMB locking is not assumed equivalent. Never unlink/replace the gate file or
pass its descriptor to a codec child. Acquisition is nonblocking/context-bounded;
closing the final open description releases the lock, including after process
exit, without deleting the persistent file. A lock is not a job lease or a grace
period, and advisory exclusion protects only cooperating application processes,
not an unrelated same-UID program that ignores the protocol.

This filesystem boundary is not yet connected to a production renderer or DB
publisher. Unreferenced bytes are not listed or downloadable, even when a complete
file exists at an immutable key. Failure after a rename can leave a private orphan;
job directories remain for lease-aware reconciliation, never recursive or source
cleanup. No full orphan collector or background cleanup consumer is enabled. The
private temporary-file and known immutable-cohort protocols below have deliberately
different ownership scopes; neither discovers unknown or deleted-revision trees.
Unsupported platforms fail before writing instead of substituting an overwriting
rename. Contexts stop work between I/O operations, but cannot promise to interrupt
a kernel-stalled filesystem flush or rename.

### Bounded private temporary-file reconciliation

An internal Linux-only `reconcile_temps` batch can remove eligible interrupted
static-output temporary files. **No public enqueue endpoint, CLI command,
application consumer or scheduled cleanup is enabled.** It is not a full cache
sweep, source/index purge, final-variant collector, decoder or READY publisher.
The canonical private work identity binds the cache, job-ID continuation and
explicit bounds to the live job/owner/attempt and creation-bound cache dependency.

Each batch examines 1..100 known DB job scopes in stable ID order, after its private
cursor and before the cleanup job's own ID. Enumeration is bounded to 1..1000
entries with at most one lookahead; a limit is reported as failed/incomplete work,
not a successful full sweep. Unknown/live/nonmatching jobs are skipped. Only
terminal finite-v1 static jobs with original media/source dependencies can own an
eligible `tmp/{job_id}` scope. Any currently active source observation that is not
healthy stops destructive maintenance; no NAS probe occurs and reachability is
not inventory completion or permission to purge indexed data.

An eligible file must have the generated lowercase UUIDv7 `.tmp` name, be regular,
mode 0600, single-link, same UID/device, at most 8 MiB and at least 24 hours past its
last write. Root/tmp/job directories are private and pinned; named inode/stat changes,
symlinks, unknown names/types, non-private permissions, hard links, newer/future or
oversized files are retained. **Any DB variant reference**, including a retired or
old-revision row, prevents removal. Sources, final variants, unknown job folders,
shared directories and the persistent gate file are never removed.

The batch retains exclusive cache exclusion across fresh DB reference/liveness/
lease checks and basename-only `unlinkat`/directory fsync. Kernel flags forbid
directory removal. No filesystem I/O or lock waiting occurs inside a SQLite writer.
A cooperative retry must obtain a shared gate and create a new temporary name;
queueing a retry cannot reattach a protected old temp inode while exclusion is held.
Unsupported hosts fail without DB/filesystem mutation or a weaker fallback.

One **job scope**, not one file, is a cumulative work unit. A scope with confirmed
unlinks and no error succeeds; a no-op/retained scope skips; errors or enumeration
limits fail. `total_known` reserves that attempt's budget once, even when it is
empty. Safe progress/change/outbox/audit and final completion use the existing
queue transactions. Filesystem unlink cannot roll back after a later SQL/cancel/
lease failure: it is not recorded as successful work without its final live
transaction. Interrupted unfinished units become cancelled; a retry receives a
fresh budget and skips absent files rather than inventing successful removals.
This is not an atomic filesystem/DB transaction or a promise to cancel an in-flight
kernel call at the exact expiry instant.

### Bounded private immutable-cohort reconciliation

The separate internal Linux-only `reconcile_artifact` protocol examines **one known
media-owned revision and finite static preset version 1**. It has no public enqueue,
CLI, application consumer or schedule. It is not recursive cache discovery,
deleted-revision garbage collection, source/index purge, rendering or publication.
The temporary-file operation cannot select this family or expand into final files.

Its canonical private identity binds the cache, media/source/revision IDs, immutable
revision version/source SHA/size/mtime, preset and explicit entry/read-byte limits.
The exact live job must retain its original cache, media and revision dependencies,
including after heartbeat, progress and audit writes. Missing indexed ownership,
changed pins or missing canonical terminal static-job evidence retain the whole
cohort without traversing it. Historical producer evidence must bind that exact
media and indexed source. Queued/running matching work, including expired or
cancel-requested work and unknown kinds bound to the same key, prevents removal.
Active source warnings also stop maintenance; no source filesystem is probed.

Exclusive cache exclusion spans those fresh decisions and all filesystem actions.
Only the explicit private same-device `variants/{revision}/{preset}/1` directory
chain is opened. A candidate must be an old (at least 24 hours), regular mode-0600,
single-link same-UID/device WebP with the canonical lowercase SHA basename, full
matching content SHA and finite static-preset container/geometry/size constraints.
These checks **do not decode pixels or prove source-output rendering provenance**
and never grant READY. Unknown/malformed/metadata-bearing output, other cohorts,
changed names/inodes, links, unsafe directories, newer/future/oversized entries and
**every DB variant reference**, regardless of status/current revision, are retained.

The request bounds enumeration to 1..1000 entries plus one lookahead and total file
reads to 1 byte..64 MiB, still subject to each preset's smaller file cap. A limit
fails the cohort scope rather than claiming a complete sweep. Basename-only
`unlinkat` refuses directory removal; each unlink is followed by directory fsync.
Shared directories and the permanent gate remain. No filesystem operation or lock
wait occurs inside a SQLite writer; unsupported hosts have no fallback.

One explicit cohort is one cumulative work unit, including a skipped empty or
unproven cohort. Exactly the two compiled maintenance families share the existing
queue/lease/budget/journal mechanics, without another queue, ledger or schema.
Physical removals cannot roll back after a later SQL/cancel/lease failure. Only
confirmed scope progress is counted; interruption cancels unfinished units, retries
reserve a new budget and absent bytes never become invented successful removals.
Full discovery, future download-pin integration and enabled administrative cleanup
remain separate requirements; this private protocol does not finish those features.

### Private revision-bound source preparation

A Linux amd64/arm64-only internal source capsule can prepare an existing indexed
image for a future static producer. This is **not a renderer, decoded-pixel proof,
READY publisher, download grant or enabled worker operation**. No public endpoint
or application consumer for the private `variant_still` work identity is registered.

Preparation requires the exact live media/revision/finite-preset job identity and
its original creation-bound media/source dependencies. Library/source activity,
source health/configuration/root pin, present entry, current media-owned revision,
entry/revision size/mtime/full SHA and positive resource versions must agree. A
successful reachability probe alone cannot bless stale indexed inventory. Invalid
metadata, unknown presets or input-byte budgets outside the explicit worker bound
are rejected. Work remains bounded by the input limit, streaming memory and context;
the internal upper deadline is 15 minutes, not a new user configuration or capability.

All root/marker/file opening and full input SHA streaming occur **outside** SQLite
writer transactions. Canonical rooted read-only descriptors reject missing roots,
markers, root aliases, escaping paths and non-regular files without creating them.
Current named inode, file size/mtime/ctime/mode and marker/root identity are checked
around the hash. Same-stat overwritten bytes still require the indexed full SHA;
mtime or a file extension is not proof. A fresh transaction rejects resource, epoch
or lease changes that occurred during I/O. The descriptor is rewound for a future
producer and remains non-writable regardless of source permissions.

The same capsule can rehash/revalidate after producer work, then supply DB-only
identity/lease checks inside a future publication transaction. It never writes
business rows, progress, audits, variants or source bytes. Unsupported platforms
fail closed without a weaker fallback. Closing the capsule invalidates reuse;
private paths, hashes and lease owners have no serialized/public representation.
These checks do not provide an atomic NAS snapshot or interrupt arbitrary kernel
filesystem stalls. A real verified isolated decoder/renderer and atomic READY/
outbox/progress-last publication/orphan recovery are still required; metadata and
this read-only capsule cannot substitute for those missing capabilities.

### Orientation and capture time

Media detail and album-media pages return these fields, including explicit `null`
when evidence is absent:

- `codec` is a bounded native image or primary video codec identifier such as
  `jpeg`, `png`, `h264`, `vp8` or `ffv1`. It is null for old uninspected rows and
  failed/unsupported metadata. A filename, MIME hint, raw tag or diagnostic is
  never used as a codec value. Codec availability is not browser playback readiness.
- `width` and `height` are the source's encoded pixel dimensions. EXIF orientation
  does not swap them or rewrite the original.
- `orientation` is a validated EXIF integer `1..8` or equivalent ordered AVIF/video
  transform; null means unknown,
  not an inferred value of 1. Image viewers must respect it unless their decoder
  already does. Video uses equivalent validated orthogonal display transforms as
  described below; browser video decoders may already apply that transform. Never
  transform twice. Generated orientation-normalized variants are still pending.
- `capture_at` is a UTC RFC3339 instant derived only from validated JPEG/PNG/WebP/AVIF/HEIF/TIFF
  `DateTimeOriginal` plus an explicit `OffsetTimeOriginal`. Missing original time,
  absent offset, or the unknown offset `-00:00` leaves it null. The server never
  substitutes file mtime, modification/digitization time, or its own timezone.
  Optional `SubSecTimeOriginal` accepts bounded ASCII digits in inline/offset TIFF
  values. Extra precision is truncated, not rounded, to milliseconds before immutable
  staging and API/database publication. Missing/blank subseconds retain second
  precision; a fraction never supplies a missing date or timezone. Empty or trailing
  ASCII-space padding is allowed; non-digits, duplicate tags and unsafe counts/offsets
  are rejected. The subsecond ASCII value is capped at 64 bytes including its NUL.

Only bounded JPEG APP1/PNG eXIf/WebP EXIF/primary-associated AVIF/HEIF Exif profiles and
classic TIFF's first-page orientation/original-time whitelist are read. PNG eXIf and WebP EXIF are capped at 64 KiB; PNG eXIf
must occur once before IDAT. WebP accepts a raw TIFF profile or its Exif prefix.
Ambiguous duplicate EXIF/whitelisted fields, malformed required values,
and header/entry resource limits produce explicit invalid-media work without trusted
metadata. GPS, MakerNote, embedded thumbnails, raw EXIF maps, metadata paths/URLs,
and unrelated directories are not followed or exposed. This is not full EXIF
support: other image containers remain pending. Video uses a separate bounded
whitelist below. No raw EXIF, XMP or color-profile map is exposed.

The inspector version/resource policy and post-inspection full source hash bind
metadata to immutable staging. Publication retains logical identity on a valid
rescan, creates a new owned revision for a changed source signature, and clears
old orientation/capture values when replacement bytes contain no such evidence.
All metadata remains subject to the same fresh session and shared SQL visibility;
orientation/capture fields do not grant original-download permission.

### Image animation metadata

Media detail and album-media pages also include these explicitly nullable fields:

- `animated`: true when an inspected PNG/APNG, GIF, WebP or AVIF container has multiple
  image-animation frames; false for proven one-frame images. Null means unknown, not static.
  APNG's separate default image is not counted unless its frame control includes it.
- `frame_count`: the proven image frame/sample count, or 1 for a proven still image.
  It is not an estimate from file size, a decoded-pixel count, or proof that every
  compressed frame decodes.
- `loop_count`: **total playthroughs**, not repeats after the first play. Zero means
  infinite, 1 means once, and a finite GIF repeat count of 2 becomes 3. APNG
  `num_plays` and WebP loop counts are already totals and are not incremented. Missing GIF loop extensions
  mean one playthrough; uninspected formats remain null. AVIF whole-edit counts are
  normalized as total plays without adding a GIF-style initial play; see below.
- `duration_ms`: one cycle's sum of positive declared frame delays, never multiplied
  by `loop_count`. APNG uses exact rational delays rounded upward once for the complete
  cycle; a zero denominator means 100. Zero numerators/delays, missing GIF delays, or
  GIF user-input timing leave duration null. No browser minimum delay or guessed
  frames-per-second value is substituted.

The GIF inspector streams the complete bounded container without LZW pixel decoding
or frame-pixel allocation. It checks frame/canvas bounds, palettes, block structure,
control/loop ambiguity and the final trailer. Comments and unrelated application
payloads are skipped privately. Plain Text and unknown rendering extensions return
`CODEC_UNAVAILABLE`, not invented frame/timing evidence. Malformed structures clear
trusted metadata and count as `INVALID_MEDIA`; input/frame/declared-duration limits
produce `MEDIA_LIMIT`. PNG/APNG inspection likewise streams the complete bounded
file, validates every chunk CRC, critical structure/palette/IDAT ordering and shared
`fcTL`/`fdAT` sequence numbers, frame canvas/control/count consistency and IEND.
A separate default image and zero-length data sub-chunks are accounted for. Unknown
critical chunks are unsupported; ancillary payloads remain private. This validates
container metadata, not zlib/LZW pixels. See [worker policy bounds](workers.md).

WebP inspection checks the exact bounded RIFF extent and zero padding, VP8/VP8L
key-frame dimensions, VP8X canvas bounds, ANIM total plays and ANMF frame rectangles,
bitstream/alpha ordering and dimension agreement. Unknown chunks remain private;
reserved/future VP8X and ANMF fields are ignored as required by WebP. Millisecond
frame durations sum for one cycle; any zero duration remains unknown. These checks
do not decode entropy-coded pixels or perform color-profile conversion.

Source hash/revision, inspector version and policy bind this evidence to publication.
Unchanged rescans retain it; format changes replace stale animation/timing/EXIF with
fresh evidence. Full pixel validation, generated animation/static fallbacks and
playback variants remain separate implementation/acceptance work. These fields do
not grant preview/original access or establish player/offline readiness.

## Controlled HEIF/HEIC still metadata

HEVC-coded HEIF/HEIC stills use the same bounded local item/extent/property/EXIF
reader as AVIF, plus the mandatory fixed `heifload` header sandbox. The `pitm`
primary image supplies encoded dimensions, not the first top-level image or a
thumbnail. Compatible direct `hvc1` items and uniform direct-tile grids are supported;
primary collections do not become animation timelines. The codec is `hevc` and
original-download MIME is the generic `image/heif` family.

- Local `hvcC` records are capped at 64 KiB, 16 arrays and 64 NAL units per array.
  Framing must include VPS/SPS/PPS with matching NAL headers. Opaque SEI payloads
  are never projected as metadata. This validates framing, not full SPS/RBSP semantics.
- Each coded item uses one local extent and four-byte NAL lengths, with at most
  4096 NAL units. Exactly one independently coded IRAP picture is established;
  additional slices are not counted as additional frames. Compressed bodies are
  skipped, not copied or decoded in Go. In-band parameter-set overrides, dependent/
  multilayer pictures and multiple pictures in one still item remain unsupported.
- Grid canvas, tile count/configuration/geometry and full coverage use the same
  bounds as AVIF. Ordered `irot`/`imir` properties precede the whitelisted EXIF
  orientation; original time still requires an explicit offset and normalizes to
  UTC milliseconds. HEIF EXIF orientation can be informational: avoid applying it
  twice when a decoder already handles item transforms.
- The isolated summary must agree with native encoded geometry, allowing only a
  proven display quarter-turn swap. Missing tools/isolation never bless native
  headers. Proven stills return `animated:false`, one frame, one play and null
  duration; sequence/movie brands and unsupported profiles do not get those defaults.

HEIF sequences, other primary codecs, protected/external references, recursive or
incompatible grids and unknown essential properties remain explicit unsupported
work. These limits do not promise full HEVC pixel/color/profile or browser support.
Originals remain attachment-only behind the extra download grant; HEIF is not added
to inline READY MIME types. Source/runtime/configuration/revision/lease and full
post-inspection hash fences are unchanged. Owned direct/grid/collection/transform/
EXIF vectors are independently decoded during generation, not by an unrestricted
runtime fallback.

## Controlled classic TIFF metadata

Classic little- and big-endian TIFF uses a bounded native directory reader plus the
mandatory fixed libvips header sandbox. The first full-resolution page supplies
encoded dimensions and the same orientation/original-date/explicit-offset/subsecond
whitelist described above. Its codec is `tiff`. The isolated `tiffload` summary must
agree with the native geometry; missing isolation or tools cannot turn native
headers into successful metadata. No pixels are allocated in the Go reader.

- Root dimensions and whitelisted fields reject duplicates, invalid types/counts,
  unsafe offsets and ambiguous EXIF. Only the primary page's EXIF is considered.
  Missing timezone stays null even if a later page supplies one.
- Declared strip or tile offset/length vectors must pair, remain bounded, and point
  to nonempty local source extents. The reader skips compressed pixels and private
  tag payloads. It does not prove their complete decompression or color correctness.
- Each directory has at most 512 entries. The next-page chain rejects cycles and
  is capped at the smaller of `inspection.max_frames` and 4096 directories. Each
  strip/tile vector has at most 1,048,576 values and also shares the metadata byte
  budget. `inspection.header_bytes` counts actual native bytes read, not the largest
  offset: valid scattered fields do not require copying the whole source prefix.
- TIFF document pages are **not** image animation. `animated`, `frame_count`,
  `loop_count` and `duration_ms` remain null, including for a multipage document.
  The first page is not replaced by a thumbnail, concatenated page stack or inferred
  animation timeline.
- BigTIFF, DNG/recognized camera-RAW profiles, reduced/mask-first images, SubIFD
  primary selection and unknown compression/photometric profiles remain explicitly
  unsupported. They do not establish deployment support for arbitrary TIFF or RAW.
  Corrupt directories/extents are invalid media; byte/page/pixel/entry limits fail
  with `MEDIA_LIMIT`, never partially trusted fields.

The original still requires its extra download grant and is served as an
`image/tiff` attachment. This does not add TIFF to inline READY variant MIME types,
produce a preview, or establish full TIFF decoder/browser support. Owned uncompressed,
Deflate, tiled, strip-vector, endian and multipage vectors exercise publication and
format rewrites without changing source bytes. Source/runtime/configuration/lease
pins and the post-inspection full hash remain mandatory.

## Controlled AVIF metadata

AVIF uses a bounded ISO-BMFF reader plus the [mandatory codec sandbox](codec-sandbox.md).
Directly coded primary items/sequences cross-check AV1 and encoded geometry through
FFprobe. Generic-major-brand and tiled-grid stills use a finite fixed libvips header
profile; their primary/AV1 tile declarations and native geometry are validated
before comparing the exact one-line header. A tool name, extension or brand alone
cannot establish success. Missing isolation/tool capability remains explicit
`CODEC_UNAVAILABLE`, never a header-only fallback.

- The primary item is selected through `pitm`, `iinf`/`infe`, `iloc` and `ipma`/`ipco`,
  not whichever thumbnail or metadata item appears first. Local file/`idat` extents
  must remain within their declared containers. External/protected references are
  unsupported and never followed.
- The primary item's `ispe` dimensions and AV1 configuration must agree with the
  controlled probe. `irot` and `imir` properties compose in association order and
  normalize to EXIF `1..8`; explicitly signaled item transforms precede EXIF fallback.
  Encoded width/height are not swapped and source pixels are not rewritten.
- Grid primary items require a matching bounded canvas descriptor, exact tile count,
  direct compatible AV1 inputs and complete canvas coverage. Tile size/configuration
  must agree; recursive derived items and per-tile transforms are unsupported.
  A grid remains one image/frame, not one frame per tile. The protected original is
  unchanged; the API keeps native encoded canvas dimensions even if the header
  decoder applies a declared quarter-turn before reporting display dimensions.
- Only a primary-associated `Exif` item is considered. Its bounded offset/profile
  uses the same classic-TIFF whitelist for original date/explicit timezone/subseconds.
  Capture is truncated to UTC milliseconds; GPS, raw fields, MakerNote, thumbnails,
  metadata URLs and unrelated-image EXIF are not exposed. Ambiguous EXIF associations
  or malformed values cannot silently replace trusted evidence.
- A proven still-only container has one frame and one play, with null duration.
  Sequence counts/timing come from consistent sample-size/timing/chunk tables with
  nonempty local extents, never guessed FPS or primary-item presence. Positive exact
  cycle timing rounds upward once to milliseconds; zero/missing timing remains null.
- Finite repetition requires the edit-list repeat setting and a whole number of
  declared edits in the track playback span, allowing at most one movie-clock tick
  of edit quantization. The normalized value is total plays, not additional repeats.
  Proven indefinite spans mean zero; missing/unproven/partial repetition stays null.
  A known non-repeating edit means one. Loop counts never multiply `duration_ms`.

The byte budget limits metadata reads, not the size of a copied `mdat`: compressed
payloads are skipped/extent-checked and later source hashing is streamed. Independent
caps include 4,096 boxes, 256 items, 16 extents per item, 64 associations per item,
64 tracks, configured dimensions/pixels/frames/cycle duration and a 64 KiB EXIF item.
Protected/external data, recursive or incompatible grid inputs, ambiguous color
tracks, unknown essential properties and generic-major image sequences still
remain unsupported rather than guessed. Other image formats and complete codec/
variant acceptance remain required; no unrestricted fallback is enabled.

Inspector v9 pins the two tool runtimes independently and rejects historical
v1..v8 policy pins. Source hash/configuration and
live publication fences are unchanged. Owned still/finite/infinite/EXIF/transform
fixtures exercise actual probing, immutable SQLite publication, source rewrites and
compiled consumers. This is metadata evidence, not validation of every compressed
AV1 frame, browser animation, packaged container codec or physical NAS/device.

## Controlled video metadata

MP4, MOV, Matroska/MKV and WebM use a mandatory [Linux FFprobe sandbox](codec-sandbox.md).
Container magic and bounded EBML/ISO-BMFF evidence select MIME, not the extension.
Classic MOV padding/movie atoms are recognized within the bounded sniff prefix;
FFprobe must still prove the expected container family and a real video stream.
The fixed `V:0` selection means the first non-attached-picture video stream.
Embedded album covers do not become video, and audio/subtitle/secondary-video
metadata is not silently substituted. This is metadata analysis, not full decoding.

- Encoded width/height and primary `codec` are validated before staging. Positive
  dimensions must satisfy configured dimension/pixel limits. No source rewrite or
  width/height swap is performed for rotation.
- `duration_ms` uses the selected track's positive declared seconds. Missing, zero
  or `N/A` track timing may use positive container duration. Both declarations must
  satisfy the duration budget. Decimal seconds are handled as exact rationals and
  rounded upward once to milliseconds; no frame-count/rate estimate is invented.
  Unknown timing remains null. Video `animated`, `frame_count` and `loop_count`
  remain null: those API fields describe inspected image animations, not guesses
  about video frames or player looping preferences.
- Pure orthogonal fixed-point display matrices map to the equivalent EXIF `1..8`
  orientation, including mirror transforms. A matrix is authoritative over legacy
  rotate tags. Arbitrary scaling, shear, perspective or translation remains unknown
  rather than being misrepresented as a quarter turn. Without a matrix, a bounded
  explicit legacy rotate tag may establish a quarter-turn orientation.
- `com.apple.quicktime.creationdate` is preferred over generic `creation_time`
  evidence. Only whitelisted RFC3339 values with explicit `Z` or numeric timezone
  offsets yield `capture_at`, normalized/truncated to UTC milliseconds before
  staging. An unknown original timestamp is not replaced by a less-specific tag.
  Conflicting evidence, missing zones and `-00:00` stay null. Generic container
  creation time is a declared metadata fallback, not proof of an observed camera
  exposure. Filesystem times and the worker's timezone are never substituted.

The narrow JSON projection reuses the same duplicate-key, exact-field, UTF-8,
nesting, type and explicitly nullable-field validator as HTTP input. Unknown raw
maps, GPS, filenames, paths and diagnostics are rejected/not selected, never copied
into the DTO. Pipe/input/resource bounds, malformed/unknown output and missing
capabilities produce explicit failed media work. No unrestricted command, network
protocol, shell, private-file fallback or public original URL is enabled.

Inspector v7 pins resource policy **and** a private trusted-runtime installation
epoch. The child checks that epoch against the exact file descriptors granted to
Landlock. It includes canonical tool/loader/dependency/cache identities, sizes,
modes and modification/change times, not a content hash of untrusted media. Keep
installed system codecs immutable while workers run; stop/restart workers around
system tool/library upgrades. Replaced installations cannot resume an old pinned
generation. Native images still work when FFprobe is missing; affected video paths
are explicitly unsupported, not silently skipped or reported successful.

All runtime/source/probe work happens outside SQLite writers. A full source hash
and root-relative stat/configuration check follow inspection. The existing
source/config/generation/lease/cancel/version fences and atomic progress/audit/
change/outbox transaction still gate publication. Valid rescans retain logical IDs
and revisions; format rewrites replace stale codec/clock/transform evidence. This
adds no READY variants or original-download permission, and does not complete the
external image, player, container-packaging or source-processing phase gates.

## Read bytes

| Endpoint                                                   | Policy and behavior                                                                                                                                    |
| ---------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `GET/HEAD /media/{media_id}/variants/{variant_id}/content` | Variant must be READY, belong to this media's current revision, and satisfy current media visibility.                                                  |
| `GET/HEAD /media/{media_id}/original`                      | Additionally requires explicit original policy, healthy configured source, readable mount marker, present entry, and matching revision/stat signature. |

The server does not hold a SQLite writer transaction while opening or streaming a
file. It opens only root-confined read-only regular-file descriptors; directories,
FIFOs, escaping symlinks, traversal paths, mismatched roots, and stale source
signatures are rejected. Confined relative symlinks may be followed. Cache keys
are immutable revision/preset/version/SHA-256 keys; a DB row cannot redirect a
variant to another revision's storage key.

Originals always use `Content-Disposition: attachment`. Safe ready variants use
inline disposition and a restricted image/video MIME allowlist. Unknown original
MIME types become `application/octet-stream`; detected MOV/MKV types remain correct
for forced-attachment originals without expanding the inline READY-preset allowlist.
Active HTML/SVG is never served as an inline ready variant. Filenames are encoded safely. Responses include `nosniff`,
a sandbox CSP, `Vary: Cookie`, and `Cache-Control: private, no-store`.

Original files may contain EXIF/GPS or other private metadata. Grant original
policy only when the person is allowed to receive the original bytes; it is not a
promise to strip that metadata. A user permitted to view a preview can retain its
visible bytes; the download policy is not DRM.

## Single Range and validators

GET supports one `bytes` range:

```http
Range: bytes=0-1048575
If-Range: "sha256-<representation-hash>"
```

Closed (`0-99`), open-ended (`100-`), and nonzero suffix (`-100`) ranges are
supported. A valid selected range returns 206, correct MIME, `Content-Length`, and
`Content-Range: bytes start-end/total`. An end/suffix beyond the file length is
clamped normally.

Malformed, overflowing, empty, zero-suffix, multipart, or unsatisfiable ranges
return 416 Problem JSON with `Content-Range: bytes */total`, when Range applies.
The ETag and private/no-store headers survive errors; the body never exposes local
paths or Go's raw filesystem error text. HEAD ignores Range, returns full metadata
headers, and has no response body.

- Strong SHA-256 ETags identify published representation bytes, not authorization.
- An exact strong `If-Range` tag permits resuming with 206. A stale/weak tag or
  HTTP-date value causes a full 200 response. Use the ETag for resume.
- Last-Modified is deliberately omitted: sub-second source changes must not be
  mistaken for a strong HTTP-date validator.
- `If-None-Match` can return 304 only after current authorization and file checks.
- A failing content `If-Match` returns 412 Problem JSON, not a metadata mutation's
  409 version conflict. Conditional checks run before range parsing.

A stale If-Range can ignore even an otherwise rejected Range, returning the full
representation. A matching If-None-Match can return 304 before Range applies.
Clients must handle these standard conditional outcomes rather than assume every
Range request produces 206.

## Availability, cancellation, and deployment

A source outage returns `503 SOURCE_UNAVAILABLE` for originals while preserving
indexed metadata and authorized ready cached previews. Missing/inconsistent ready
cache files return `503 VARIANT_UNAVAILABLE`. Neither condition deletes source
files or reconciles away the index. Source configuration is a strict allowlist:
its name and canonical root must match the indexed source, its private path/marker
configuration pin must match the running process, and its root-relative mount
marker must be readable at request time. Original entries must also belong to the
current source configuration epoch; checking root reachability alone cannot bless
an older inventory after a configuration change. GalleryGo never creates source
roots or markers for you. Deactivating a source hides its metadata and content
from every account, including administrators; an outage alone preserves authorized
cached previews. Archived media are excluded by the shared visibility predicate,
including metadata, covers, counts, originals, and variants. Originals additionally
require both a present source entry and an available logical media object; a file
that becomes non-media cannot retain its former original-content eligibility.

Content streams do not inherit the 10-second metadata timeout. Writes have a
30-second refreshed idle deadline, and reads/seeks check cancellation and file
size/modification signatures. A detected in-flight change or broken write aborts
the connection/stream; it never appends JSON to partially delivered media or
silently accepts mixed revisions. A client must discard an incomplete body.

These signatures are not an adversarial tamper-proof NAS snapshot. Full source
hashes belong to ingestion/publication; they are not recomputed for each Range
request. Future offline downloads must verify complete length and SHA-256 before
marking bytes ready. Correct immutable publication and trusted cache storage remain
required.

`os.Root` confines entry traversal/TOCTOU, but does not forbid privileged bind
mounts, device files, or make stalled kernel NAS I/O cancellable. Use trusted
read-only NAS mounts, appropriate `nodev`/`nosuid`/`noexec` mount policy, separate
local SQLite/cache storage, and bounded filesystem timeouts where supported.
Physical NAS/mount behavior and real-device playback remain release verification
gates, not claims inferred from temporary local files or cross-compilation.

A reverse proxy must preserve Range/If-Range/ETag headers and private responses.
Do not add a CDN or global service-worker GET cache in front of these endpoints.
Offline originals are not enabled; no generic API caching rule may turn an online
original download into an unbounded offline copy managed by this product.
