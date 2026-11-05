package media

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"strings"
	"testing"
	"time"
)

func exifBytes(order binary.ByteOrder, orientation uint16, date, zone string) []byte {
	data := make([]byte, 160)
	if order == binary.LittleEndian {
		copy(data[:2], "II")
	} else {
		copy(data[:2], "MM")
	}
	order.PutUint16(data[2:4], 42)
	order.PutUint32(data[4:8], 8)
	order.PutUint16(data[8:10], 3)
	entry := func(offset int, tag, kind uint16, count, value uint32) {
		order.PutUint16(data[offset:offset+2], tag)
		order.PutUint16(data[offset+2:offset+4], kind)
		order.PutUint32(data[offset+4:offset+8], count)
		order.PutUint32(data[offset+8:offset+12], value)
	}
	entry(10, 0x0112, 3, 1, 0)
	order.PutUint16(data[18:20], orientation)
	entry(22, 0x8769, 4, 1, 50)
	entry(34, 0x8825, 4, 1, 0xffffffff) // GPS is never dereferenced, even if it points outside the segment.
	order.PutUint16(data[50:52], 2)
	entry(52, 0x9003, 2, 20, 80)
	entry(64, 0x9011, 2, 7, 100)
	copy(data[80:100], date)
	copy(data[100:107], zone)
	copy(data[107:], "GPS /private/source/camera-secret")
	return data
}

func exifWithSubseconds(order binary.ByteOrder, fraction string) []byte {
	data := make([]byte, max(160, 140+len(fraction)))
	copy(data, exifBytes(order, 6, "2026:11:05 12:34:56\x00", "+08:00\x00"))
	order.PutUint16(data[50:52], 3)
	order.PutUint32(data[60:64], 112)
	order.PutUint32(data[72:76], 132)
	copy(data[112:132], "2026:11:05 12:34:56\x00")
	copy(data[132:139], "+08:00\x00")
	order.PutUint16(data[76:78], 0x9291)
	order.PutUint16(data[78:80], 2)
	order.PutUint32(data[80:84], uint32(len(fraction)+1))
	clear(data[84:92])
	if len(fraction)+1 <= 4 {
		copy(data[84:88], fraction+"\x00")
	} else {
		order.PutUint32(data[84:88], 139)
		copy(data[139:], fraction+"\x00")
	}
	return data
}

func TestEXIFOriginalSubsecondsNormalizeToUTCMilliseconds(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		for fraction, milliseconds := range map[string]int{"1": 100, "12": 120, "123": 123, "123456": 123, "000999999": 0, "999999999": 999, "123 ": 123, "": 0, "   ": 0, strings.Repeat("4", 63): 444} {
			result := inspectFixture(t, jpegWithEXIF(t, exifWithSubseconds(order, fraction)), "capture.jpg", DefaultInspectPolicy())
			want := time.Date(2026, 11, 5, 4, 34, 56, milliseconds*int(time.Millisecond), time.UTC)
			if result.Status != "ok" || result.CaptureAt == nil || !result.CaptureAt.Equal(want) || result.CaptureAt.Nanosecond()%int(time.Millisecond) != 0 || result.Orientation == nil || *result.Orientation != 6 {
				t.Fatal("EXIF inline/offset subseconds lost or leaked non-DB precision", fraction, result)
			}
		}
	}
}

