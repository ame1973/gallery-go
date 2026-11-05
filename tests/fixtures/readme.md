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
