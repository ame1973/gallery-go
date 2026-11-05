package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func stillSource(width, height int64, orientation *int64) Inspection {
	return Inspection{Kind: "image", Status: "ok", Width: &width, Height: &height, Orientation: orientation}
}

func TestStaticPresetDimensionsAreFiniteOrientedAndNeverEnlarged(t *testing.T) {
	for key, edge := range map[string]int64{"thumb_320": 320, "thumb_720": 720, "display_1920": 1920} {
		preset, err := staticPreset(key)
		if err != nil || preset.key != key || preset.version != 1 || preset.longEdge != edge || preset.maxBytes < 1 || preset.maxBytes > 8<<20 {
			t.Fatal("incorrect fixed preset", preset, err)
		}
		for orientation := int64(1); orientation <= 8; orientation++ {
			for _, source := range []struct{ width, height, wantWidth, wantHeight int64 }{
				{3, 2, 3, 2},
				{edge, edge, edge, edge},
				{edge * 2, edge, edge, edge / 2},
				{edge, edge * 2, edge / 2, edge},
				{edge * 3, 1, edge, 1},
				{1, edge * 3, 1, edge},
				{edge * 3, edge * 2, edge, (edge*2 + 1) / 3},
				{1000000, 1000, edge, max(1, (edge+500)/1000)},
			} {
				width, height, err := preset.dimensions(stillSource(source.width, source.height, &orientation))
				wantWidth, wantHeight := source.wantWidth, source.wantHeight
				if orientation >= 5 {
					wantWidth, wantHeight = wantHeight, wantWidth
				}
				if err != nil || width != wantWidth || height != wantHeight || max(width, height) > edge {
					t.Fatal("preset geometry did not fit normalized source", key, orientation, source, width, height, err)
				}
			}
		}
		width, height, err := preset.dimensions(stillSource(3, 2, nil))
		if err != nil || width != 3 || height != 2 {
			t.Fatal("unknown orientation did not preserve encoded axes", width, height, err)
		}
	}
	for _, key := range []string{"", "poster", "thumb_321", "THUMB_320", "thumb_320/1", "thumb_320\x00", "display_1920[Q=1]"} {
		if preset, err := staticPreset(key); !errors.Is(err, errStillArtifact) || preset != (stillPreset{}) {
			t.Fatal("uncontracted recipe accepted", key, preset, err)
		}
	}
}

func TestStaticArtifactsBindOwnedWebPBytesToImmutablePresetKeys(t *testing.T) {
	for _, key := range []string{"thumb_320", "thumb_720", "display_1920"} {
		for _, format := range []string{"lossy", "lossless"} {
			data := webpFixture(t, format)
			before := bytes.Clone(data)
			digest := sha256.Sum256(data)
			hash := hex.EncodeToString(digest[:])
			for orientation := int64(1); orientation <= 8; orientation++ {
				width, height := int64(3), int64(2)
				if orientation >= 5 {
					width, height = height, width
				}
				result, err := validateStillArtifact(t.Context(), "revision-a", key, stillSource(width, height, &orientation), data)
				wantPath := "variants/revision-a/" + key + "/1/" + hash + ".webp"
				if err != nil || result != (stillArtifact{"revision-a", key, wantPath, "image/webp", hash, 1, int64(len(data)), 3, 2}) || !bytes.Equal(before, data) {
					t.Fatal("static artifact lost identity/geometry or mutated bytes", key, format, orientation, result, err)
				}
				replay, err := validateStillArtifact(t.Context(), "revision-a", key, stillSource(width, height, &orientation), data)
				if err != nil || replay != result {
					t.Fatal("artifact validation is not deterministic", replay, result, err)
				}
			}
		}
	}
	data := encodeWebP(extendedWebP(t, "lossy", 0))
	if _, err := validateStillArtifact(t.Context(), "revision-a", "thumb_320", stillSource(3, 2, nil), data); err != nil {
		t.Fatal("canonical static extended WebP was rejected", err)
	}
	other, err := validateStillArtifact(t.Context(), "revision-b", "thumb_320", stillSource(3, 2, nil), data)
	if err != nil || other.revision != "revision-b" || !strings.HasPrefix(other.storage, "variants/revision-b/") {
		t.Fatal("identical output bytes were not separately revision-bound", other, err)
	}
}

