# Protected media API

Media metadata and read-only content endpoints use the same SQL visibility rules as
[album and library reads](access.md). There is no public source/cache filesystem
mount, fallback credential, anonymous thumbnail URL, or original-download bypass.

**Current scope:** protected serving, native image inventory and controlled video
metadata publication are implemented. Native JPEG/PNG/GIF/WebP dimensions,
whitelisted image EXIF, GIF/APNG/WebP frame/cycle/loop metadata and sandboxed
MP4/MOV/MKV/WebM codec/geometry/duration/transform/creation evidence are available.
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

- `GET /albums/{album_id}/media` lists visible media in stable ID order. An empty
  authorized album returns an empty page; an unauthorized album returns 404.
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

### Orientation and capture time

Media detail and album-media pages return these fields, including explicit `null`
when evidence is absent:

- `codec` is a bounded native image or primary video codec identifier such as
  `jpeg`, `png`, `h264`, `vp8` or `ffv1`. It is null for old uninspected rows and
  failed/unsupported metadata. A filename, MIME hint, raw tag or diagnostic is
  never used as a codec value. Codec availability is not browser playback readiness.
- `width` and `height` are the source's encoded pixel dimensions. EXIF orientation
  does not swap them or rewrite the original.
- `orientation` is the JPEG/PNG/WebP EXIF orientation integer `1..8`; null means unknown,
  not an inferred value of 1. Image viewers must respect it unless their decoder
  already does. Video uses equivalent validated orthogonal display transforms as
  described below; browser video decoders may already apply that transform. Never
  transform twice. Generated orientation-normalized variants are still pending.
- `capture_at` is a UTC RFC3339 instant derived only from validated JPEG/PNG/WebP
  `DateTimeOriginal` plus an explicit `OffsetTimeOriginal`. Missing original time,
  absent offset, or the unknown offset `-00:00` leaves it null. The server never
  substitutes file mtime, modification/digitization time, or its own timezone.
  Optional `SubSecTimeOriginal` accepts bounded ASCII digits in inline/offset TIFF
  values. Extra precision is truncated, not rounded, to milliseconds before immutable
  staging and API/database publication. Missing/blank subseconds retain second
  precision; a fraction never supplies a missing date or timezone. Empty or trailing
  ASCII-space padding is allowed; non-digits, duplicate tags and unsafe counts/offsets
  are rejected. The subsecond ASCII value is capped at 64 bytes including its NUL.

Only the bounded JPEG APP1/PNG eXIf/WebP EXIF classic-TIFF orientation and original
time/offset fields are read. PNG eXIf and WebP EXIF are capped at 64 KiB; PNG eXIf
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

- `animated`: true when a fully inspected PNG/APNG, GIF or WebP container has multiple
  animation frames; false for one-frame PNG/GIF/WebP or JPEG. Null means unknown, not static.
  APNG's separate default image is not counted unless its frame control includes it.
- `frame_count`: the proven image-animation frame count, or 1 for static PNG/JPEG/WebP.
  It is not an estimate from file size, a decoded-pixel count, or proof that every
  compressed frame decodes.
- `loop_count`: **total playthroughs**, not repeats after the first play. Zero means
  infinite, 1 means once, and a finite GIF repeat count of 2 becomes 3. APNG
  `num_plays` and WebP loop counts are already totals and are not incremented. Missing GIF loop extensions
  mean one playthrough; uninspected formats remain null.
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
