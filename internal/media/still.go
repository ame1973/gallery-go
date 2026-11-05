package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
)

var errStillArtifact = errors.New("invalid static WebP preset artifact")

type stillPreset struct {
	key                         string
	version, longEdge, maxBytes int64
}

func staticPreset(key string) (stillPreset, error) {
	preset := stillPreset{key: key, version: 1}
	switch key {
	case "thumb_320":
		preset.longEdge, preset.maxBytes = 320, 1<<20
	case "thumb_720":
		preset.longEdge, preset.maxBytes = 720, 4<<20
	case "display_1920":
		preset.longEdge, preset.maxBytes = 1920, 8<<20
	default:
		return stillPreset{}, errStillArtifact
	}
	return preset, nil
}

// dimensions applies only the proven source orientation and a bounded no-upscale
// fit. Round once to the nearest positive pixel; quarter turns swap the axes.
func (preset stillPreset) dimensions(source Inspection) (int64, int64, error) {
	if source.Status != "ok" || source.Kind != "image" || source.Width == nil || source.Height == nil {
		return 0, 0, errStillArtifact
	}
	width, height := *source.Width, *source.Height
	if preset.longEdge < 1 || width < 1 || height < 1 || width > 1000000 || height > 1000000 || width > 1000000000/height {
		return 0, 0, errStillArtifact
	}
	if source.Orientation != nil {
		if *source.Orientation < 1 || *source.Orientation > 8 {
			return 0, 0, errStillArtifact
		}
		if *source.Orientation >= 5 {
			width, height = height, width
		}
	}
	longest := max(width, height)
	if longest > preset.longEdge {
		width = max(1, (width*preset.longEdge+longest/2)/longest)
		height = max(1, (height*preset.longEdge+longest/2)/longest)
	}
	return width, height, nil
}

// This private value is validation evidence, never a public DTO or publication
// capability. A producer must still decode successfully, rehash/fence the source,
// fsync/rename private output and co-commit READY with its live lease and outbox.
type stillArtifact struct {
	revision, preset, storage, mime, hash string
	presetVersion, size, width, height    int64
}

// validateStillArtifact validates the complete bounded container, not entropy-coded
// pixels. Reject metadata rather than copying it into a derived media response.
// The original input inspector deliberately remains more permissive about chunks.
func validateStillArtifact(ctx context.Context, revision, key string, source Inspection, data []byte) (stillArtifact, error) {
	if err := ctx.Err(); err != nil {
		return stillArtifact{}, err
	}
	preset, err := staticPreset(key)
	if err != nil || len(revision) > 128 || strings.ContainsAny(revision, "\x00\r\n") || len(data) < 12 || int64(len(data)) > preset.maxBytes {
		return stillArtifact{}, errStillArtifact
	}
	width, height, err := preset.dimensions(source)
	if err != nil {
		return stillArtifact{}, err
	}
	policy := DefaultInspectPolicy()
	policy.MaxDimension, policy.MaxPixels = int(preset.longEdge), preset.longEdge*preset.longEdge
	policy.HeaderBytes, policy.MaxFrames = preset.maxBytes, 1
	info, err := inspectWebP(ctx, bytes.NewReader(data), int64(len(data)), policy)
	if ctx.Err() != nil {
		return stillArtifact{}, ctx.Err()
	}
	if err != nil || info.width != width || info.height != height || info.frames != 1 || info.loops != 1 || info.duration != nil || info.orientation != nil || info.capture != nil {
		return stillArtifact{}, errStillArtifact
	}
	// inspectWebP has already checked every extent/padding byte. The finite output
	// vocabulary additionally excludes ICC/EXIF/XMP, animation and unknown payloads.
	for offset := 12; offset < len(data); {
		length := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		switch string(data[offset : offset+4]) {
		case "VP8X":
			if length != 10 || data[offset+8]&^byte(16) != 0 || !bytes.Equal(data[offset+9:offset+12], []byte{0, 0, 0}) {
				return stillArtifact{}, errStillArtifact
			}
		case "ALPH", "VP8 ", "VP8L":
		default:
			return stillArtifact{}, errStillArtifact
		}
		offset += 8 + length + length%2
	}
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	storage, err := VariantPath(revision, preset.key, preset.version, hash, "image/webp")
	if err != nil {
		return stillArtifact{}, errStillArtifact
	}
	if err := ctx.Err(); err != nil {
		return stillArtifact{}, err
	}
	return stillArtifact{revision, preset.key, storage, "image/webp", hash, preset.version, int64(len(data)), width, height}, nil
}
