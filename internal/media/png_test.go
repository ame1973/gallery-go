package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"strings"
	"testing"
	"time"
)

type testPNGChunk struct {
	kind string
	data []byte
}

type testAPNGDelay struct{ numerator, denominator uint16 }

func pngFixture(t testing.TB, white bool) []byte {
	t.Helper()
	picture := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	if white {
		picture.SetNRGBA(0, 0, color.NRGBA{255, 255, 255, 255})
	}
	var data bytes.Buffer
	if err := png.Encode(&data, picture); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func pngChunks(data []byte) []testPNGChunk {
	var chunks []testPNGChunk
	for offset := 8; offset < len(data); {
		length := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		chunks = append(chunks, testPNGChunk{string(data[offset+4 : offset+8]), append([]byte{}, data[offset+8:offset+8+length]...)})
		offset += length + 12
	}
	return chunks
}

func encodePNGChunks(chunks []testPNGChunk) []byte {
	data := []byte("\x89PNG\r\n\x1a\n")
	for _, chunk := range chunks {
		data = binary.BigEndian.AppendUint32(data, uint32(len(chunk.data)))
		data = append(data, chunk.kind...)
		data = append(data, chunk.data...)
		checksum := crc32.NewIEEE()
		_, _ = checksum.Write([]byte(chunk.kind))
		_, _ = checksum.Write(chunk.data)
		data = binary.BigEndian.AppendUint32(data, checksum.Sum32())
	}
	return data
}

func apngFixture(t testing.TB, included bool, plays uint32, delays []testAPNGDelay) []byte {
	t.Helper()
	base := pngChunks(pngFixture(t, false))
	white := pngChunks(pngFixture(t, true))
	control := binary.BigEndian.AppendUint32(nil, uint32(len(delays)))
	control = binary.BigEndian.AppendUint32(control, plays)
	chunks := []testPNGChunk{base[0], {"acTL", control}}
	var sequence uint32
	for index, delay := range delays {
		if !included && index == 0 {
			chunks = append(chunks, base[1])
		}
		frame := binary.BigEndian.AppendUint32(nil, sequence)
		sequence++
		for _, value := range []uint32{3, 2, 0, 0} {
			frame = binary.BigEndian.AppendUint32(frame, value)
		}
		frame = binary.BigEndian.AppendUint16(frame, delay.numerator)
		frame = binary.BigEndian.AppendUint16(frame, delay.denominator)
		frame = append(frame, 0, 0)
		chunks = append(chunks, testPNGChunk{"fcTL", frame})
		pixels := base[1].data
		if index%2 == 1 {
			pixels = white[1].data
		}
		if included && index == 0 {
			chunks = append(chunks, testPNGChunk{"IDAT", pixels})
		} else {
			payload := binary.BigEndian.AppendUint32(nil, sequence)
			sequence++
			chunks = append(chunks, testPNGChunk{"fdAT", append(payload, pixels...)})
		}
	}
	return encodePNGChunks(append(chunks, testPNGChunk{"IEND", nil}))
}

func TestPNGMetadataProvesStaticAndWhitelistsEXIF(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		chunks := pngChunks(pngFixture(t, false))
		exif := exifBytes(order, 6, "2026:11:05 12:34:56\x00", "+08:00\x00")
		chunks = append(chunks[:1], append([]testPNGChunk{{"eXIf", exif}, {"tEXt", []byte("Author\x00GPS /private/source/camera-secret")}}, chunks[1:]...)...)
		data := encodePNGChunks(chunks)
		result := inspectFixture(t, data, "misleading.gif", DefaultInspectPolicy())
		want := time.Date(2026, 11, 5, 4, 34, 56, 0, time.UTC)
		if result.Status != "ok" || result.Format != "png" || result.Animated == nil || *result.Animated || result.FrameCount == nil || *result.FrameCount != 1 || result.LoopCount == nil || *result.LoopCount != 1 || result.DurationMS != nil || result.Orientation == nil || *result.Orientation != 6 || result.CaptureAt == nil || !result.CaptureAt.Equal(want) {
			t.Fatal("PNG metadata lost static/EXIF evidence", result)
		}
		encoded, err := json.Marshal(result)
		if err != nil || strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "camera-secret") {
			t.Fatal("PNG private metadata leaked", string(encoded), err)
		}
		if _, err := png.Decode(bytes.NewReader(data)); err != nil {
			t.Fatal("EXIF PNG fixture does not decode", err)
		}
	}
	exif := exifBytes(binary.LittleEndian, 6, "2026:11:05 12:34:56\x00", "-00:00\x00")
	chunks := pngChunks(pngFixture(t, false))
	chunks = append(chunks[:1], append([]testPNGChunk{{"eXIf", exif}}, chunks[1:]...)...)
	result := inspectFixture(t, encodePNGChunks(chunks), "capture.png", DefaultInspectPolicy())
	if result.Status != "ok" || result.Orientation == nil || result.CaptureAt != nil {
		t.Fatal("PNG camera timezone was guessed", result)
	}
}

