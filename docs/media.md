# Protected media API

Media metadata and read-only content endpoints use the same SQL visibility rules as
[album and library reads](access.md). There is no public source/cache filesystem
mount, fallback credential, anonymous thumbnail URL, or original-download bypass.

**Current scope:** protected serving and native read-only inventory publication are
implemented, including bounded JPEG/PNG/GIF dimensions, whitelisted JPEG EXIF, and
GIF frame/cycle/loop metadata.
External codec processing, normal variant publication, gallery/player screens, and
offline manifests are still under development. A fresh installation has no indexed
media until a configured source is scanned; test fixtures are not production seed
data. The container does not yet include external media codecs. Native metadata
and original downloads do not establish complete image/video decoding or playback.

All paths below are relative to `/api/v2` and require a valid HTTPS session cookie.
Original download also requires the actual account's `download_original:true`,
**including for administrators**. Lack of resource visibility or download policy
is hidden as 404. Anonymous, expired, disabled, and revoked sessions receive 401.
The server rechecks these conditions before GET, HEAD, Range, or a conditional 304.

## Metadata and variants

- `GET /albums/{album_id}/media` lists visible media in stable ID order. An empty
  authorized album returns an empty page; an unauthorized album returns 404.
- `GET /media/{media_id}` returns ID, album ID, kind, display basename, nullable
  dimensions/duration/animation/orientation/capture time/current revision, and offline
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

- `width` and `height` are the source's encoded pixel dimensions. EXIF orientation
  does not swap them or rewrite the original.
- `orientation` is the JPEG EXIF orientation integer `1..8`; null means unknown,
  not an inferred value of 1. Display clients must apply the corresponding transform
  when they render an original. Generated, orientation-normalized variants are pending.
- `capture_at` is a UTC RFC3339 instant derived only from validated JPEG
  `DateTimeOriginal` plus an explicit `OffsetTimeOriginal`. Missing original time,
  absent offset, or the unknown offset `-00:00` leaves it null. The server never
  substitutes file mtime, modification/digitization time, or its own timezone.

Only the bounded JPEG APP1/classic-TIFF orientation and original time/offset fields
are read. Ambiguous duplicate EXIF/whitelisted fields, malformed required values,
and header/entry resource limits produce explicit invalid-media work without trusted
metadata. GPS, MakerNote, embedded thumbnails, raw EXIF maps, metadata paths/URLs,
and unrelated directories are not followed or exposed. This is not full EXIF
support: PNG EXIF, subseconds, other image containers, and video capture metadata
remain pending. PNG/APNG and animated WebP timing are not yet implemented.

The inspector version/resource policy and post-inspection full source hash bind
metadata to immutable staging. Publication retains logical identity on a valid
rescan, creates a new owned revision for a changed source signature, and clears
old orientation/capture values when replacement bytes contain no such evidence.
All metadata remains subject to the same fresh session and shared SQL visibility;
orientation/capture fields do not grant original-download permission.

### Image animation metadata

Media detail and album-media pages also include these explicitly nullable fields:

- `animated`: true when a fully inspected GIF container has multiple image frames;
  false for a one-frame GIF or JPEG. Null means unknown, not static, including
  current PNG/APNG inspection.
- `frame_count`: the GIF image-frame count, or 1 for JPEG. It is not an estimate
  from file size, a decoded-pixel count, or proof that every compressed frame decodes.
- `loop_count`: **total playthroughs**, not repeats after the first play. Zero means
  infinite, 1 means once, and a finite GIF repeat count of 2 becomes 3. Missing GIF
  loop extensions mean one playthrough; uninspected formats remain null.
- `duration_ms`: for GIF, one cycle's sum of positive declared frame delays. It is
  not multiplied by `loop_count`. If any image frame lacks a positive delay or
  requests user input, the duration is null. Zero/missing delays are never assigned
  browser-specific minimum delays or a guessed frames-per-second value.

The GIF inspector streams the complete bounded container without LZW pixel decoding
or frame-pixel allocation. It checks frame/canvas bounds, palettes, block structure,
control/loop ambiguity and the final trailer. Comments and unrelated application
payloads are skipped privately. Plain Text and unknown rendering extensions return
`CODEC_UNAVAILABLE`, not invented frame/timing evidence. Malformed structures clear
trusted metadata and count as `INVALID_MEDIA`; input/frame/declared-duration limits
produce `MEDIA_LIMIT`. See [worker policy bounds](workers.md).

Source hash/revision, inspector version and policy bind this evidence to publication.
Unchanged rescans retain it; format changes clear stale animation/timing fields.
APNG/WebP timing, full pixel validation, generated animation/static fallbacks and
playback variants remain separate implementation/acceptance work. These fields do
not grant preview/original access or establish player/offline readiness.

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
MIME types become `application/octet-stream`; active HTML/SVG is not served as an
inline ready variant. Filenames are encoded safely. Responses include `nosniff`,
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
