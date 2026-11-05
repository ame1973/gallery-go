package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"io"
	"strings"
	"testing"
	"time"
)

func gifFixture(t testing.TB, delays []int, loops int) []byte {
	t.Helper()
	palette := color.Palette{color.Black, color.White}
	animation := gif.GIF{Delay: delays, LoopCount: loops, Config: image.Config{ColorModel: palette, Width: 3, Height: 2}}
	for index := range delays {
		frame := image.NewPaletted(image.Rect(0, 0, 3, 2), palette)
		for pixel := range frame.Pix {
			frame.Pix[pixel] = uint8(index % 2)
		}
		animation.Image = append(animation.Image, frame)
	}
	var encoded bytes.Buffer
	if err := gif.EncodeAll(&encoded, &animation); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func insertGIFBlock(data []byte, offset int, block []byte) []byte {
	result := make([]byte, 0, len(data)+len(block))
	result = append(result, data[:offset]...)
	result = append(result, block...)
	return append(result, data[offset:]...)
}

func TestGIFMetadataCountsFramesNormalizesLoopsAndPreservesUnknownTiming(t *testing.T) {
	for _, test := range []struct {
		name   string
		delays []int
		repeat int
		loops  int64
		known  bool
	}{
		{"static", []int{0}, -1, 1, false},
		{"timed-static", []int{10}, -1, 1, true},
		{"once", []int{10, 20}, -1, 1, true},
		{"infinite", []int{10, 20}, 0, 0, true},
		{"finite", []int{10, 20}, 2, 3, true},
		{"maximum-repeat", []int{10, 20}, 65535, 65536, true},
		{"missing-first-delay", []int{0, 20}, 0, 0, false},
		{"missing-later-delay", []int{10, 0}, -1, 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := gifFixture(t, test.delays, test.repeat)
			result := inspectFixture(t, input, "misleading.jpg", DefaultInspectPolicy())
			if result.Status != "ok" || result.Format != "gif" || result.Animated == nil || *result.Animated != (len(test.delays) > 1) || result.FrameCount == nil || *result.FrameCount != int64(len(test.delays)) || result.LoopCount == nil || *result.LoopCount != test.loops || result.Orientation != nil || result.CaptureAt != nil {
				t.Fatal(result)
			}
			var duration int64
			for _, delay := range test.delays {
				duration += int64(delay) * 10
			}
			if test.known && (result.DurationMS == nil || *result.DurationMS != duration) || !test.known && result.DurationMS != nil {
				t.Fatal("cycle duration was multiplied by loops or missing delays were guessed", result)
			}
			decoded, err := gif.DecodeAll(bytes.NewReader(input))
			if err != nil || len(decoded.Image) != len(test.delays) {
				t.Fatal("generated GIF fixture does not actually decode", err)
			}
		})
	}
	data := gifFixture(t, []int{10, 20}, 2)
	data = bytes.Replace(data, []byte("NETSCAPE2.0"), []byte("ANIMEXTS1.0"), 1)
	result := inspectFixture(t, data, "animation.gif", DefaultInspectPolicy())
	if result.Status != "ok" || result.LoopCount == nil || *result.LoopCount != 3 {
		t.Fatal("alternate defined loop extension was lost", result)
	}
}

func TestGIFMetadataPortableContainerFixtureActuallyDecodes(t *testing.T) {
	// The container harness constructs this tiny two-color GIF using only Python stdlib.
	data, err := hex.DecodeString("47494638396101000100800000000000ffffff21ff0b4e45545343415045322e30030102000021f904000a0000002c000000000100010000020244010021f90400140000002c00000000010001000002024c01003b")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil || len(decoded.Image) != 2 || decoded.Image[0].Pix[0] != 0 || decoded.Image[1].Pix[0] != 1 || decoded.Delay[0] != 10 || decoded.Delay[1] != 20 || decoded.LoopCount != 2 {
		t.Fatal("portable container fixture is not valid distinct GIF frames", decoded, err)
	}
	result := inspectFixture(t, data, "portable.gif", DefaultInspectPolicy())
	if result.Status != "ok" || result.Animated == nil || !*result.Animated || result.FrameCount == nil || *result.FrameCount != 2 || result.DurationMS == nil || *result.DurationMS != 300 || result.LoopCount == nil || *result.LoopCount != 3 {
		t.Fatal("portable fixture metadata disagrees with actual decoding", result)
	}
}

