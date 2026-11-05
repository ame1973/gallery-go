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
	"strings"
	"testing"
	"time"
)

func webpFixture(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", "webp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]string
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	input, err := base64.StdEncoding.DecodeString(fixtures[name])
	if err != nil || len(input) == 0 {
		t.Fatal("invalid synthetic WebP fixture", name, err)
	}
	return input
}

func TestWebPMetadataUsesRealLossyLosslessAndAnimatedContainers(t *testing.T) {
	for _, name := range []string{"lossy", "lossless", "animated"} {
		result := inspectFixture(t, webpFixture(t, name), "misleading.mp4", DefaultInspectPolicy())
		if result.Status != "ok" || result.MIME != "image/webp" || result.Width == nil || *result.Width != 3 || result.Height == nil || *result.Height != 2 || result.Animated == nil || *result.Animated != (name == "animated") || result.FrameCount == nil {
			t.Fatal("WebP metadata unavailable or invented", name, result)
		}
		if name == "animated" && (*result.FrameCount != 2 || result.LoopCount == nil || *result.LoopCount != 2 || result.DurationMS == nil || *result.DurationMS != 200) {
			t.Fatal("WebP total plays/cycle timing was confused with GIF", result)
		}
	}
}

type testWebPChunk struct {
	kind string
	data []byte
}

func webpChunks(data []byte) []testWebPChunk {
	var chunks []testWebPChunk
	for offset := 12; offset < len(data); {
		length := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		chunks = append(chunks, testWebPChunk{string(data[offset : offset+4]), append([]byte{}, data[offset+8:offset+8+length]...)})
		offset += 8 + length + length%2
	}
	return chunks
}

func encodeWebP(chunks []testWebPChunk) []byte {
	body := []byte("WEBP")
	for _, chunk := range chunks {
		body = append(body, chunk.kind...)
		body = binary.LittleEndian.AppendUint32(body, uint32(len(chunk.data)))
		body = append(body, chunk.data...)
		if len(chunk.data)%2 != 0 {
			body = append(body, 0)
		}
	}
	result := binary.LittleEndian.AppendUint32([]byte("RIFF"), uint32(len(body)))
	return append(result, body...)
}

func extendedWebP(t testing.TB, name string, flags byte) []testWebPChunk {
	t.Helper()
	header := []byte{flags, 0, 0, 0, 2, 0, 0, 1, 0, 0}
	return append([]testWebPChunk{{"VP8X", header}}, webpChunks(webpFixture(t, name))...)
}

func TestWebPMetadataWhitelistsEXIFAndIgnoresReservedOrPrivateData(t *testing.T) {
	for _, prefixed := range []bool{false, true} {
		chunks := extendedWebP(t, "lossless", 8)
		exif := exifBytes(binary.LittleEndian, 6, "2026:11:05 12:34:56\x00", "+08:00\x00")
		if prefixed {
			exif = append([]byte("Exif\x00\x00"), exif...)
		}
		chunks = append(chunks, testWebPChunk{"EXIF", exif}, testWebPChunk{"XMP ", []byte("GPS /private/source/camera-secret")})
		result := inspectFixture(t, encodeWebP(chunks), "capture.webp", DefaultInspectPolicy())
		want := time.Date(2026, 11, 5, 4, 34, 56, 0, time.UTC)
		data, err := json.Marshal(result)
		if err != nil || result.Status != "ok" || result.Orientation == nil || *result.Orientation != 6 || result.CaptureAt == nil || !result.CaptureAt.Equal(want) || result.Animated == nil || *result.Animated || strings.Contains(string(data), "private") || strings.Contains(string(data), "camera-secret") {
			t.Fatal("WebP EXIF/private boundary failed", prefixed, result, string(data), err)
		}
	}
	chunks := webpChunks(webpFixture(t, "animated"))
	chunks[0].data[0] |= 0xc1
	chunks[0].data[1], chunks[0].data[2], chunks[0].data[3] = 1, 2, 3
	chunks[0].data = append(chunks[0].data, 0xaa, 0xbb)
	chunks[2].data[15] |= 0xfc
	result := inspectFixture(t, encodeWebP(chunks), "reserved.webp", DefaultInspectPolicy())
	if result.Status != "ok" || result.FrameCount == nil || *result.FrameCount != 2 {
		t.Fatal("WebP reader failed to ignore reserved/future fields", result)
	}
	chunks = extendedWebP(t, "lossy", 0)
	chunks = append(chunks, testWebPChunk{"ANIM", []byte{1}}, testWebPChunk{"ANMF", []byte{2}})
	result = inspectFixture(t, encodeWebP(chunks), "static.webp", DefaultInspectPolicy())
	if result.Status != "ok" || result.Animated == nil || *result.Animated || result.FrameCount == nil || *result.FrameCount != 1 || result.LoopCount == nil || *result.LoopCount != 1 {
		t.Fatal("animation-disabled WebP incorrectly interpreted animation chunks", result)
	}
}