func TestAPNGMetadataCountsOnlyAnimationFramesAndRoundsTheCycleOnce(t *testing.T) {
	for _, test := range []struct {
		name     string
		included bool
		plays    uint32
		delays   []testAPNGDelay
		duration int64
	}{
		{"included", true, 2, []testAPNGDelay{{1, 10}, {1, 5}}, 300},
		{"excluded-default", false, 2, []testAPNGDelay{{1, 10}, {1, 5}}, 300},
		{"infinite", true, 0, []testAPNGDelay{{10, 0}, {20, 0}}, 300},
		{"one-frame", true, 1, []testAPNGDelay{{1, 10}}, 100},
		{"one-excluded-frame", false, 1, []testAPNGDelay{{1, 10}}, 100},
		{"max-plays", true, math.MaxInt32, []testAPNGDelay{{1, 10}}, 100},
		{"fractional", true, 1, []testAPNGDelay{{1, 3}, {1, 3}}, 667},
		{"submillisecond", false, 1, []testAPNGDelay{{1, 10000}, {1, 10000}}, 1},
		{"zero-is-unknown", true, 1, []testAPNGDelay{{0, 0}, {1, 10}}, -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := apngFixture(t, test.included, test.plays, test.delays)
			result := inspectFixture(t, data, "animation.apng", DefaultInspectPolicy())
			if result.Status != "ok" || result.Animated == nil || *result.Animated != (len(test.delays) > 1) || result.FrameCount == nil || *result.FrameCount != int64(len(test.delays)) || result.LoopCount == nil || *result.LoopCount != int64(test.plays) {
				t.Fatal("APNG default image/total plays were confused with GIF", result)
			}
			if test.duration >= 0 && (result.DurationMS == nil || *result.DurationMS != test.duration) || test.duration < 0 && result.DurationMS != nil {
				t.Fatal("APNG timing was guessed or rounded per frame", result)
			}
			// Rebuild each frame as a PNG and independently decode its zlib/pixels.
			chunks := pngChunks(data)
			for _, chunk := range chunks {
				if chunk.kind == "IDAT" || chunk.kind == "fdAT" {
					pixels := chunk.data
					if chunk.kind == "fdAT" {
						pixels = pixels[4:]
					}
					frame := encodePNGChunks([]testPNGChunk{chunks[0], {"IDAT", pixels}, {"IEND", nil}})
					if _, err := png.Decode(bytes.NewReader(frame)); err != nil {
						t.Fatal("APNG frame fixture does not decode", err)
					}
				}
			}
		})
	}
	chunks := pngChunks(apngFixture(t, true, 1, []testAPNGDelay{{1, 10}, {1, 5}}))
	chunks[2].data[24] = 2 // The first frame's PREVIOUS disposal is permitted, treated as BACKGROUND.
	result := inspectFixture(t, encodePNGChunks(chunks), "first-previous.png", DefaultInspectPolicy())
	if result.Status != "ok" {
		t.Fatal("valid first-frame PREVIOUS disposal rejected", result)
	}
}