func TestEXIFSubsecondsRejectAmbiguityAndNeverSupplyTimezone(t *testing.T) {
	for _, fraction := range []string{"12x", "-1", "1.2", "1 2", "1\x002", "１２３", strings.Repeat("1", 64)} {
		result := inspectFixture(t, jpegWithEXIF(t, exifWithSubseconds(binary.LittleEndian, fraction)), "bad.jpg", DefaultInspectPolicy())
		want := "INVALID_MEDIA"
		if len(fraction) == 64 {
			want = "MEDIA_LIMIT"
		}
		if result.Status != "invalid" || result.ErrorCode == nil || *result.ErrorCode != want || result.CaptureAt != nil || result.Orientation != nil || result.Width != nil {
			t.Fatal("bad EXIF fraction gained trusted metadata", fraction, result)
		}
	}
	for _, change := range []string{"type", "zero-count", "offset", "terminator", "duplicate"} {
		data := exifWithSubseconds(binary.LittleEndian, "123456")
		switch change {
		case "type":
			binary.LittleEndian.PutUint16(data[78:80], 4)
		case "zero-count":
			binary.LittleEndian.PutUint32(data[80:84], 0)
		case "offset":
			binary.LittleEndian.PutUint32(data[84:88], 0xffffffff)
		case "terminator":
			data[145] = 'x'
		case "duplicate":
			copy(data[64:76], data[76:88])
		}
		result := inspectFixture(t, jpegWithEXIF(t, data), "bad.jpg", DefaultInspectPolicy())
		if result.Status != "invalid" || result.CaptureAt != nil || result.Orientation != nil {
			t.Fatal("ambiguous/unbounded subsecond field was accepted", change, result)
		}
	}
	for _, missing := range []string{"date", "zone", "unknown-zone"} {
		data := exifWithSubseconds(binary.LittleEndian, "123456")
		switch missing {
		case "date":
			binary.LittleEndian.PutUint16(data[52:54], 0x9004)
		case "zone":
			binary.LittleEndian.PutUint16(data[64:66], 0x9999)
		case "unknown-zone":
			copy(data[132:139], "-00:00\x00")
		}
		result := inspectFixture(t, jpegWithEXIF(t, data), "unknown.jpg", DefaultInspectPolicy())
		if result.Status != "ok" || result.CaptureAt != nil {
			t.Fatal("subseconds supplied an unproven original time/timezone", missing, result)
		}
	}
}

func TestEXIFMillisecondPrecisionIsSharedByPNGAndWebP(t *testing.T) {
	exif := exifWithSubseconds(binary.LittleEndian, "123456789")
	png := pngChunks(pngFixture(t, false))
	png = append(png[:1], append([]testPNGChunk{{"eXIf", exif}}, png[1:]...)...)
	webp := extendedWebP(t, "lossless", 8)
	webp = append(webp, testWebPChunk{"EXIF", exif})
	for _, data := range [][]byte{encodePNGChunks(png), encodeWebP(webp)} {
		result := inspectFixture(t, data, "capture", DefaultInspectPolicy())
		want := time.Date(2026, 11, 5, 4, 34, 56, 123*int(time.Millisecond), time.UTC)
		if result.Status != "ok" || result.CaptureAt == nil || !result.CaptureAt.Equal(want) {
			t.Fatal("container-specific EXIF precision diverged", result)
		}
	}
}

func jpegWithEXIF(t *testing.T, payloads ...[]byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 12, 7)), nil); err != nil {
		t.Fatal(err)
	}
	data := append([]byte{}, encoded.Bytes()[:2]...)
	for _, payload := range payloads {
		value := append([]byte("Exif\x00\x00"), payload...)
		segment := []byte{0xff, 0xe1, 0, 0}
		binary.BigEndian.PutUint16(segment[2:], uint16(len(value)+2))
		data = append(data, segment...)
		data = append(data, value...)
	}
	return append(data, encoded.Bytes()[2:]...)
}

func TestJPEGEXIFWhitelistsOrientationAndExplicitZoneCapture(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		for orientation := uint16(1); orientation <= 8; orientation++ {
			result := inspectFixture(t, jpegWithEXIF(t, exifBytes(order, orientation, "2026:11:05 12:34:56\x00", "+08:00\x00")), "photo.jpg", DefaultInspectPolicy())
			want := time.Date(2026, 11, 5, 4, 34, 56, 0, time.UTC)
			if result.Status != "ok" || result.Orientation == nil || *result.Orientation != int64(orientation) || result.CaptureAt == nil || !result.CaptureAt.Equal(want) || result.CaptureAt.Location() != time.UTC || result.Width == nil || *result.Width != 12 || result.Height == nil || *result.Height != 7 {
				t.Fatal("EXIF changed original dimensions or invented capture", result)
			}
			data, err := json.Marshal(result)
			if err != nil || bytes.Contains(data, []byte("private")) || bytes.Contains(data, []byte("camera-secret")) || bytes.Contains(data, []byte("GPS")) {
				t.Fatal("private EXIF metadata leaked", string(data), err)
			}
		}
	}
	for _, zone := range []string{"+00:00\x00", "-05:30\x00", "+14:00\x00", "-14:00\x00"} {
		result := inspectFixture(t, jpegWithEXIF(t, exifBytes(binary.LittleEndian, 6, "2024:02:29 00:00:01\x00", zone)), "photo.jpg", DefaultInspectPolicy())
		want, err := time.Parse("2006:01:02 15:04:05-07:00", "2024:02:29 00:00:01"+strings.TrimSuffix(zone, "\x00"))
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "ok" || result.CaptureAt == nil || !result.CaptureAt.Equal(want.UTC()) {
			t.Fatal(zone, result)
		}
	}
}