func TestWebPMetadataAcceptsOrderedAlphaProfileAndUnknownFrameTails(t *testing.T) {
	chunks := extendedWebP(t, "lossy", 16|32)
	alpha := []byte{0, 255, 255, 255, 255, 255, 255}
	chunks = append(chunks[:1], append([]testWebPChunk{{"ICCP", []byte("private-profile")}, {"ALPH", alpha}}, chunks[1:]...)...)
	result := inspectFixture(t, encodeWebP(chunks), "alpha.webp", DefaultInspectPolicy())
	if result.Status != "ok" || result.Width == nil || *result.Width != 3 {
		t.Fatal("valid reconstruction chunk order rejected", result)
	}
	chunks = webpChunks(webpFixture(t, "animated"))
	lossy := webpChunks(webpFixture(t, "lossy"))[0]
	frame := encodeWebP([]testWebPChunk{{"ALPH", alpha}, lossy, {"JUNK", []byte{1, 2, 3}}})[12:]
	chunks[2].data = append(chunks[2].data[:16], frame...)
	result = inspectFixture(t, encodeWebP(chunks), "frame-alpha.webp", DefaultInspectPolicy())
	if result.Status != "ok" || result.FrameCount == nil || *result.FrameCount != 2 || result.DurationMS == nil || *result.DurationMS != 200 {
		t.Fatal("valid alpha/unknown frame tail changed animation metadata", result)
	}
}

func TestWebPMetadataUsesTotalPlaysAndPreservesZeroDurationUnknown(t *testing.T) {
	for _, plays := range []uint16{0, 1, 2, 65535} {
		chunks := webpChunks(webpFixture(t, "animated"))
		binary.LittleEndian.PutUint16(chunks[1].data[4:], plays)
		result := inspectFixture(t, encodeWebP(chunks), "animation.webp", DefaultInspectPolicy())
		if result.Status != "ok" || result.LoopCount == nil || *result.LoopCount != int64(plays) || result.DurationMS == nil || *result.DurationMS != 200 {
			t.Fatal("WebP total playthroughs were incremented or multiplied", result)
		}
	}
	chunks := webpChunks(webpFixture(t, "animated"))
	copy(chunks[2].data[12:15], []byte{0, 0, 0})
	result := inspectFixture(t, encodeWebP(chunks), "zero.webp", DefaultInspectPolicy())
	if result.Status != "ok" || result.DurationMS != nil || result.FrameCount == nil || *result.FrameCount != 2 {
		t.Fatal("zero WebP timing was browser-clamped or invented", result)
	}
}