func TestPNGMetadataRejectsCRCOrderSequencesAndAmbiguousEXIF(t *testing.T) {
	base := apngFixture(t, true, 2, []testAPNGDelay{{1, 10}, {1, 5}})
	changes := map[string]func([]testPNGChunk) []testPNGChunk{
		"no-header":          func(c []testPNGChunk) []testPNGChunk { return c[1:] },
		"duplicate-header":   func(c []testPNGChunk) []testPNGChunk { return append(c[:1], append([]testPNGChunk{c[0]}, c[1:]...)...) },
		"bad-header-size":    func(c []testPNGChunk) []testPNGChunk { c[0].data = nil; return c },
		"zero-width":         func(c []testPNGChunk) []testPNGChunk { binary.BigEndian.PutUint32(c[0].data, 0); return c },
		"bad-depth":          func(c []testPNGChunk) []testPNGChunk { c[0].data[8] = 3; return c },
		"bad-compression":    func(c []testPNGChunk) []testPNGChunk { c[0].data[10] = 1; return c },
		"bad-interlace":      func(c []testPNGChunk) []testPNGChunk { c[0].data[12] = 2; return c },
		"no-end":             func(c []testPNGChunk) []testPNGChunk { return c[:len(c)-1] },
		"nonempty-end":       func(c []testPNGChunk) []testPNGChunk { c[len(c)-1].data = []byte{0}; return c },
		"no-idat":            func(c []testPNGChunk) []testPNGChunk { return append(c[:3], c[4:]...) },
		"empty-idat":         func(c []testPNGChunk) []testPNGChunk { c[3].data = nil; return c },
		"no-animation":       func(c []testPNGChunk) []testPNGChunk { return append(c[:1], c[2:]...) },
		"duplicate-actl":     func(c []testPNGChunk) []testPNGChunk { return append(c[:2], append([]testPNGChunk{c[1]}, c[2:]...)...) },
		"zero-frame-count":   func(c []testPNGChunk) []testPNGChunk { binary.BigEndian.PutUint32(c[1].data, 0); return c },
		"too-few-frames":     func(c []testPNGChunk) []testPNGChunk { binary.BigEndian.PutUint32(c[1].data, 3); return c },
		"too-many-frames":    func(c []testPNGChunk) []testPNGChunk { binary.BigEndian.PutUint32(c[1].data, 1); return c },
		"plays-high-bit":     func(c []testPNGChunk) []testPNGChunk { binary.BigEndian.PutUint32(c[1].data[4:], 1<<31); return c },
		"sequence-start":     func(c []testPNGChunk) []testPNGChunk { binary.BigEndian.PutUint32(c[2].data, 1); return c },
		"sequence-gap":       func(c []testPNGChunk) []testPNGChunk { binary.BigEndian.PutUint32(c[4].data, 2); return c },
		"sequence-duplicate": func(c []testPNGChunk) []testPNGChunk { binary.BigEndian.PutUint32(c[5].data, 1); return c },
		"frame-width-zero":   func(c []testPNGChunk) []testPNGChunk { binary.BigEndian.PutUint32(c[4].data[4:], 0); return c },
		"frame-outside": func(c []testPNGChunk) []testPNGChunk {
			binary.BigEndian.PutUint32(c[4].data[12:], math.MaxUint32)
			return c
		},
		"default-mismatch": func(c []testPNGChunk) []testPNGChunk { binary.BigEndian.PutUint32(c[2].data[4:], 2); return c },
		"bad-disposal":     func(c []testPNGChunk) []testPNGChunk { c[4].data[24] = 3; return c },
		"bad-blend":        func(c []testPNGChunk) []testPNGChunk { c[4].data[25] = 2; return c },
		"empty-frame":      func(c []testPNGChunk) []testPNGChunk { c[5].data = c[5].data[:4]; return c },
		"short-frame-data": func(c []testPNGChunk) []testPNGChunk { c[5].data = c[5].data[:3]; return c },
		"missing-control":  func(c []testPNGChunk) []testPNGChunk { return append(c[:4], c[5:]...) },
		"empty-control":    func(c []testPNGChunk) []testPNGChunk { c[4].data = nil; return c },
		"invalid-chunk-name": func(c []testPNGChunk) []testPNGChunk {
			return append(c[:4], append([]testPNGChunk{{"ab1D", nil}}, c[4:]...)...)
		},
		"reserved-name-bit": func(c []testPNGChunk) []testPNGChunk {
			return append(c[:4], append([]testPNGChunk{{"abcd", nil}}, c[4:]...)...)
		},
		"noncontiguous-idat": func(c []testPNGChunk) []testPNGChunk {
			return append(c[:4], append([]testPNGChunk{{"tEXt", nil}, {"IDAT", nil}}, c[4:]...)...)
		},
		"late-exif": func(c []testPNGChunk) []testPNGChunk {
			return append(c[:4], append([]testPNGChunk{{"eXIf", exifBytes(binary.LittleEndian, 1, "2026:11:05 12:34:56\x00", "+08:00\x00")}}, c[4:]...)...)
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			result := inspectFixture(t, encodePNGChunks(change(pngChunks(base))), "bad.png", DefaultInspectPolicy())
			if result.Status != "invalid" || result.ErrorCode == nil || *result.ErrorCode != "INVALID_MEDIA" || result.Animated != nil || result.FrameCount != nil || result.DurationMS != nil || result.Width != nil || result.Orientation != nil || result.CaptureAt != nil {
				t.Fatal("malformed PNG gained trusted metadata", name, result)
			}
		})
	}
	for _, change := range []string{"crc", "trailing", "length-high-bit", "truncated"} {
		data := append([]byte{}, base...)
		switch change {
		case "crc":
			data[len(data)-1] ^= 1
		case "trailing":
			data = append(data, base...)
		case "length-high-bit":
			binary.BigEndian.PutUint32(data[8:12], 1<<31)
		case "truncated":
			data = data[:len(data)-3]
		}
		result := inspectFixture(t, data, "broken.apng", DefaultInspectPolicy())
		if result.Status != "invalid" || result.ErrorCode == nil || *result.ErrorCode != "INVALID_MEDIA" || result.FrameCount != nil {
			t.Fatal(change, result)
		}
	}
	for _, exif := range [][]byte{[]byte("Exif\x00\x00"), exifBytes(binary.LittleEndian, 0, "2026:11:05 12:34:56\x00", "+08:00\x00")} {
		chunks := pngChunks(pngFixture(t, false))
		chunks = append(chunks[:1], append([]testPNGChunk{{"eXIf", exif}, {"eXIf", exif}}, chunks[1:]...)...)
		result := inspectFixture(t, encodePNGChunks(chunks), "exif.png", DefaultInspectPolicy())
		if result.Status != "invalid" || result.CaptureAt != nil || result.Orientation != nil {
			t.Fatal("ambiguous/bad PNG EXIF accepted", result)
		}
	}
}