func TestStaticArtifactChecksCannotBeReplacedBySourceMetadataInspection(t *testing.T) {
	plain := webpFixture(t, "lossless")
	for _, chunk := range []testWebPChunk{
		{"EXIF", exifBytes(binary.LittleEndian, 1, "2026:11:05 12:34:56\x00", "+08:00\x00")},
		{"XMP ", []byte("private-source-path-and-GPS")},
		{"ICCP", []byte("private-profile")},
		{"JUNK", []byte("private-source-marker")},
		{"ANIM", []byte{0}}, // Ignored by the input reader when animation is disabled.
		{"ANMF", []byte{0}},
	} {
		chunks := extendedWebP(t, "lossless", 0)
		if chunk.kind == "ICCP" {
			chunks = append(chunks[:1], append([]testWebPChunk{chunk}, chunks[1:]...)...)
		} else {
			chunks = append(chunks, chunk)
		}
		data := encodeWebP(chunks)
		if _, err := inspectWebP(t.Context(), bytes.NewReader(data), int64(len(data)), DefaultInspectPolicy()); err != nil {
			t.Fatal("negative output must remain valid input metadata", chunk.kind, err)
		}
		if result, err := validateStillArtifact(t.Context(), "revision-a", "thumb_320", stillSource(3, 2, nil), data); !errors.Is(err, errStillArtifact) || result != (stillArtifact{}) || strings.Contains(err.Error(), "private-") {
			t.Fatal("private/animation/unknown output payload accepted or leaked", chunk.kind, result, err)
		}
	}
	for _, mutate := range []func([]testWebPChunk){
		func(c []testWebPChunk) { c[0].data[0] = 0xc1 },
		func(c []testWebPChunk) { c[0].data[0] = 4 },
		func(c []testWebPChunk) { c[0].data[1] = 1 },
		func(c []testWebPChunk) { c[0].data = append(c[0].data, []byte("private-extension")...) },
	} {
		chunks := extendedWebP(t, "lossless", 0)
		mutate(chunks)
		data := encodeWebP(chunks)
		if _, err := inspectWebP(t.Context(), bytes.NewReader(data), int64(len(data)), DefaultInspectPolicy()); err != nil {
			t.Fatal("input reserved-field compatibility was changed", err)
		}
		if _, err := validateStillArtifact(t.Context(), "revision-a", "thumb_320", stillSource(3, 2, nil), data); !errors.Is(err, errStillArtifact) {
			t.Fatal("noncanonical output extended header accepted", err)
		}
	}
	if _, err := validateStillArtifact(t.Context(), "revision-a", "thumb_320", stillSource(3, 2, nil), plain); err != nil {
		t.Fatal("strict output rules accidentally rejected the owned baseline", err)
	}
}