func TestGIFMetadataDoesNotInheritZeroDelaysOrExposePrivateExtensions(t *testing.T) {
	data := gifFixture(t, []int{0, 20}, -1)
	imageAt := bytes.IndexByte(data, 0x2c)
	zeroControl := []byte{0x21, 0xf9, 4, 0, 0, 0, 0, 0}
	data = insertGIFBlock(data, imageAt, zeroControl)
	result := inspectFixture(t, data, "animation.gif", DefaultInspectPolicy())
	if result.Status != "ok" || result.DurationMS != nil || result.FrameCount == nil || *result.FrameCount != 2 {
		t.Fatal("explicit zero delay was assigned a browser clamp", result)
	}
	data = gifFixture(t, []int{10, 20}, 0)
	controlAt := bytes.Index(data, []byte{0x21, 0xf9})
	data[controlAt+3] |= 2
	result = inspectFixture(t, data, "animation.gif", DefaultInspectPolicy())
	if result.Status != "ok" || result.DurationMS != nil {
		t.Fatal("user-input timing was presented as a known duration", result)
	}
	data = gifFixture(t, []int{10, 20}, 0)
	private := []byte("GPS /private/source/camera-secret")
	comment := append([]byte{0x21, 0xfe, byte(len(private))}, private...)
	comment = append(comment, 0)
	application := append([]byte{0x21, 0xff, 11}, []byte("PRIVATEAPP1")...)
	application = append(application, byte(len(private)))
	application = append(application, private...)
	application = append(application, 0)
	data = insertGIFBlock(data, bytes.IndexByte(data, 0x2c), append(comment, application...))
	result = inspectFixture(t, data, "animation.gif", DefaultInspectPolicy())
	encoded, err := json.Marshal(result)
	if err != nil || result.Status != "ok" || result.DurationMS == nil || *result.DurationMS != 300 || strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "camera-secret") {
		t.Fatal("comments/applications changed timing or leaked data", result, string(encoded), err)
	}
}

func TestGIFMetadataRejectsMalformedAmbiguousAndUnsupportedContainers(t *testing.T) {
	base := gifFixture(t, []int{10, 20}, 0)
	imageAt, controlAt := bytes.IndexByte(base, 0x2c), bytes.Index(base, []byte{0x21, 0xf9})
	loopAt := bytes.Index(base, []byte{0x21, 0xff, 11})
	if imageAt < 0 || controlAt < 0 || loopAt < 0 || base[imageAt+9] != 0 {
		t.Fatal("unexpected fixture layout")
	}
	changes := map[string]func([]byte) []byte{
		"missing-trailer":       func(d []byte) []byte { return d[:len(d)-1] },
		"trailing-stream":       func(d []byte) []byte { return append(d, d...) },
		"truncated-descriptor":  func(d []byte) []byte { return d[:imageAt+3] },
		"truncated-palette":     func(d []byte) []byte { return d[:15] },
		"control-size":          func(d []byte) []byte { d[controlAt+2] = 3; return d },
		"control-terminator":    func(d []byte) []byte { d[controlAt+7] = 1; return d },
		"control-reserved":      func(d []byte) []byte { d[controlAt+3] = 0x80; return d },
		"control-disposal":      func(d []byte) []byte { d[controlAt+3] = 0x10; return d },
		"duplicate-control":     func(d []byte) []byte { return insertGIFBlock(d, controlAt, d[controlAt:controlAt+8]) },
		"dangling-control":      func(d []byte) []byte { return insertGIFBlock(d, len(d)-1, d[controlAt:controlAt+8]) },
		"duplicate-loop":        func(d []byte) []byte { return insertGIFBlock(d, loopAt, d[loopAt:loopAt+19]) },
		"late-loop":             func(d []byte) []byte { return insertGIFBlock(d, len(d)-1, d[loopAt:loopAt+19]) },
		"loop-size":             func(d []byte) []byte { d[loopAt+14] = 4; return d },
		"loop-code":             func(d []byte) []byte { d[loopAt+15] = 2; return d },
		"loop-terminator":       func(d []byte) []byte { d[loopAt+18] = 1; return d },
		"application-size":      func(d []byte) []byte { d[loopAt+2] = 0; return d },
		"frame-width-zero":      func(d []byte) []byte { binary.LittleEndian.PutUint16(d[imageAt+5:], 0); return d },
		"frame-outside-canvas":  func(d []byte) []byte { binary.LittleEndian.PutUint16(d[imageAt+1:], 65535); return d },
		"frame-reserved":        func(d []byte) []byte { d[imageAt+9] = 0x18; return d },
		"lzw-width":             func(d []byte) []byte { d[imageAt+10] = 9; return d },
		"missing-image-data":    func(d []byte) []byte { d[imageAt+11] = 0; return d },
		"truncated-image-block": func(d []byte) []byte { d[imageAt+11] = 255; return d },
		"unknown-block":         func(d []byte) []byte { return insertGIFBlock(d, len(d)-1, []byte{0x7f}) },
		"no-frames":             func(d []byte) []byte { return append(d[:controlAt], 0x3b) },
		"no-color-table": func(d []byte) []byte {
			d[10] &^= 0x80
			return append(d[:13], d[19:]...)
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			data := change(append([]byte{}, base...))
			result := inspectFixture(t, data, "animation.gif", DefaultInspectPolicy())
			if result.Status != "invalid" || result.ErrorCode == nil || *result.ErrorCode != "INVALID_MEDIA" || result.Width != nil || result.Height != nil || result.Animated != nil || result.FrameCount != nil || result.LoopCount != nil || result.DurationMS != nil {
				t.Fatal("malformed GIF gained trusted metadata", result)
			}
		})
	}
	for _, label := range []byte{0x01, 0x7e} {
		data := insertGIFBlock(base, imageAt, []byte{0x21, label})
		result := inspectFixture(t, data, "animation.gif", DefaultInspectPolicy())
		if result.Status != "unsupported" || result.ErrorCode == nil || *result.ErrorCode != "CODEC_UNAVAILABLE" || result.Animated != nil || result.FrameCount != nil || result.Width != nil {
			t.Fatal("unsupported rendering extension became trusted animation", result)
		}
	}
}

