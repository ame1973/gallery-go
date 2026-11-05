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