func TestWebPMetadataRejectsMalformedFramesBitstreamsAndRIFF(t *testing.T) {
	base := webpFixture(t, "animated")
	changes := map[string]func([]testWebPChunk) []testWebPChunk{
		"no-animation-control": func(c []testWebPChunk) []testWebPChunk { return append(c[:1], c[2:]...) },
		"no-frames":            func(c []testWebPChunk) []testWebPChunk { return c[:2] },
		"duplicate-control": func(c []testWebPChunk) []testWebPChunk {
			return append(c[:2], append([]testWebPChunk{c[1]}, c[2:]...)...)
		},
		"duplicate-header":    func(c []testWebPChunk) []testWebPChunk { return append(c[:1], c...) },
		"short-header":        func(c []testWebPChunk) []testWebPChunk { c[0].data = c[0].data[:9]; return c },
		"short-control":       func(c []testWebPChunk) []testWebPChunk { c[1].data = c[1].data[:5]; return c },
		"short-frame":         func(c []testWebPChunk) []testWebPChunk { c[2].data = c[2].data[:15]; return c },
		"frame-outside":       func(c []testWebPChunk) []testWebPChunk { c[2].data[0] = 1; return c },
		"frame-size-mismatch": func(c []testWebPChunk) []testWebPChunk { c[2].data[6] = 1; return c },
		"missing-bitstream":   func(c []testWebPChunk) []testWebPChunk { c[2].data = c[2].data[:16]; return c },
		"bad-inner-padding":   func(c []testWebPChunk) []testWebPChunk { c[2].data[len(c[2].data)-1] = 1; return c },
		"bad-inner-length": func(c []testWebPChunk) []testWebPChunk {
			binary.LittleEndian.PutUint32(c[2].data[20:], 0xffffffff)
			return c
		},
		"duplicate-bitstream": func(c []testWebPChunk) []testWebPChunk {
			c[2].data = append(c[2].data, c[2].data[16:]...)
			return c
		},
		"unknown-before-bitstream": func(c []testWebPChunk) []testWebPChunk {
			unknown := encodeWebP([]testWebPChunk{{"JUNK", nil}})[12:]
			c[2].data = append(c[2].data[:16], append(unknown, c[2].data[16:]...)...)
			return c
		},
		"late-profile": func(c []testWebPChunk) []testWebPChunk { return append(c, testWebPChunk{"ICCP", nil}) },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			result := inspectFixture(t, encodeWebP(change(webpChunks(base))), "bad.webp", DefaultInspectPolicy())
			if result.Status != "invalid" || result.ErrorCode == nil || *result.ErrorCode != "INVALID_MEDIA" || result.Width != nil || result.Animated != nil || result.FrameCount != nil || result.DurationMS != nil {
				t.Fatal("malformed WebP gained trusted metadata", name, result)
			}
		})
	}
	for _, name := range []string{"lossy", "lossless"} {
		for _, change := range []string{"duplicate", "signature", "empty", "version", "entropy"} {
			chunks := webpChunks(webpFixture(t, name))
			switch change {
			case "duplicate":
				chunks = append(chunks, chunks[0])
			case "signature":
				chunks[0].data[0] ^= 1
			case "empty":
				chunks[0].data = nil
			case "version":
				if name == "lossless" {
					chunks[0].data[4] |= 0xe0
				} else {
					chunks[0].data[0] |= 8
				}
			case "entropy":
				length := 10
				if name == "lossless" {
					length = 5
				}
				chunks[0].data = chunks[0].data[:length]
			}
			result := inspectFixture(t, encodeWebP(chunks), "bad.webp", DefaultInspectPolicy())
			if result.Status != "invalid" || result.Width != nil || result.FrameCount != nil {
				t.Fatal(name, change, result)
			}
		}
	}
	for _, change := range []string{"declared-size", "trailing", "padding", "truncated"} {
		data := webpFixture(t, "lossless")
		switch change {
		case "declared-size":
			binary.LittleEndian.PutUint32(data[4:8], 0xffffffff)
		case "trailing":
			data = append(data, 0, 0)
		case "padding":
			data[len(data)-1] = 1
		case "truncated":
			data = data[:len(data)-1]
		}
		result := inspectFixture(t, data, "bad.webp", DefaultInspectPolicy())
		if result.Status != "invalid" || result.Width != nil || result.FrameCount != nil {
			t.Fatal(change, result)
		}
	}
	for _, change := range []string{"canvas", "profile", "exif", "alpha-lossless", "duplicate-exif"} {
		chunks := extendedWebP(t, "lossless", 0)
		switch change {
		case "canvas":
			chunks[0].data[4] = 3
		case "profile":
			chunks[0].data[0] |= 32
		case "exif":
			chunks[0].data[0] |= 8
		case "alpha-lossless":
			chunks = append(chunks[:1], append([]testWebPChunk{{"ALPH", []byte{0}}}, chunks[1:]...)...)
		case "duplicate-exif":
			exif := exifBytes(binary.LittleEndian, 6, "2026:11:05 12:34:56\x00", "+08:00\x00")
			chunks = append(chunks, testWebPChunk{"EXIF", exif}, testWebPChunk{"EXIF", exif})
		}
		result := inspectFixture(t, encodeWebP(chunks), "bad.webp", DefaultInspectPolicy())
		if result.Status != "invalid" || result.Width != nil || result.Orientation != nil {
			t.Fatal(change, result)
		}
	}
}