func TestPNGMetadataPaletteChunkContinuityAndCriticalCapabilities(t *testing.T) {
	picture := image.NewPaletted(image.Rect(0, 0, 3, 2), color.Palette{color.Black, color.White})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, picture); err != nil {
		t.Fatal(err)
	}
	palette := pngChunks(encoded.Bytes())
	for _, change := range []string{"valid", "missing", "duplicate", "oversize"} {
		chunks := pngChunks(encoded.Bytes())
		switch change {
		case "missing":
			chunks = append(chunks[:1], chunks[2:]...)
		case "duplicate":
			chunks = append(chunks[:2], append([]testPNGChunk{chunks[1]}, chunks[2:]...)...)
		case "oversize":
			chunks[1].data = make([]byte, 771)
		}
		result := inspectFixture(t, encodePNGChunks(chunks), "palette.png", DefaultInspectPolicy())
		if change == "valid" && result.Status != "ok" || change != "valid" && result.Status != "invalid" {
			t.Fatal("palette rules violated", change, result)
		}
	}
	chunks := pngChunks(pngFixture(t, false))
	chunks[0].data[9] = 0 // A grayscale PNG cannot declare a palette.
	chunks = append(chunks[:1], append([]testPNGChunk{palette[1]}, chunks[1:]...)...)
	if result := inspectFixture(t, encodePNGChunks(chunks), "gray.png", DefaultInspectPolicy()); result.Status != "invalid" {
		t.Fatal("forbidden grayscale palette accepted", result)
	}
	base := pngChunks(apngFixture(t, true, 2, []testAPNGDelay{{1, 10}, {1, 5}}))
	pixels := base[3].data
	base[3].data = nil // Zero-length IDAT is valid if the same contiguous run has data.
	base = append(base[:4], append([]testPNGChunk{{"IDAT", pixels}}, base[4:]...)...)
	frameData := base[6].data
	base[6].data = append([]byte{}, frameData[:4]...)
	frameData[3] = 3 // Empty fdAT still participates in the shared sequence counter.
	base = append(base[:7], append([]testPNGChunk{{"fdAT", frameData}}, base[7:]...)...)
	if result := inspectFixture(t, encodePNGChunks(base), "split.png", DefaultInspectPolicy()); result.Status != "ok" || result.FrameCount == nil || *result.FrameCount != 2 {
		t.Fatal("valid zero-length IDAT/fdAT subchunks rejected", result)
	}
	chunks = pngChunks(pngFixture(t, false))
	chunks = append(chunks[:2], append([]testPNGChunk{{"XYZI", nil}}, chunks[2:]...)...)
	if result := inspectFixture(t, encodePNGChunks(chunks), "critical.png", DefaultInspectPolicy()); result.Status != "unsupported" || result.ErrorCode == nil || *result.ErrorCode != "CODEC_UNAVAILABLE" || result.Animated != nil {
		t.Fatal("unknown critical chunk became trusted metadata", result)
	}
	chunks = pngChunks(pngFixture(t, false))
	exif := exifBytes(binary.LittleEndian, 6, "2026:11:05 12:34:56\x00", "+08:00\x00")
	chunks = append(chunks[:1], append([]testPNGChunk{{"eXIf", exif}, {"eXIf", exif}}, chunks[1:]...)...)
	if result := inspectFixture(t, encodePNGChunks(chunks), "duplicate.png", DefaultInspectPolicy()); result.Status != "invalid" || result.Orientation != nil || result.CaptureAt != nil {
		t.Fatal("duplicate otherwise valid EXIF was accepted", result)
	}
}