func TestGIFMetadataResourceLimitsAndCancellationFailClosed(t *testing.T) {
	base := gifFixture(t, []int{10, 20}, 0)
	for _, limit := range []string{"frames", "duration", "bytes", "unknown-duration"} {
		t.Run(limit, func(t *testing.T) {
			policy := DefaultInspectPolicy()
			data := base
			switch limit {
			case "frames":
				policy.MaxFrames = 1
			case "duration":
				policy.MaxDuration = 250 * time.Millisecond
			case "bytes":
				policy.HeaderBytes = 512
				comment := append([]byte{0x21, 0xfe}, bytes.Repeat(append([]byte{255}, bytes.Repeat([]byte{'x'}, 255)...), 3)...)
				comment = append(comment, 0)
				data = insertGIFBlock(data, bytes.IndexByte(data, 0x2c), comment)
			case "unknown-duration":
				policy.MaxDuration = 250 * time.Millisecond
				data = gifFixture(t, []int{0, 30}, 0)
			}
			result := inspectFixture(t, data, "animation.gif", policy)
			if result.Status != "invalid" || result.ErrorCode == nil || *result.ErrorCode != "MEDIA_LIMIT" || result.Animated != nil || result.DurationMS != nil || result.FrameCount != nil {
				t.Fatal("animation resource limit bypassed", limit, result)
			}
		})
	}
	policy := DefaultInspectPolicy()
	policy.MaxFrames, policy.MaxDuration, policy.HeaderBytes = 2, 300*time.Millisecond, 512
	// Two comment sub-blocks pad a real complete GIF to the exact allowed input size.
	padding := 512 - len(base) - 5
	if padding <= 255 || padding > 510 {
		t.Fatal("unexpected fixture size", len(base))
	}
	comment := append([]byte{0x21, 0xfe, 255}, bytes.Repeat([]byte{'x'}, 255)...)
	comment = append(comment, byte(padding-255))
	comment = append(comment, bytes.Repeat([]byte{'x'}, padding-255)...)
	comment = append(comment, 0)
	atBoundary := insertGIFBlock(base, bytes.IndexByte(base, 0x2c), comment)
	result := inspectFixture(t, atBoundary, "boundary.gif", policy)
	if len(atBoundary) != 512 || result.Status != "ok" || result.DurationMS == nil || *result.DurationMS != 300 {
		t.Fatal("inclusive GIF byte/frame/duration limits rejected valid evidence", len(atBoundary), result)
	}
	ctx, cancel := context.WithCancel(t.Context())
	reader := &cancelGIFReader{Reader: bytes.NewReader(base), cancel: cancel}
	_, err := inspectGIF(ctx, reader, int64(len(base)), DefaultInspectPolicy())
	if !errors.Is(err, context.Canceled) {
		t.Fatal("GIF input ignored cancellation", err)
	}
}

type cancelGIFReader struct {
	io.Reader
	cancel context.CancelFunc
}

func (r *cancelGIFReader) Read(data []byte) (int, error) {
	n, err := r.Reader.Read(data)
	r.cancel()
	return n, err
}

func FuzzBoundedGIFMetadata(f *testing.F) {
	f.Add(gifFixture(f, []int{10, 20}, 0))
	f.Add(gifFixture(f, []int{0}, -1))
	f.Add([]byte("GIF89a"))
	policy := DefaultInspectPolicy()
	policy.HeaderBytes, policy.MaxFrames, policy.MaxDuration = 65536, 64, time.Minute
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 65536 {
			data = data[:65536]
		}
		result, err := inspectGIF(t.Context(), bytes.NewReader(data), int64(len(data)), policy)
		if err != nil && !errors.Is(err, errGIF) && !errors.Is(err, errGIFUnsupported) && !errors.Is(err, errMetadataLimit) && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatal(err)
		}
		if err == nil && (result.frames < 1 || result.frames > 64 || result.loops < 0 || result.loops > 65536 || result.duration != nil && (*result.duration < 1 || *result.duration > time.Minute.Milliseconds())) {
			t.Fatal("invalid or unbounded GIF metadata", result)
		}
	})
}