func TestJPEGEXIFNeverGuessesTimezoneOrUsesModificationTime(t *testing.T) {
	for _, change := range []string{"missing-offset", "unknown-offset", "missing-original", "only-modification-date", "no-exif"} {
		t.Run(change, func(t *testing.T) {
			data := exifBytes(binary.LittleEndian, 6, "2026:11:05 12:34:56\x00", "+08:00\x00")
			switch change {
			case "missing-offset":
				binary.LittleEndian.PutUint16(data[64:66], 0x9999)
			case "unknown-offset":
				copy(data[100:107], "-00:00\x00")
			case "missing-original":
				binary.LittleEndian.PutUint16(data[52:54], 0x9004)
			case "only-modification-date":
				binary.LittleEndian.PutUint16(data[52:54], 0x0132)
			}
			input := jpegWithEXIF(t, data)
			if change == "no-exif" {
				input = jpegWithEXIF(t)
			}
			result := inspectFixture(t, input, "photo.jpg", DefaultInspectPolicy())
			if result.Status != "ok" || result.CaptureAt != nil {
				t.Fatal("unproven timezone/capture became UTC", change, result)
			}
		})
	}
}

func TestJPEGEXIFRejectsAmbiguousUnsafeAndMalformedWhitelistedMetadata(t *testing.T) {
	changes := map[string]func([]byte){
		"bad-byte-order":          func(d []byte) { copy(d[:2], "ZZ") },
		"big-tiff":                func(d []byte) { binary.LittleEndian.PutUint16(d[2:4], 43) },
		"first-ifd-before-header": func(d []byte) { binary.LittleEndian.PutUint32(d[4:8], 0) },
		"first-ifd-overflow":      func(d []byte) { binary.LittleEndian.PutUint32(d[4:8], 0xffffffff) },
		"ifd-count-over-limit":    func(d []byte) { binary.LittleEndian.PutUint16(d[8:10], 513) },
		"ifd-table-outside":       func(d []byte) { binary.LittleEndian.PutUint16(d[8:10], 13) },
		"orientation-zero":        func(d []byte) { binary.LittleEndian.PutUint16(d[18:20], 0) },
		"orientation-nine":        func(d []byte) { binary.LittleEndian.PutUint16(d[18:20], 9) },
		"orientation-type":        func(d []byte) { binary.LittleEndian.PutUint16(d[12:14], 4) },
		"orientation-count":       func(d []byte) { binary.LittleEndian.PutUint32(d[14:18], 2) },
		"duplicate-orientation":   func(d []byte) { copy(d[34:46], d[10:22]) },
		"duplicate-exif":          func(d []byte) { copy(d[34:46], d[22:34]) },
		"cyclic-exif":             func(d []byte) { binary.LittleEndian.PutUint32(d[30:34], 8) },
		"bad-exif-pointer-type":   func(d []byte) { binary.LittleEndian.PutUint16(d[24:26], 3) },
		"exif-count-over-limit":   func(d []byte) { binary.LittleEndian.PutUint16(d[50:52], 513) },
		"duplicate-date":          func(d []byte) { copy(d[64:76], d[52:64]) },
		"date-kind":               func(d []byte) { binary.LittleEndian.PutUint16(d[54:56], 1) },
		"date-count":              func(d []byte) { binary.LittleEndian.PutUint32(d[56:60], 0xffffffff) },
		"date-offset":             func(d []byte) { binary.LittleEndian.PutUint32(d[60:64], 0xffffffff) },
		"date-terminator":         func(d []byte) { d[99] = 'x' },
		"date-inner-null":         func(d []byte) { d[84] = 0 },
		"date-invalid-calendar":   func(d []byte) { copy(d[80:100], "2025:02:29 12:34:56\x00") },
		"zone-no-sign":            func(d []byte) { copy(d[100:107], "Z08:00\x00") },
		"zone-no-colon":           func(d []byte) { copy(d[100:107], "+08000\x00") },
		"zone-not-digits":         func(d []byte) { copy(d[100:107], "+AA:00\x00") },
		"zone-too-large":          func(d []byte) { copy(d[100:107], "+14:01\x00") },
		"zone-bad-minute":         func(d []byte) { copy(d[100:107], "+08:60\x00") },
		"utc-year-underflow":      func(d []byte) { copy(d[80:100], "0001:01:01 00:00:00\x00"); copy(d[100:107], "+14:00\x00") },
		"utc-year-overflow":       func(d []byte) { copy(d[80:100], "9999:12:31 23:59:59\x00"); copy(d[100:107], "-14:00\x00") },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			data := exifBytes(binary.LittleEndian, 6, "2026:11:05 12:34:56\x00", "+08:00\x00")
			change(data)
			result := inspectFixture(t, jpegWithEXIF(t, data), "photo.jpg", DefaultInspectPolicy())
			if result.Status != "invalid" || result.ErrorCode == nil || result.Width != nil || result.Height != nil || result.Orientation != nil || result.CaptureAt != nil {
				t.Fatal("invalid EXIF gained trusted metadata", name, result)
			}
			want := "INVALID_MEDIA"
			if strings.Contains(name, "over-limit") {
				want = "MEDIA_LIMIT"
			}
			if *result.ErrorCode != want {
				t.Fatal(name, result)
			}
		})
	}
	data := exifBytes(binary.LittleEndian, 6, "2026:11:05 12:34:56\x00", "+08:00\x00")
	result := inspectFixture(t, jpegWithEXIF(t, data, data), "photo.jpg", DefaultInspectPolicy())
	if result.Status != "invalid" || result.CaptureAt != nil {
		t.Fatal("duplicate EXIF segments accepted", result)
	}
}