func TestPNGMetadataResourceLimitsAndCancellation(t *testing.T) {
	base := apngFixture(t, true, 1, []testAPNGDelay{{1, 10}, {1, 5}})
	for _, limit := range []string{"bytes", "frames", "duration", "unknown-duration", "exif"} {
		policy := DefaultInspectPolicy()
		data := base
		switch limit {
		case "bytes":
			policy.HeaderBytes = 512
			chunks := pngChunks(base)
			data = encodePNGChunks(append(chunks[:1], append([]testPNGChunk{{"tEXt", bytes.Repeat([]byte{'x'}, 600)}}, chunks[1:]...)...))
		case "frames":
			policy.MaxFrames = 1
		case "duration":
			policy.MaxDuration = 299 * time.Millisecond
		case "unknown-duration":
			policy.MaxDuration = 299 * time.Millisecond
			data = apngFixture(t, false, 1, []testAPNGDelay{{0, 100}, {3, 10}})
		case "exif":
			chunks := pngChunks(base)
			data = encodePNGChunks(append(chunks[:1], append([]testPNGChunk{{"eXIf", make([]byte, (64<<10)+1)}}, chunks[1:]...)...))
		}
		result := inspectFixture(t, data, "limited.png", policy)
		if result.Status != "invalid" || result.ErrorCode == nil || *result.ErrorCode != "MEDIA_LIMIT" || result.Animated != nil || result.FrameCount != nil || result.Orientation != nil {
			t.Fatal("PNG metadata resource limit bypassed", limit, result)
		}
	}
	policy := DefaultInspectPolicy()
	policy.MaxFrames, policy.MaxDuration, policy.HeaderBytes = 2, 300*time.Millisecond, 512
	chunks := pngChunks(base)
	padding := 512 - len(base) - 12
	data := encodePNGChunks(append(chunks[:1], append([]testPNGChunk{{"tEXt", bytes.Repeat([]byte{'x'}, padding)}}, chunks[1:]...)...))
	result := inspectFixture(t, data, "boundary.png", policy)
	if len(data) != 512 || result.Status != "ok" || result.DurationMS == nil || *result.DurationMS != 300 {
		t.Fatal("inclusive PNG byte/frame/duration boundaries rejected", len(data), result)
	}
	ctx, cancel := context.WithCancel(t.Context())
	_, err := inspectPNG(ctx, &cancelGIFReader{Reader: bytes.NewReader(base), cancel: cancel}, int64(len(base)), DefaultInspectPolicy())
	if !errors.Is(err, context.Canceled) {
		t.Fatal("PNG inspection ignored mid-read cancellation", err)
	}
}

func FuzzBoundedPNGMetadata(f *testing.F) {
	f.Add(pngFixture(f, false))
	f.Add(apngFixture(f, true, 0, []testAPNGDelay{{1, 10}, {1, 5}}))
	f.Add([]byte("\x89PNG\r\n\x1a\n"))
	policy := DefaultInspectPolicy()
	policy.HeaderBytes, policy.MaxFrames, policy.MaxDuration = 65536, 64, time.Minute
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 65536 {
			data = data[:65536]
		}
		result, err := inspectPNG(t.Context(), bytes.NewReader(data), int64(len(data)), policy)
		if err != nil && !errors.Is(err, errPNG) && !errors.Is(err, errPNGUnsupported) && !errors.Is(err, errEXIF) && !errors.Is(err, errMetadataLimit) && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatal(err)
		}
		if err == nil && (result.frames < 1 || result.frames > 64 || result.loops < 0 || result.loops > math.MaxInt32 || result.duration != nil && (*result.duration < 1 || *result.duration > time.Minute.Milliseconds())) {
			t.Fatal("invalid or unbounded PNG metadata", result)
		}
	})
}
