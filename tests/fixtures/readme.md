# Synthetic media test vectors

`webp.json` contains base64-encoded, complete RIFF/WebP files made from repository-
owned 3-by-2 RGB frames. No personal photograph, external sample or source-media
mount is involved. Tests do not need an encoder at runtime.

- `lossy`: one VP8 frame, 3 by 2 pixels.
- `lossless`: one VP8L frame, 3 by 2 pixels.
- `animated`: two VP8L frames, 100 ms each, two total playthroughs. The first frame
  uses RGB `(32, 96, 144)` and the second `(240, 224, 192)`.

These tiny fixtures exercise real compressed image data, not just fabricated codec
headers. They do not establish full decoder, animation-player, NAS or device support.

## Owned AVIF vectors

`avif.json` contains complete AV1 image containers over the same owned 16-by-12
frames. System FFmpeg 6.1.1 `libaom-av1` generated still, two-play, infinite and
one-play sequences at 5 fps with one encoder thread. Each sequence has two real
frames and a 400 ms cycle. AVIF total plays do not add GIF's initial-play offset.

The EXIF/rotated/mirrored still variants rebuild only bounded item/property metadata
around those original coded pixels: a primary-associated synthetic classic-TIFF
profile carries orientation, original date/offset and `.123` subseconds. `irot=3`
and then `imir=1` prove association-order transform composition. No GPS or personal
photograph is present. Every vector was independently frame-decoded during
generation; system libvips also read the still EXIF/time metadata. That libvips
build did not recognize the valid sequence vectors, so tests do not equate a
single tool's file-recognition list with complete codec support. Runtime tests
need only the fixed isolated FFprobe and checked-in vectors, not an encoder.

The additional `grid` vector declares a 96-by-64 canvas from two distinct owned
64-by-64 AV1 tiles. Local derived references, tile configuration and canvas crop
are explicit. System libvips independently decoded all 18,432 RGB bytes and both
edge colors during generation. The original seven vectors are unchanged. Generic
HEIF-major tests change only the owned still's major brand, not private media.
The isolated image-header profile is tested separately from that trusted-input
fixture-generation decode; metadata success is not full AV1 pixel acceptance.

## Owned HEVC HEIF/HEIC vectors

`heif.json` contains seven compact primary-item stills built from owned 64-by-64
RGB colors `(32, 96, 144)` and `(240, 224, 192)`. System FFmpeg/libx265 encoded one
main-still-picture frame per color, with one frame thread, no thread pool/WPP and
explicit SDR color parameters. Standard-library container packing creates direct,
generic-major, 96-by-64 two-tile grid, EXIF, rotated, mirrored and non-first-primary
collection vectors. No personal photograph, network sample or runtime encoder is used.

The owned encoder repeats parameter sets despite its `repeat-headers=0` option.
The fixture packer verifies those bytes match the out-of-band configuration, removes
the repeats and encoder-only unregistered SEI, then independently decodes every
RGB pixel of every final file. That is fixture assembly, **not** production source
repair or a runtime fallback. The seven packed files total 3,892 bytes and decoded
RGB data totals 104,448 bytes; each pixel matches its expected owned-color region
within the generation tolerance. This is not HDR/ICC/color-calibration acceptance.

The synthetic primary EXIF has orientation 3 and an explicitly offset original
clock with `.123456` subseconds; container rotation followed by mirroring overrides
that orientation with 5 while keeping capture UTC milliseconds. Grid rotations
also prove swapped display geometry without rewriting encoded 96-by-64 dimensions.
The collection puts another image before the declared primary, so first-image
selection would choose the wrong pixels. Runtime tests use mandatory isolated
headers plus bounded native framing, not a full HEVC decoder or browser player.

## Owned classic TIFF vectors

`tiff.json` contains seven complete TIFFs built with standard-library byte packing
and zlib over owned RGB `(32, 96, 144)` pixels. The first page is 12 by 7 pixels;
the multipage vector adds a 3-by-2 page of `(240, 224, 192)` with different EXIF.
No external photograph, NAS mount, network fetch or runtime encoder is involved.

- `little_exif` and `big_exif` use classic TIFF's two byte orders and one RGB strip.
- `deflate` compresses that strip with standard zlib/Deflate.
- `tiled_deflate` uses a 16-by-16 Deflate tile cropped by the 12-by-7 canvas.
- `strips` uses three big-endian RGB strips with LONG offsets and SHORT byte counts.
- `multipage` proves first-page selection rather than stacked geometry or animation.
- `plain` omits orientation and capture fields; no default/host time is substituted.

Whitelisted synthetic EXIF declares orientation 6, an explicitly zoned original
date and `.123456` subseconds, normalized to `.123Z` after UTC conversion. A private
canary description must never appear in metadata DTOs. System libvips independently
decoded **every RGB byte of every page** during generation, including Deflate and
tile/strip layouts. Runtime tests use the fixed isolated header profile, not that
trusted-input generation decoder. Metadata success does not imply full TIFF/RAW,
all-page decoding, browser playback or READY variant acceptance.

## Owned video vectors

`video.json` contains tiny complete containers generated with system FFmpeg 6.1.1
from two repository-owned 16-by-12 RGB frames at 5 fps. The frames use the same
solid RGB colors listed above; no camera footage, audio recording, network sample
or private NAS media is involved. Runtime tests need FFprobe, not an encoder.

- `mp4`: H.264 in MP4; encoded dimensions 16 by 12, 400 ms, display matrix equivalent
  to EXIF orientation 6, whole-second declared UTC creation time.
- `mov`: H.264 in MOV with that transform/duration and an explicit-offset
  `com.apple.quicktime.creationdate` retaining UTC millisecond evidence.
- `legacy_mov`: the owned MOV with its optional `ftyp` atom changed to same-size
  padding, preserving offsets; the classic unbranded movie still decodes two frames.
- `matroska`: FFV1 in MKV, 400 ms and explicitly zoned fractional creation time.
- `webm`: VP8 in WebM with the same duration/time evidence.
- `multi`: FFV1 followed by VP8 tracks in MKV; the fixed non-attached first-video
  selection must choose FFV1, not combine track metadata.
- `attached_only`: synthetic silence with one PNG cover in MP4. No real video track
  exists; an attached picture must not fabricate video metadata.

Generation uses single-thread H.264 (`libx264`, ultrafast, CRF 28), FFV1 and VP8
(`libvpx`) encoders over the owned raw RGB frames. MOV/MP4 are remuxed with explicit
`-display_rotation:v:0 -90`; MOV uses `use_metadata_tags` for the capture-date tag.
The four primary containers and classic MOV were independently decoded to frame
MD5 output during generation, proving two real frames rather than header-only
vectors. They do not prove every codec profile/frame, browser player, container
packaging, NAS or physical-device support. Source bytes stay read-only in tests.
