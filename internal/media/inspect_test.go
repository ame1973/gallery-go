package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func inspectFixture(t *testing.T, data []byte, name string, policy InspectPolicy) Inspection {
	t.Helper()
	path := filepath.Join(t.TempDir(), "read-only-input")
	if err := os.WriteFile(path, data, 0o400); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	result, err := Inspect(t.Context(), file, name, policy)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatal("inspection modified input", err)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), path) || strings.Contains(string(encoded), "GPS") || strings.Contains(string(encoded), "stderr") {
		t.Fatal("private inspection data leaked")
	}
	return result
}

func TestNativeInspectionUsesMagicNotExtensionAndBoundsDimensions(t *testing.T) {
	picture := image.NewRGBA(image.Rect(0, 0, 12, 7))
	for _, format := range []string{"jpeg", "png", "gif"} {
		t.Run(format, func(t *testing.T) {
			var encoded bytes.Buffer
			var err error
			switch format {
			case "jpeg":
				err = jpeg.Encode(&encoded, picture, nil)
			case "png":
				err = png.Encode(&encoded, picture)
			case "gif":
				err = gif.Encode(&encoded, picture, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			result := inspectFixture(t, encoded.Bytes(), "misleading.mp4", DefaultInspectPolicy())
			if result.Status != "ok" || result.Kind != "image" || result.Format != format || result.Width == nil || *result.Width != 12 || result.Height == nil || *result.Height != 7 || result.ErrorCode != nil {
				t.Fatal(result)
			}
			if format == "gif" && result.Animated != nil {
				t.Fatal("header-only probe fabricated GIF frame state")
			}
			policy := DefaultInspectPolicy()
			policy.MaxPixels = 83
			limited := inspectFixture(t, encoded.Bytes(), "photo", policy)
			if limited.Status != "invalid" || limited.ErrorCode == nil || *limited.ErrorCode != "MEDIA_LIMIT" || limited.Width != nil {
				t.Fatal("pixel limit bypassed", limited)
			}
		})
	}
}

func TestAnimatedGIFInspectionDoesNotPretendStaticOrInventDuration(t *testing.T) {
	frame := image.NewPaletted(image.Rect(0, 0, 3, 2), color.Palette{color.Black, color.White})
	animation := gif.GIF{Image: []*image.Paletted{frame, frame}, Delay: []int{10, 20}, LoopCount: 0}
	var data bytes.Buffer
	if err := gif.EncodeAll(&data, &animation); err != nil {
		t.Fatal(err)
	}
	result := inspectFixture(t, data.Bytes(), "animation.gif", DefaultInspectPolicy())
	if result.Status != "ok" || result.Animated != nil || result.DurationMS != nil {
		t.Fatal("header inspection fabricated animation metadata", result)
	}
}

func TestInspectionExplicitlyDiagnosesUnsupportedAndInvalidInputs(t *testing.T) {
	avif := make([]byte, 24)
	binary.BigEndian.PutUint32(avif[:4], 24)
	copy(avif[4:8], "ftyp")
	copy(avif[8:12], "avif")
	copy(avif[16:20], "mp42")
	webp := []byte("RIFF\x10\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00\x00\x00\x00\x00")
	for _, test := range []struct {
		data                           []byte
		name, kind, mime, status, code string
	}{
		{avif, "video.mov", "image", "image/avif", "unsupported", "CODEC_UNAVAILABLE"},
		{webp, "image.webp", "image", "image/webp", "unsupported", "CODEC_UNAVAILABLE"},
		{[]byte("\x1a\x45\xdf\xa3\x8b\x42\x82\x88matroska"), "video.mkv", "video", "video/x-matroska", "unsupported", "CODEC_UNAVAILABLE"},
		{[]byte("\x1a\x45\xdf\xa3\x87\x42\x82\x84webm"), "video.webm", "video", "video/webm", "unsupported", "CODEC_UNAVAILABLE"},
		{[]byte("\x1a\x45\xdf\xa3container"), "invalid.mkv", "video", "application/octet-stream", "invalid", "INVALID_MEDIA"},
		{[]byte("not a JPEG"), "bad.JPEG", "image", "application/octet-stream", "invalid", "INVALID_MEDIA"},
		{[]byte("GIF89a"), "bad.gif", "image", "image/gif", "invalid", "INVALID_MEDIA"},
		{[]byte("ordinary sidecar text"), "notes.txt", "", "application/octet-stream", "not_media", ""},
	} {
		result := inspectFixture(t, test.data, test.name, DefaultInspectPolicy())
		if result.Kind != test.kind || result.MIME != test.mime || result.Status != test.status {
			t.Fatal(test.name, result)
		}
		if test.code != "" && (result.ErrorCode == nil || *result.ErrorCode != test.code) {
			t.Fatal(test.name, result)
		}
		if result.Width != nil || result.Height != nil {
			t.Fatal("unsupported/invalid metadata fabricated dimensions", result)
		}
	}
}

func FuzzSniffMediaContainerHeaders(f *testing.F) {
	for _, seed := range [][]byte{nil, []byte("GIF89a"), []byte("\x1a\x45\xdf\xa3\x87\x42\x82\x84webm"), []byte("\x1a\x45\xdf\xa3\xff\xff\xff\xff"), []byte("\x00\x00\x00\x10ftypavif\x00\x00\x00\x00")} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, header []byte) {
		if len(header) > 512 {
			header = header[:512]
		}
		kind, mime, _ := sniffMedia(header)
		if kind != "" && kind != "image" && kind != "video" {
			t.Fatal("unknown media kind", kind)
		}
		if mime == "" {
			t.Fatal("empty MIME")
		}
	})
}

func TestInspectionPolicyAndCancellationFailBeforeDecode(t *testing.T) {
	for _, policy := range []InspectPolicy{{}, func() InspectPolicy { p := DefaultInspectPolicy(); p.HeaderBytes = 511; return p }()} {
		if !errors.Is(policy.Validate(), ErrInspectPolicy) {
			t.Fatal("invalid resource policy accepted")
		}
	}
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, []byte("GIF89a"), 0o400); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = Inspect(ctx, file, "image.gif", DefaultInspectPolicy())
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