func TestWebPMetadataResourceLimitsAndCancellation(t *testing.T) {
	base := webpFixture(t, "animated")
	for _, limit := range []string{"frames", "duration", "pixels", "bytes", "exif"} {
		policy := DefaultInspectPolicy()
		chunks := webpChunks(base)
		switch limit {
		case "frames":
			policy.MaxFrames = 1
		case "duration":
			policy.MaxDuration = 199 * time.Millisecond
		case "pixels":
			policy.MaxPixels = 5
		case "bytes":
			policy.HeaderBytes = 512
			chunks = append(chunks, testWebPChunk{"JUNK", make([]byte, 600)})
		case "exif":
			chunks = append(chunks, testWebPChunk{"EXIF", make([]byte, (64<<10)+1)})
		}
		result := inspectFixture(t, encodeWebP(chunks), "limited.webp", policy)
		if result.Status != "invalid" || result.ErrorCode == nil || *result.ErrorCode != "MEDIA_LIMIT" || result.Width != nil || result.DurationMS != nil || result.FrameCount != nil {
			t.Fatal("WebP resource limit bypassed", limit, result)
		}
	}
	policy := DefaultInspectPolicy()
	policy.MaxFrames, policy.MaxDuration, policy.HeaderBytes = 2, 200*time.Millisecond, 512
	chunks := webpChunks(base)
	chunks = append(chunks, testWebPChunk{"JUNK", make([]byte, 512-len(base)-8)})
	data := encodeWebP(chunks)
	result := inspectFixture(t, data, "boundary.webp", policy)
	if len(data) != 512 || result.Status != "ok" || result.DurationMS == nil || *result.DurationMS != 200 {
		t.Fatal("inclusive WebP limits rejected valid evidence", len(data), result)
	}
	ctx, cancel := context.WithCancel(t.Context())
	_, err := inspectWebP(ctx, &cancelGIFReader{Reader: bytes.NewReader(base), cancel: cancel}, int64(len(base)), DefaultInspectPolicy())
	if !errors.Is(err, context.Canceled) {
		t.Fatal("WebP parser ignored cancellation", err)
	}
}

func FuzzBoundedWebPMetadata(f *testing.F) {
	f.Add(webpFixture(f, "lossy"))
	f.Add(webpFixture(f, "lossless"))
	f.Add(webpFixture(f, "animated"))
	f.Add([]byte("RIFF\x04\x00\x00\x00WEBP"))
	policy := DefaultInspectPolicy()
	policy.HeaderBytes, policy.MaxFrames, policy.MaxDuration = 65536, 64, time.Minute
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 65536 {
			data = data[:65536]
		}
		result, err := inspectWebP(t.Context(), bytes.NewReader(data), int64(len(data)), policy)
		if err != nil && !errors.Is(err, errWebP) && !errors.Is(err, errEXIF) && !errors.Is(err, errMetadataLimit) && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatal(err)
		}
		if err == nil && (result.width < 1 || result.height < 1 || result.frames < 1 || result.frames > 64 || result.loops < 0 || result.loops > 65535 || result.duration != nil && (*result.duration < 1 || *result.duration > time.Minute.Milliseconds())) {
			t.Fatal("unbounded/invalid WebP metadata", result)
		}
	})
}