func TestStaticArtifactRejectsUnknownIdentityGeometryCorruptionAndResourceLimits(t *testing.T) {
	data := webpFixture(t, "lossless")
	for _, revision := range []string{"", ".", "../escape", "/absolute", "other/revision", `other\revision`, "revision\x00", "revision\n", strings.Repeat("a", 129)} {
		if result, err := validateStillArtifact(t.Context(), revision, "thumb_320", stillSource(3, 2, nil), data); !errors.Is(err, errStillArtifact) || result != (stillArtifact{}) {
			t.Fatal("unsafe revision identity accepted", revision, result, err)
		}
	}
	for name, source := range map[string]Inspection{
		"missing": {}, "video": {Kind: "video", Status: "ok"},
		"uninspected": {Kind: "image", Width: new(int64(3)), Height: new(int64(2))},
		"nil-width":   {Kind: "image", Status: "ok", Height: new(int64(2))},
		"nil-height":  {Kind: "image", Status: "ok", Width: new(int64(3))},
		"zero":        stillSource(0, 2, nil), "negative": stillSource(3, -1, nil),
		"dimension-limit": stillSource(1000001, 1, nil), "pixel-limit": stillSource(1000000, 1001, nil),
		"wrong-size": stillSource(4, 2, nil), "not-an-exact-fit": stillSource(6, 4, nil),
		"wrong-rotation":   stillSource(3, 2, new(int64(6))),
		"unknown-rotation": stillSource(3, 2, new(int64(0))),
		"invalid-rotation": stillSource(3, 2, new(int64(9))),
	} {
		if result, err := validateStillArtifact(t.Context(), "revision-a", "thumb_320", source, data); !errors.Is(err, errStillArtifact) || result != (stillArtifact{}) {
			t.Fatal("unproven source geometry accepted", name, result, err)
		}
	}
	for _, bad := range [][]byte{nil, []byte("not a WebP"), data[:len(data)-1], append(bytes.Clone(data), 0), webpFixture(t, "animated"), bytes.Repeat([]byte{0}, (1<<20)+1)} {
		if result, err := validateStillArtifact(t.Context(), "revision-a", "thumb_320", stillSource(3, 2, nil), bad); !errors.Is(err, errStillArtifact) || result != (stillArtifact{}) {
			t.Fatal("corrupt/animated/oversize output accepted", len(bad), result, err)
		}
	}
	if result, err := validateStillArtifact(t.Context(), "revision-a", "poster", stillSource(3, 2, nil), data); !errors.Is(err, errStillArtifact) || result != (stillArtifact{}) {
		t.Fatal("unimplemented output preset accepted", result, err)
	}
	for _, key := range []string{"thumb_320", "thumb_720", "display_1920"} {
		preset, _ := staticPreset(key)
		oversize := make([]byte, preset.maxBytes+1)
		if result, err := validateStillArtifact(t.Context(), "revision-a", key, stillSource(3, 2, nil), oversize); !errors.Is(err, errStillArtifact) || result != (stillArtifact{}) {
			t.Fatal("per-preset byte budget was not enforced", key, result, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if result, err := validateStillArtifact(ctx, "revision-a", "thumb_320", stillSource(3, 2, nil), data); !errors.Is(err, context.Canceled) || result != (stillArtifact{}) {
		t.Fatal("cancelled artifact accepted", result, err)
	}
}

func FuzzStaticArtifactNeverTrustsUnboundedContainersOrGeometry(f *testing.F) {
	for _, key := range []string{"thumb_320", "thumb_720", "display_1920", "unknown"} {
		for _, name := range []string{"lossy", "lossless", "animated"} {
			f.Add(webpFixture(f, name), key, int64(3), int64(2), int64(1))
		}
	}
	f.Fuzz(func(t *testing.T, data []byte, key string, width, height, orientation int64) {
		result, err := validateStillArtifact(t.Context(), "revision-a", key, stillSource(width, height, &orientation), data)
		if err != nil {
			if result != (stillArtifact{}) {
				t.Fatal("invalid artifact retained trusted fields", result)
			}
			return
		}
		preset, err := staticPreset(key)
		if err != nil || result.width < 1 || result.height < 1 || max(result.width, result.height) > preset.longEdge || result.size != int64(len(data)) || result.size > preset.maxBytes || result.mime != "image/webp" {
			t.Fatal("successful artifact escaped finite bounds", result, err)
		}
		want, err := VariantPath(result.revision, result.preset, result.presetVersion, result.hash, result.mime)
		if err != nil || want != result.storage {
			t.Fatal("artifact identity was not canonical", result, err)
		}
	})
}