func TestJPEGEXIFHeaderBudgetAndTruncatedSegmentsFailClosed(t *testing.T) {
	data := exifBytes(binary.LittleEndian, 6, "2026:11:05 12:34:56\x00", "+08:00\x00")
	encoded := jpegWithEXIF(t, data)
	policy := DefaultInspectPolicy()
	policy.HeaderBytes = 512
	result := inspectFixture(t, encoded, "photo.jpg", policy)
	if result.Status != "invalid" || result.ErrorCode == nil || *result.ErrorCode != "MEDIA_LIMIT" {
		t.Fatal("JPEG marker budget bypassed", result)
	}
	for _, data := range [][]byte{nil, []byte("II"), data[:7], data[:80]} {
		if _, _, err := parseEXIF(data); err == nil {
			t.Fatal("truncated EXIF accepted")
		}
	}
}

func FuzzBoundedEXIF(f *testing.F) {
	f.Add(exifBytes(binary.LittleEndian, 6, "2026:11:05 12:34:56\x00", "+08:00\x00"))
	f.Add(exifBytes(binary.BigEndian, 1, "2024:02:29 00:00:01\x00", "-05:30\x00"))
	f.Add([]byte("II\x2a\x00\xff\xff\xff\xff"))
	f.Add(exifWithSubseconds(binary.LittleEndian, "123"))
	f.Add(exifWithSubseconds(binary.BigEndian, "123456789"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 65533 {
			data = data[:65533]
		}
		orientation, capture, err := parseEXIF(data)
		if err != nil && !errors.Is(err, errEXIF) && !errors.Is(err, errMetadataLimit) {
			t.Fatal(err)
		}
		if err == nil && orientation != nil && (*orientation < 1 || *orientation > 8) {
			t.Fatal("invalid orientation")
		}
		if err == nil && capture != nil {
			_, offset := capture.Zone()
			if offset != 0 || capture.Year() < 1 || capture.Year() > 9999 || capture.Nanosecond()%1000000 != 0 {
				t.Fatal("unbounded/non-UTC capture")
			}
			if _, err := capture.MarshalJSON(); err != nil {
				t.Fatal(err)
			}
		}
	})
}
