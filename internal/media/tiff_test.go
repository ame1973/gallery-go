package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tiffFixture(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("../../tests/fixtures/tiff.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]string
	if json.Unmarshal(data, &fixtures) != nil {
		t.Fatal("invalid owned TIFF vectors")
	}
	decoded, err := base64.StdEncoding.DecodeString(fixtures[name])
	if err != nil || len(decoded) == 0 {
		t.Fatal("missing owned TIFF vector", name, err)
	}
	return decoded
}

func tiffTag(t testing.TB, data []byte, tag uint16) []byte {
	t.Helper()
	var order binary.ByteOrder = binary.LittleEndian
	if string(data[:2]) == "MM" {
		order = binary.BigEndian
	}
	first := int(order.Uint32(data[4:8]))
	count := int(order.Uint16(data[first : first+2]))
	for index := range count {
		entry := data[first+2+index*12 : first+2+(index+1)*12]
		if order.Uint16(entry[:2]) == tag {
			return entry
		}
	}
	t.Fatalf("missing TIFF test tag %x", tag)
	return nil
}

func TestTIFFMetadataUsesFirstPageAndSharedEXIFWithoutInventingAnimation(t *testing.T) {
	for _, name := range []string{"little_exif", "big_exif", "multipage", "deflate", "plain", "tiled_deflate", "strips"} {
		t.Run(name, func(t *testing.T) {
			result := inspectFixture(t, tiffFixture(t, name), "misleading.mp4", DefaultInspectPolicy())
			if result.Status != "ok" || result.Kind != "image" || result.MIME != "image/tiff" || result.Format != "tiff" || result.Codec == nil || *result.Codec != "tiff" || result.Width == nil || *result.Width != 12 || result.Height == nil || *result.Height != 7 {
				t.Fatal("TIFF encoded first-page metadata lost or guessed", result)
			}
			if result.DurationMS != nil || result.Animated != nil || result.FrameCount != nil || result.LoopCount != nil {
				t.Fatal("document pages fabricated image-animation evidence", result)
			}
			if name == "plain" {
				if result.Orientation != nil || result.CaptureAt != nil {
					t.Fatal("missing EXIF used a page default or file clock", result)
				}
			} else if result.Orientation == nil || *result.Orientation != 6 || result.CaptureAt == nil || result.CaptureAt.Format(time.RFC3339Nano) != "2026-11-05T04:34:56.123Z" {
				t.Fatal("TIFF orientation/explicit-zone millisecond whitelist diverged", result)
			}
		})
	}
}

func TestTIFFRequiresItsIsolatedRuntimeAndNeverFallsBackToHeaders(t *testing.T) {
	policy, err := DefaultInspectPolicy().PinCodecRuntime(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	policy.imageRuntime = "unavailable"
	result := inspectFixture(t, tiffFixture(t, "little_exif"), "photo.tiff", policy)
	if result.Status != "unsupported" || result.ErrorCode == nil || *result.ErrorCode != "CODEC_UNAVAILABLE" || result.Width != nil || result.Codec != nil || result.Orientation != nil || result.CaptureAt != nil {
		t.Fatal("missing TIFF capability trusted native headers", result)
	}
}

func TestTIFFMalformedDirectoriesWhitelistedFieldsAndExtentsFailClosed(t *testing.T) {
	order := binary.LittleEndian
	for name, change := range map[string]func([]byte){
		"zero width":      func(data []byte) { order.PutUint32(tiffTag(t, data, 256)[8:12], 0) },
		"width type":      func(data []byte) { order.PutUint16(tiffTag(t, data, 256)[2:4], 2) },
		"width count":     func(data []byte) { order.PutUint32(tiffTag(t, data, 256)[4:8], 2) },
		"duplicate width": func(data []byte) { order.PutUint16(tiffTag(t, data, 257)[:2], 256) },
		"first offset":    func(data []byte) { order.PutUint32(data[4:8], 0xffffffff) },
		"no first page":   func(data []byte) { order.PutUint32(data[4:8], 0) },
		"directory loop": func(data []byte) {
			count := int(order.Uint16(data[8:10]))
			order.PutUint32(data[10+count*12:14+count*12], 8)
		},
		"next offset": func(data []byte) {
			count := int(order.Uint16(data[8:10]))
			order.PutUint32(data[10+count*12:14+count*12], 0xffffffff)
		},
		"EXIF loop":        func(data []byte) { order.PutUint32(tiffTag(t, data, 34665)[8:12], 8) },
		"EXIF offset":      func(data []byte) { order.PutUint32(tiffTag(t, data, 34665)[8:12], 0xffffffff) },
		"orientation":      func(data []byte) { order.PutUint16(tiffTag(t, data, 274)[8:10], 9) },
		"strip outside":    func(data []byte) { order.PutUint32(tiffTag(t, data, 273)[8:12], 0xffffffff) },
		"empty strip":      func(data []byte) { order.PutUint32(tiffTag(t, data, 279)[8:12], 0) },
		"strip count":      func(data []byte) { order.PutUint32(tiffTag(t, data, 279)[4:8], 0) },
		"missing strip":    func(data []byte) { order.PutUint16(tiffTag(t, data, 273)[:2], 272) },
		"mixed strip tile": func(data []byte) { order.PutUint16(tiffTag(t, data, 270)[:2], 324) },
		"EXIF fraction": func(data []byte) {
			exif := int(order.Uint32(tiffTag(t, data, 34665)[8:12]))
			position := int(order.Uint32(data[exif+34 : exif+38]))
			data[position] = 'x'
		},
	} {
		t.Run(name, func(t *testing.T) {
			data := tiffFixture(t, "little_exif")
			change(data)
			result := inspectFixture(t, data, "bad.tiff", DefaultInspectPolicy())
			if result.Status != "invalid" || result.ErrorCode == nil || *result.ErrorCode != "INVALID_MEDIA" || result.Width != nil || result.Codec != nil || result.Orientation != nil || result.CaptureAt != nil {
				t.Fatal("malformed TIFF retained trusted metadata", name, result)
			}
		})
	}
}

func TestTIFFUnknownRawReducedAndBigTIFFProfilesStayExplicitUnsupported(t *testing.T) {
	order := binary.LittleEndian
	for name, change := range map[string]func([]byte){
		"BigTIFF":             func(data []byte) { order.PutUint16(data[2:4], 43) },
		"Canon raw":           func(data []byte) { copy(data[8:12], "CR\x02\x00") },
		"DNG":                 func(data []byte) { order.PutUint16(tiffTag(t, data, 270)[:2], 0xc612) },
		"SubIFD primary":      func(data []byte) { order.PutUint16(tiffTag(t, data, 270)[:2], 0x014a) },
		"CFA raw":             func(data []byte) { order.PutUint16(tiffTag(t, data, 262)[8:10], 32803) },
		"private compression": func(data []byte) { order.PutUint16(tiffTag(t, data, 259)[8:10], 32767) },
		"reduced first page": func(data []byte) {
			entry := tiffTag(t, data, 270)
			order.PutUint16(entry[:2], 254)
			order.PutUint16(entry[2:4], 4)
			order.PutUint32(entry[4:8], 1)
			order.PutUint32(entry[8:12], 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			data := tiffFixture(t, "little_exif")
			change(data)
			result := inspectFixture(t, data, "photo.dng", DefaultInspectPolicy())
			if result.Status != "unsupported" || result.ErrorCode == nil || *result.ErrorCode != "CODEC_UNAVAILABLE" || result.Width != nil || result.Orientation != nil || result.Codec != nil {
				t.Fatal("unsupported TIFF/raw profile invented capability", result)
			}
		})
	}
}

func TestTIFFDirectoryPixelsVectorAndByteBudgets(t *testing.T) {
	for name, change := range map[string]func(*InspectPolicy){
		"pixels":    func(policy *InspectPolicy) { policy.MaxPixels = 83 },
		"dimension": func(policy *InspectPolicy) { policy.MaxDimension = 11 },
		"pages":     func(policy *InspectPolicy) { policy.MaxFrames = 1 },
		"bytes":     func(policy *InspectPolicy) { policy.HeaderBytes = 512 },
	} {
		t.Run(name, func(t *testing.T) {
			policy := DefaultInspectPolicy()
			change(&policy)
			result := inspectFixture(t, tiffFixture(t, "multipage"), "pages.tiff", policy)
			if result.Status != "invalid" || result.ErrorCode == nil || *result.ErrorCode != "MEDIA_LIMIT" || result.Width != nil {
				t.Fatal("TIFF metadata policy bypass", result)
			}
		})
	}
	for _, entryLimit := range []bool{false, true} {
		data := tiffFixture(t, "little_exif")
		if entryLimit {
			binary.LittleEndian.PutUint16(data[8:10], 513)
		} else {
			binary.LittleEndian.PutUint32(tiffTag(t, data, 273)[4:8], 1<<20+1)
		}
		result := inspectFixture(t, data, "bomb.tiff", DefaultInspectPolicy())
		if result.ErrorCode == nil || *result.ErrorCode != "MEDIA_LIMIT" || result.CaptureAt != nil {
			t.Fatal("TIFF directory/vector allocation cap bypass", result)
		}
	}
}

func TestTIFFStripVectorsAcceptBothIntegerWidthsAndRejectUnboundedValues(t *testing.T) {
	data := tiffFixture(t, "little_exif")
	for _, tag := range []uint16{273, 279} {
		entry := tiffTag(t, data, tag)
		value := binary.LittleEndian.Uint32(entry[8:12])
		binary.LittleEndian.PutUint16(entry[2:4], 3)
		binary.LittleEndian.PutUint16(entry[8:10], uint16(value))
		clear(entry[10:12])
	}
	if result := inspectFixture(t, data, "short.tif", DefaultInspectPolicy()); result.Status != "ok" {
		t.Fatal("legal SHORT strip vectors rejected", result)
	}
	order := binary.BigEndian
	for name, change := range map[string]func([]byte){
		"different counts":       func(data []byte) { order.PutUint32(tiffTag(t, data, 273)[4:8], 2) },
		"offset array outside":   func(data []byte) { order.PutUint32(tiffTag(t, data, 273)[8:12], 0xffffffff) },
		"counts array outside":   func(data []byte) { order.PutUint32(tiffTag(t, data, 279)[8:12], 0xffffffff) },
		"offset array in header": func(data []byte) { order.PutUint32(tiffTag(t, data, 273)[8:12], 0) },
		"strip vector outside": func(data []byte) {
			offset := order.Uint32(tiffTag(t, data, 273)[8:12])
			order.PutUint32(data[offset:offset+4], 0xffffffff)
		},
		"empty strip in vector": func(data []byte) {
			offset := order.Uint32(tiffTag(t, data, 279)[8:12])
			order.PutUint16(data[offset:offset+2], 0)
		},
	} {
		t.Run(name, func(t *testing.T) {
			data := tiffFixture(t, "strips")
			change(data)
			result := inspectFixture(t, data, "bad.tif", DefaultInspectPolicy())
			if result.ErrorCode == nil || *result.ErrorCode != "INVALID_MEDIA" || result.Width != nil || result.CaptureAt != nil {
				t.Fatal("malformed TIFF strip vector gained metadata", result)
			}
		})
	}
	data = tiffFixture(t, "strips")
	order.PutUint16(tiffTag(t, data, 273)[2:4], 16)
	if result := inspectFixture(t, data, "long8.tif", DefaultInspectPolicy()); result.ErrorCode == nil || *result.ErrorCode != "CODEC_UNAVAILABLE" {
		t.Fatal("classic TIFF accepted an unsupported 64-bit offset profile", result)
	}
}

func TestTIFFScatteredEXIFUsesActualReadBudgetNotWholeSourcePrefix(t *testing.T) {
	data := tiffFixture(t, "little_exif")
	old := binary.LittleEndian.Uint32(tiffTag(t, data, 34665)[8:12])
	far := uint32(2 << 20)
	scattered := make([]byte, int(far)+42)
	copy(scattered, data)
	copy(scattered[far:], data[old:old+42])
	binary.LittleEndian.PutUint32(tiffTag(t, scattered, 34665)[8:12], far)
	policy := DefaultInspectPolicy()
	policy.HeaderBytes = 512
	result := inspectFixture(t, scattered, "scattered.tif", policy)
	if result.Status != "ok" || result.CaptureAt == nil || result.CaptureAt.Format(time.RFC3339Nano) != "2026-11-05T04:34:56.123Z" {
		t.Fatal("scattered bounded EXIF required copying the source prefix", result)
	}
}

func TestTIFFNeverGuessesOriginalTimezoneOrUsesLaterPageEXIF(t *testing.T) {
	data := tiffFixture(t, "multipage")
	exif := int(binary.LittleEndian.Uint32(tiffTag(t, data, 34665)[8:12]))
	binary.LittleEndian.PutUint16(data[exif+14:exif+16], 0x9999)
	result := inspectFixture(t, data, "pages.tif", DefaultInspectPolicy())
	if result.Status != "ok" || result.CaptureAt != nil || result.Orientation == nil || *result.Orientation != 6 {
		t.Fatal("a later page or host clock supplied missing primary timezone", result)
	}
}

func TestTIFFNativeTruncationAndCancellation(t *testing.T) {
	data := tiffFixture(t, "little_exif")
	for length := 0; length < len(data); length++ {
		if _, err := parseTIFF(t.Context(), bytes.NewReader(data[:length]), int64(length), DefaultInspectPolicy()); err == nil {
			t.Fatal("truncated TIFF declared pixels/metadata accepted", length)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := parseTIFF(ctx, bytes.NewReader(data), int64(len(data)), DefaultInspectPolicy()); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled TIFF metadata continued", err)
	}
	path := filepath.Join(t.TempDir(), "photo.tif")
	if err := os.WriteFile(path, data, 0o400); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	policy, err := DefaultInspectPolicy().PinCodecRuntime(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	policy.imageRuntime = string(bytes.Repeat([]byte{'0'}, 64))
	if _, err := Inspect(t.Context(), file, "photo.tif", policy); !errors.Is(err, ErrContentChanged) {
		t.Fatal("TIFF accepted a changed image installation epoch", err)
	}
}

func FuzzBoundedTIFF(f *testing.F) {
	for _, name := range []string{"little_exif", "big_exif", "multipage", "deflate", "plain", "tiled_deflate", "strips"} {
		f.Add(tiffFixture(f, name))
	}
	f.Add([]byte("II+\x00\x08\x00\x00\x00"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		policy := DefaultInspectPolicy()
		policy.HeaderBytes = 4096
		info, err := parseTIFF(t.Context(), bytes.NewReader(data), int64(len(data)), policy)
		if err != nil && !errors.Is(err, errTIFF) && !errors.Is(err, errTIFFUnsupported) && !errors.Is(err, errEXIF) && !errors.Is(err, errMetadataLimit) && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatal("unexpected native TIFF error", err)
		}
		if err == nil && (info.width < 1 || info.height < 1 || info.width > int64(policy.MaxDimension) || info.height > int64(policy.MaxDimension) || info.width > policy.MaxPixels/info.height || info.orientation != nil && (*info.orientation < 1 || *info.orientation > 8) || info.capture != nil && info.capture.Nanosecond()%int(time.Millisecond) != 0) {
			t.Fatal("unbounded/invalid TIFF metadata", info)
		}
	})
}
