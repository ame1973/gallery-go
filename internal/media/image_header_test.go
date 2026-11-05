package media

import (
	"errors"
	"strings"
	"testing"
)

func TestImageHeaderSummaryAcceptsOnlyFixedBoundedGeometry(t *testing.T) {
	for _, text := range []string{"/proc/self/fd/3: 96x64 uchar, 3 bands, srgb, heifload\n", "/proc/self/fd/3: 16x12 ushort, 1 band, b-w, tiffload\n"} {
		value, err := parseImageHeader([]byte(text), DefaultInspectPolicy())
		if err != nil || value.width < 1 || value.height < 1 {
			t.Fatal("bounded image summary rejected", value, err)
		}
	}
	for _, text := range []string{
		"/private/source: 16x12 uchar, 3 bands, srgb, heifload\n",
		"/proc/self/fd/3: 0x12 uchar, 3 bands, srgb, heifload\n",
		"/proc/self/fd/3: +16x12 uchar, 3 bands, srgb, heifload\n",
		"/proc/self/fd/3: 16x-12 uchar, 3 bands, srgb, heifload\n",
		"/proc/self/fd/3: 16x12 uchar, 0 bands, srgb, heifload\n",
		"/proc/self/fd/3: 16x12 uchar, 17 bands, srgb, heifload\n",
		"/proc/self/fd/3: 16x12 uchar, 3, srgb, heifload\n",
		"/proc/self/fd/3: 16x12 uchar, 3 bands, srgb, /private/loader\n",
		"/proc/self/fd/3: 16x12 unknown, 3 bands, srgb, heifload\n",
		"/proc/self/fd/3: 16x12 uchar, 3 bands, srgb, heifload\nGPS: private\n",
		"/proc/self/fd/3: 16x12 uchar, 3 bands, srgb, heifload",
	} {
		if _, err := parseImageHeader([]byte(text), DefaultInspectPolicy()); err == nil {
			t.Fatal("uncontracted image output accepted", text)
		}
	}
	if _, err := parseImageHeader([]byte(strings.Repeat("x", 1025)), DefaultInspectPolicy()); !errors.Is(err, errMetadataLimit) {
		t.Fatal("image output budget ignored", err)
	}
	policy := DefaultInspectPolicy()
	policy.MaxPixels = 96*64 - 1
	if _, err := parseImageHeader([]byte("/proc/self/fd/3: 96x64 uchar, 3 bands, srgb, heifload\n"), policy); !errors.Is(err, errMetadataLimit) {
		t.Fatal("image pixel budget ignored", err)
	}
}

func FuzzFixedImageHeaderSummary(f *testing.F) {
	f.Add([]byte("/proc/self/fd/3: 96x64 uchar, 3 bands, srgb, heifload\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			return
		}
		value, err := parseImageHeader(data, DefaultInspectPolicy())
		if err == nil && (value.width < 1 || value.height < 1 || value.width > 65535 || value.height > 65535 || value.width > 120000000/value.height) {
			t.Fatal("unbounded image geometry accepted", value)
		}
	})
}
