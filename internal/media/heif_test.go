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

func heifFixture(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("../../tests/fixtures/heif.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]string
	if json.Unmarshal(data, &fixtures) != nil {
		t.Fatal("invalid owned HEIF vectors")
	}
	decoded, err := base64.StdEncoding.DecodeString(fixtures[name])
	if err != nil || len(decoded) == 0 {
		t.Fatal("missing owned HEIF vector", name, err)
	}
	return decoded
}

func TestHEIFPrimaryGridTransformsAndEXIFUseTheMandatoryImageProfile(t *testing.T) {
	for _, name := range []string{"still", "generic", "grid", "exif", "rotated", "mirrored", "collection"} {
		t.Run(name, func(t *testing.T) {
			result := inspectFixture(t, heifFixture(t, name), "misleading.avif", DefaultInspectPolicy())
			width, height := int64(64), int64(64)
			if name == "grid" || name == "rotated" || name == "mirrored" {
				width = 96
			}
			if result.Status != "ok" || result.Kind != "image" || result.MIME != "image/heif" || result.Format != "heif" || result.Codec == nil || *result.Codec != "hevc" || result.Width == nil || *result.Width != width || result.Height == nil || *result.Height != height || result.Animated == nil || *result.Animated || result.FrameCount == nil || *result.FrameCount != 1 || result.LoopCount == nil || *result.LoopCount != 1 || result.DurationMS != nil {
				t.Fatal("HEIF primary/canvas metadata lost or guessed", name, result)
			}
			orientation := int64(0)
			switch name {
			case "exif":
				orientation = 3
			case "rotated":
				orientation = 6
			case "mirrored":
				orientation = 5
			}
			if orientation == 0 && result.Orientation != nil || orientation != 0 && (result.Orientation == nil || *result.Orientation != orientation) {
				t.Fatal("HEIF item transforms lost precedence over EXIF", result)
			}
			if name == "exif" || name == "mirrored" {
				if result.CaptureAt == nil || result.CaptureAt.Format(time.RFC3339Nano) != "2026-11-05T04:34:56.123Z" {
					t.Fatal("HEIF original explicit-zone millisecond evidence lost", result)
				}
			} else if result.CaptureAt != nil {
				t.Fatal("HEIF invented a capture clock", result)
			}
		})
	}
}

func TestHEIFMissingRuntimeAndSequenceProfilesNeverFallBackToNativeHeaders(t *testing.T) {
	policy, err := DefaultInspectPolicy().PinCodecRuntime(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	policy.imageRuntime = "unavailable"
	result := inspectFixture(t, heifFixture(t, "still"), "image.heic", policy)
	if result.Status != "unsupported" || result.Codec != nil || result.Width != nil || result.CaptureAt != nil || result.ErrorCode == nil || *result.ErrorCode != "CODEC_UNAVAILABLE" {
		t.Fatal("HEIF native headers became capability fallback", result)
	}
	for _, brand := range []string{"hevc", "hevx", "msf1"} {
		data := heifFixture(t, "still")
		copy(data[8:12], brand)
		result := inspectFixture(t, data, "sequence.heif", DefaultInspectPolicy())
		if result.Status != "unsupported" || result.Width != nil || result.FrameCount != nil || result.Codec != nil || result.ErrorCode == nil || *result.ErrorCode != "CODEC_UNAVAILABLE" {
			t.Fatal("sequence brand fabricated one still frame", brand, result)
		}
	}
	data := heifFixture(t, "still")
	movie := avifTestBox(t, avifFixture(t, "animated"), "moov")
	data = append(data, binary.BigEndian.AppendUint32(nil, uint32(len(movie.body)+8))...)
	data = append(data, "moov"...)
	data = append(data, movie.body...)
	if _, err := parseHEIF(t.Context(), bytes.NewReader(data), int64(len(data)), DefaultInspectPolicy()); !errors.Is(err, errAVIFUnsupported) {
		t.Fatal("HEIF movie graph was interpreted as a static item", err)
	}
}

func TestHEIFResourceTruncationAndCancellationBoundaries(t *testing.T) {
	for name, change := range map[string]func(*InspectPolicy){
		"pixels":    func(policy *InspectPolicy) { policy.MaxPixels = 4095 },
		"dimension": func(policy *InspectPolicy) { policy.MaxDimension = 63 },
		"bytes":     func(policy *InspectPolicy) { policy.HeaderBytes = 512 },
	} {
		t.Run(name, func(t *testing.T) {
			policy := DefaultInspectPolicy()
			change(&policy)
			result := inspectFixture(t, heifFixture(t, "mirrored"), "image.heif", policy)
			if result.Status != "invalid" || result.ErrorCode == nil || *result.ErrorCode != "MEDIA_LIMIT" || result.Width != nil || result.Codec != nil {
				t.Fatal("HEIF resource limit failed open", result)
			}
		})
	}
	data := heifFixture(t, "still")
	for length := range len(data) {
		if _, err := parseHEIF(t.Context(), bytes.NewReader(data[:length]), int64(length), DefaultInspectPolicy()); err == nil {
			t.Fatal("truncated HEIF accepted", length)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := parseHEIF(ctx, bytes.NewReader(data), int64(len(data)), DefaultInspectPolicy()); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled HEIF continued", err)
	}
}

func TestHEIFMalformedConfigurationReferencesAndNALHeadersFailClosed(t *testing.T) {
	for name, change := range map[string]func(*testing.T, []byte){
		"record version":           func(t *testing.T, data []byte) { avifTestBox(t, data, "meta", "iprp", "ipco", "hvcC").body[0] = 0 },
		"record NAL forbidden bit": func(t *testing.T, data []byte) { avifTestBox(t, data, "meta", "iprp", "ipco", "hvcC").body[28] |= 0x80 },
		"record temporal ID":       func(t *testing.T, data []byte) { avifTestBox(t, data, "meta", "iprp", "ipco", "hvcC").body[29] = 0 },
		"record NAL type":          func(t *testing.T, data []byte) { avifTestBox(t, data, "meta", "iprp", "ipco", "hvcC").body[28] = 66 },
		"record NAL length": func(t *testing.T, data []byte) {
			binary.BigEndian.PutUint16(avifTestBox(t, data, "meta", "iprp", "ipco", "hvcC").body[26:], 0xffff)
		},
		"record missing arrays": func(t *testing.T, data []byte) { avifTestBox(t, data, "meta", "iprp", "ipco", "hvcC").body[22] = 0 },
		"primary": func(t *testing.T, data []byte) {
			binary.BigEndian.PutUint16(avifTestBox(t, data, "meta", "pitm").body[4:], 99)
		},
		"external extent": func(t *testing.T, data []byte) {
			binary.BigEndian.PutUint32(avifTestBox(t, data, "meta", "iloc").body[14:], 0xffffffff)
		},
		"zero geometry": func(t *testing.T, data []byte) {
			binary.BigEndian.PutUint32(avifTestBox(t, data, "meta", "iprp", "ipco", "ispe").body[4:], 0)
		},
		"NAL length": func(t *testing.T, data []byte) {
			binary.BigEndian.PutUint32(avifTestBox(t, data, "mdat").body, 0xffffffff)
		},
		"empty NAL":           func(t *testing.T, data []byte) { binary.BigEndian.PutUint32(avifTestBox(t, data, "mdat").body, 0) },
		"NAL forbidden bit":   func(t *testing.T, data []byte) { avifTestBox(t, data, "mdat").body[4] |= 0x80 },
		"NAL temporal ID":     func(t *testing.T, data []byte) { avifTestBox(t, data, "mdat").body[5] = 0 },
		"missing first slice": func(t *testing.T, data []byte) { avifTestBox(t, data, "mdat").body[6] &^= 0x80 },
	} {
		t.Run(name, func(t *testing.T) {
			data := heifFixture(t, "still")
			change(t, data)
			result := inspectFixture(t, data, "bad.heic", DefaultInspectPolicy())
			if result.Status != "invalid" || result.ErrorCode == nil || *result.ErrorCode != "INVALID_MEDIA" || result.Width != nil || result.Codec != nil || result.FrameCount != nil || result.CaptureAt != nil {
				t.Fatal("malformed HEIF retained trusted fields", name, result)
			}
		})
	}
	for name, change := range map[string]func(*testing.T, []byte){
		"other coding": func(t *testing.T, data []byte) { copy(avifTestBox(t, data, "meta", "iinf", "infe").body[8:12], "av01") },
		"other decoder property": func(t *testing.T, data []byte) {
			property := avifTestBox(t, data, "meta", "iprp", "ipco", "hvcC")
			copy(data[property.offset-4:property.offset], "av1C")
		},
		"NAL width":         func(t *testing.T, data []byte) { avifTestBox(t, data, "meta", "iprp", "ipco", "hvcC").body[21] &^= 3 },
		"layered record":    func(t *testing.T, data []byte) { avifTestBox(t, data, "meta", "iprp", "ipco", "hvcC").body[29] |= 8 },
		"dependent picture": func(t *testing.T, data []byte) { avifTestBox(t, data, "mdat").body[4] = 0 },
		"layered picture":   func(t *testing.T, data []byte) { avifTestBox(t, data, "mdat").body[5] |= 8 },
		"external data": func(t *testing.T, data []byte) {
			binary.BigEndian.PutUint16(avifTestBox(t, data, "meta", "iloc").body[10:], 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			data := heifFixture(t, "still")
			change(t, data)
			result := inspectFixture(t, data, "unsupported.heif", DefaultInspectPolicy())
			if result.Status != "unsupported" || result.ErrorCode == nil || *result.ErrorCode != "CODEC_UNAVAILABLE" || result.Width != nil || result.Codec != nil {
				t.Fatal("unsupported HEIF codec authority fell back", name, result)
			}
		})
	}
}

func TestHEVCFramingCapsAndOnePictureDoNotCountSlicesAsFrames(t *testing.T) {
	nal := func(kind, first byte) []byte {
		return []byte{0, 0, 0, 3, kind << 1, 1, first}
	}
	first, slice, delimiter := nal(20, 0x80), nal(20, 0), nal(35, 0)
	check := func(data []byte) error {
		r := avifReader{ctx: t.Context(), file: bytes.NewReader(data), size: int64(len(data)), remaining: 1 << 20, hevc: true}
		return r.hevcItem(&avifItem{extents: []avifExtent{{0, int64(len(data))}}})
	}
	if err := check(append(bytes.Clone(first), slice...)); err != nil {
		t.Fatal("one picture's additional slice was counted as a second picture", err)
	}
	if err := check(append(bytes.Clone(first), first...)); !errors.Is(err, errAVIFUnsupported) {
		t.Fatal("multiple pictures accepted as one still", err)
	}
	if err := check(delimiter); !errors.Is(err, errAVIF) {
		t.Fatal("a delimiter fabricated a picture", err)
	}
	if err := check(append(bytes.Clone(first), 0)); !errors.Is(err, errAVIF) {
		t.Fatal("trailing incomplete NAL prefix accepted", err)
	}
	if err := check(append(bytes.Clone(first), bytes.Repeat(delimiter, 4096)...)); !errors.Is(err, errMetadataLimit) {
		t.Fatal("unbounded per-item NAL count", err)
	}
	r := avifReader{}
	if err := r.hevcItem(&avifItem{extents: []avifExtent{{0, 4}, {4, 4}}}); !errors.Is(err, errAVIFUnsupported) {
		t.Fatal("unsupported split coded extents were guessed", err)
	}
	if err := hevcConfiguration(make([]byte, 64<<10+1)); !errors.Is(err, errMetadataLimit) {
		t.Fatal("unbounded decoder record", err)
	}
	for _, field := range []string{"arrays", "NALs"} {
		data := bytes.Clone(avifTestBox(t, heifFixture(t, "still"), "meta", "iprp", "ipco", "hvcC").body)
		if field == "arrays" {
			data[22] = 17
		} else {
			binary.BigEndian.PutUint16(data[24:26], 65)
		}
		if err := hevcConfiguration(data); !errors.Is(err, errMetadataLimit) {
			t.Fatal("unbounded decoder-record array", field, err)
		}
	}
}

func TestItemImageNonessentialOtherCodecPropertiesRemainIgnorable(t *testing.T) {
	for _, hevc := range []bool{false, true} {
		data := avifFixture(t, "still")
		parse := parseAVIF
		kind := "hvcC"
		if hevc {
			data, parse, kind = heifFixture(t, "still"), parseHEIF, "av1C"
		}
		property := avifTestBox(t, data, "meta", "iprp", "ipco", "colr")
		copy(data[property.offset-4:property.offset], kind)
		if _, err := parse(t.Context(), bytes.NewReader(data), int64(len(data)), DefaultInspectPolicy()); err != nil {
			t.Fatal("irrelevant nonessential property changed primary coding", hevc, err)
		}
	}
}

func FuzzBoundedHEVCConfiguration(f *testing.F) {
	data := heifFixture(f, "still")
	position := bytes.Index(data, []byte("hvcC"))
	length := int(binary.BigEndian.Uint32(data[position-4:]))
	f.Add(data[position+4 : position-4+length])
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<17 {
			return
		}
		if err := hevcConfiguration(data); err != nil && !errors.Is(err, errAVIF) && !errors.Is(err, errAVIFUnsupported) && !errors.Is(err, errMetadataLimit) {
			t.Fatal("unexpected bounded decoder-record error", err)
		}
	})
}

func FuzzBoundedHEIFMetadata(f *testing.F) {
	for _, name := range []string{"still", "generic", "grid", "exif", "rotated", "mirrored", "collection"} {
		f.Add(heifFixture(f, name))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 65536 {
			return
		}
		policy := DefaultInspectPolicy()
		policy.HeaderBytes = 65536
		info, err := parseHEIF(t.Context(), bytes.NewReader(data), int64(len(data)), policy)
		if err == nil && (info.width < 1 || info.height < 1 || info.width > int64(policy.MaxDimension) || info.height > int64(policy.MaxDimension) || info.width > policy.MaxPixels/info.height || info.frames != 1 || !info.loopKnown || info.loops != 1 || info.duration != nil || info.capture != nil && info.capture.Nanosecond()%int(time.Millisecond) != 0) {
			t.Fatal("unbounded or guessed HEIF metadata", info)
		}
	})
}
