package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

func avifFixture(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("../../tests/fixtures/avif.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]string
	if json.Unmarshal(data, &fixtures) != nil {
		t.Fatal("invalid owned AVIF vectors")
	}
	decoded, err := base64.StdEncoding.DecodeString(fixtures[name])
	if err != nil || len(decoded) == 0 {
		t.Fatal("missing AVIF vector", name, err)
	}
	return decoded
}

func TestAVIFStaticAnimationExifAndTransformsUseActualBoundedEvidence(t *testing.T) {
	for _, item := range []struct {
		name                                 string
		frames, loops, duration, orientation int64
		capture                              bool
	}{
		{"still", 1, 1, 0, 0, false}, {"animated", 2, 2, 400, 0, false}, {"infinite", 2, 0, 400, 0, false}, {"once", 2, 1, 400, 0, false},
		{"exif", 1, 1, 0, 6, true}, {"rotated", 1, 1, 0, 6, true}, {"mirrored", 1, 1, 0, 5, true},
	} {
		t.Run(item.name, func(t *testing.T) {
			result := inspectFixture(t, avifFixture(t, item.name), "misleading.mov", DefaultInspectPolicy())
			if result.Status != "ok" || result.Kind != "image" || result.MIME != "image/avif" || result.Format != "avif" || result.Codec == nil || *result.Codec != "av1" || result.Width == nil || *result.Width != 16 || result.Height == nil || *result.Height != 12 || result.Animated == nil || *result.Animated != (item.frames > 1) || result.FrameCount == nil || *result.FrameCount != item.frames || result.LoopCount == nil || *result.LoopCount != item.loops {
				t.Fatal("AVIF metadata was lost or invented", result)
			}
			if item.duration == 0 && result.DurationMS != nil || item.duration > 0 && (result.DurationMS == nil || *result.DurationMS != item.duration) {
				t.Fatal("AVIF cycle timing was guessed", result)
			}
			if item.orientation == 0 && result.Orientation != nil || item.orientation > 0 && (result.Orientation == nil || *result.Orientation != item.orientation) {
				t.Fatal("ordered AVIF transforms were misreported", result)
			}
			if item.capture && (result.CaptureAt == nil || result.CaptureAt.Format(time.RFC3339Nano) != "2026-11-05T04:34:56.123Z") || !item.capture && result.CaptureAt != nil {
				t.Fatal("AVIF original clock was guessed or not normalized", result)
			}
		})
	}
}

func TestAVIFGenericHEIFBrandIsRecognizedWithoutFalseCapability(t *testing.T) {
	data := avifFixture(t, "still")
	copy(data[8:12], "mif1")
	result := inspectFixture(t, data, "misleading.heic", DefaultInspectPolicy())
	if result.Status != "unsupported" || result.MIME != "image/avif" || result.Codec != nil || result.ErrorCode == nil || *result.ErrorCode != "CODEC_UNAVAILABLE" {
		t.Fatal("generic brand was mislabeled or claimed unsupported decoder capability", result)
	}
}

func TestAVIFResourceAndUnavailableCapabilityFailWithoutTrustedFields(t *testing.T) {
	for name, mutate := range map[string]func(*InspectPolicy){
		"pixels":         func(p *InspectPolicy) { p.MaxPixels = 191 },
		"frames":         func(p *InspectPolicy) { p.MaxFrames = 1 },
		"duration":       func(p *InspectPolicy) { p.MaxDuration = 399 * time.Millisecond },
		"metadata bytes": func(p *InspectPolicy) { p.HeaderBytes = 512 },
	} {
		t.Run(name, func(t *testing.T) {
			policy := DefaultInspectPolicy()
			mutate(&policy)
			result := inspectFixture(t, avifFixture(t, "animated"), "image.avif", policy)
			if result.Status != "invalid" || result.ErrorCode == nil || *result.ErrorCode != "MEDIA_LIMIT" || result.Codec != nil || result.Width != nil || result.FrameCount != nil || result.CaptureAt != nil {
				t.Fatal("AVIF budget failed open", result)
			}
		})
	}
	policy := DefaultInspectPolicy()
	policy.codecRuntime = "unavailable"
	result := inspectFixture(t, avifFixture(t, "still"), "image.avif", policy)
	if result.Status != "unsupported" || result.ErrorCode == nil || *result.ErrorCode != "CODEC_UNAVAILABLE" || result.Width != nil || result.FrameCount != nil {
		t.Fatal("missing AVIF codec fell back to headers", result)
	}
}

func TestAVIFTruncatedLengthsAndCancelledMetadataNeverPublish(t *testing.T) {
	data := avifFixture(t, "animated")
	for length := range len(data) {
		if _, err := parseAVIF(t.Context(), bytes.NewReader(data[:length]), int64(length), DefaultInspectPolicy()); err == nil {
			t.Fatal("truncated AVIF accepted", length)
		}
	}
	for _, size := range []uint32{1, 7, 0xffffffff} {
		changed := bytes.Clone(data)
		binary.BigEndian.PutUint32(changed[:4], size)
		if _, err := parseAVIF(t.Context(), bytes.NewReader(changed), int64(len(changed)), DefaultInspectPolicy()); err == nil {
			t.Fatal("invalid root box size accepted", size)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := parseAVIF(ctx, bytes.NewReader(data), int64(len(data)), DefaultInspectPolicy()); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled AVIF metadata continued", err)
	}
}

func avifTestBox(t *testing.T, data []byte, path ...string) avifBox {
	t.Helper()
	r := &avifReader{ctx: t.Context()}
	boxes, err := r.children(data, 0)
	if err != nil {
		t.Fatal(err)
	}
	var selected avifBox
	for index, kind := range path {
		var found bool
		for _, box := range boxes {
			if box.kind == kind {
				selected, found = box, true
				break
			}
		}
		if !found {
			t.Fatal("owned fixture is missing box", path)
		}
		if index == len(path)-1 {
			return selected
		}
		skip := 0
		switch kind {
		case "meta", "iref":
			skip = 4
		case "iinf":
			skip = 6
		}
		boxes, err = r.children(selected.body[skip:], selected.offset+int64(skip))
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("empty box path")
	return avifBox{}
}

func TestAVIFMetadataAndSampleTableMutationsFailClosed(t *testing.T) {
	for name, mutate := range map[string]func(*testing.T, []byte){
		"primary": func(t *testing.T, d []byte) {
			b := avifTestBox(t, d, "meta", "pitm")
			binary.BigEndian.PutUint16(b.body[4:], 99)
		},
		"item offset": func(t *testing.T, d []byte) {
			b := avifTestBox(t, d, "meta", "iloc")
			binary.BigEndian.PutUint32(b.body[14:], 0xffffffff)
		},
		"outside media": func(t *testing.T, d []byte) {
			b := avifTestBox(t, d, "meta", "iloc")
			binary.BigEndian.PutUint32(b.body[14:], 8)
		},
		"property association": func(t *testing.T, d []byte) { b := avifTestBox(t, d, "meta", "iprp", "ipma"); b.body[11] = 0x7f },
		"zero geometry": func(t *testing.T, d []byte) {
			b := avifTestBox(t, d, "meta", "iprp", "ipco", "ispe")
			binary.BigEndian.PutUint32(b.body[4:], 0)
		},
		"AV1 configuration": func(t *testing.T, d []byte) { b := avifTestBox(t, d, "meta", "iprp", "ipco", "av1C"); b.body[0] = 0 },
		"zero media clock": func(t *testing.T, d []byte) {
			b := avifTestBox(t, d, "moov", "trak", "mdia", "mdhd")
			binary.BigEndian.PutUint32(b.body[12:], 0)
		},
		"sample count": func(t *testing.T, d []byte) {
			b := avifTestBox(t, d, "moov", "trak", "mdia", "minf", "stbl", "stsz")
			binary.BigEndian.PutUint32(b.body[8:], 1)
		},
		"sample timing": func(t *testing.T, d []byte) {
			b := avifTestBox(t, d, "moov", "trak", "mdia", "minf", "stbl", "stts")
			binary.BigEndian.PutUint32(b.body[8:], 1)
		},
		"chunk offset": func(t *testing.T, d []byte) {
			b := avifTestBox(t, d, "moov", "trak", "mdia", "minf", "stbl", "stco")
			binary.BigEndian.PutUint32(b.body[8:], 8)
		},
		"chunk mapping": func(t *testing.T, d []byte) {
			b := avifTestBox(t, d, "moov", "trak", "mdia", "minf", "stbl", "stsc")
			binary.BigEndian.PutUint32(b.body[12:], 0)
		},
		"edit flags": func(t *testing.T, d []byte) { b := avifTestBox(t, d, "moov", "trak", "edts", "elst"); b.body[3] = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			data := avifFixture(t, "animated")
			mutate(t, data)
			result := inspectFixture(t, data, "bad.avif", DefaultInspectPolicy())
			if result.Status != "invalid" || result.Codec != nil || result.Width != nil || result.FrameCount != nil || result.ErrorCode == nil || *result.ErrorCode != "INVALID_MEDIA" {
				t.Fatal("malformed AVIF retained trusted metadata", result)
			}
		})
	}
	for _, path := range [][]string{{"meta", "iloc"}, {"meta", "iprp", "ipco", "av1C"}, {"moov", "trak", "mdia", "minf", "dinf", "dref"}} {
		data := avifFixture(t, "animated")
		b := avifTestBox(t, data, path...)
		switch path[len(path)-1] {
		case "iloc":
			binary.BigEndian.PutUint16(b.body[10:], 1)
		case "av1C":
			copy(data[b.offset-4:b.offset], "zzzz")
		case "dref":
			b.body[len(b.body)-1] = 0
		}
		result := inspectFixture(t, data, "unsupported.avif", DefaultInspectPolicy())
		if result.Status != "unsupported" || result.Codec != nil || result.ErrorCode == nil || *result.ErrorCode != "CODEC_UNAVAILABLE" {
			t.Fatal("unsupported AVIF authority fell back", path, result)
		}
	}
}

func TestAVIFUnknownTimingAndNonessentialPropertiesAreNotGuessed(t *testing.T) {
	data := avifFixture(t, "animated")
	timing := avifTestBox(t, data, "moov", "trak", "mdia", "minf", "stbl", "stts")
	binary.BigEndian.PutUint32(timing.body[12:], 0)
	clock := avifTestBox(t, data, "moov", "trak", "mdia", "mdhd")
	binary.BigEndian.PutUint32(clock.body[16:], 0)
	value, err := parseAVIF(t.Context(), bytes.NewReader(data), int64(len(data)), DefaultInspectPolicy())
	if err != nil || value.frames != 2 || value.duration != nil || value.loopKnown {
		t.Fatal("missing timing fabricated duration or repeats", value, err)
	}
	data = avifFixture(t, "still")
	colour := avifTestBox(t, data, "meta", "iprp", "ipco", "colr")
	copy(data[colour.offset-4:colour.offset], "zzzz")
	if _, err := parseAVIF(t.Context(), bytes.NewReader(data), int64(len(data)), DefaultInspectPolicy()); err != nil {
		t.Fatal("unknown nonessential property rejected", err)
	}
}

func FuzzBoundedAVIFMetadata(f *testing.F) {
	for _, name := range []string{"still", "animated", "exif", "mirrored"} {
		f.Add(avifFixture(f, name))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 65536 {
			return
		}
		policy := DefaultInspectPolicy()
		policy.HeaderBytes = 65536
		value, err := parseAVIF(t.Context(), bytes.NewReader(data), int64(len(data)), policy)
		if err == nil && (value.width < 1 || value.height < 1 || value.width > int64(policy.MaxDimension) || value.height > int64(policy.MaxDimension) || value.width > policy.MaxPixels/value.height || value.frames < 1 || value.frames > int64(policy.MaxFrames) || value.capture != nil && value.capture.Nanosecond()%1000000 != 0) {
			t.Fatal("unbounded AVIF metadata accepted", value)
		}
	})
}
