package media

import (
	"errors"
	"strings"
	"testing"
)

func TestOriginalOnlyContainersNeverExpandInlineVariantMIMEs(t *testing.T) {
	for value, expected := range map[string]string{
		"video/quicktime": "video/quicktime", "video/x-matroska": "video/x-matroska",
		"VIDEO/QUICKTIME; codecs=h264": "video/quicktime", "video/mp4": "video/mp4",
		"video/webm": "video/webm", "image/png": "image/png", "image/tiff": "image/tiff", "image/heif": "image/heif", "": "application/octet-stream",
		"text/html": "application/octet-stream", "image/svg+xml": "application/octet-stream",
		"video/private-format": "application/octet-stream", "video/mp4\r\nX-Test: injected": "application/octet-stream",
	} {
		if actual := originalMIME(value); actual != expected {
			t.Fatal("unsafe original MIME normalization", value, actual)
		}
	}
	for _, value := range []string{"video/quicktime", "video/x-matroska", "image/tiff", "image/heif", "text/html", "image/svg+xml"} {
		if _, err := VariantPath("revision", "display", 1, strings.Repeat("a", 64), value); !errors.Is(err, ErrVariantUnavailable) || safeMIME(value) != "" {
			t.Fatal("original-only type became an inline READY preset", value, err)
		}
	}
}
