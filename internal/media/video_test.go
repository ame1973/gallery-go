//go:build linux && (amd64 || arm64)

package media

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func videoText(value string) *string { return &value }

func videoFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("../../tests/fixtures/video.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]string
	if json.Unmarshal(data, &fixtures) != nil {
		t.Fatal("invalid owned video vectors")
	}
	decoded, err := base64.StdEncoding.DecodeString(fixtures[name])
	if err != nil || len(decoded) == 0 {
		t.Fatal("missing video vector", name, err)
	}
	return decoded
}

func TestActualSandboxVideoMetadataUsesContainerEvidenceNotExtension(t *testing.T) {
	for _, item := range []struct {
		name, format, mime, codec, capture string
		orientation                        int64
	}{
		{"mp4", "mp4", "video/mp4", "h264", "2026-11-05T04:34:56Z", 6},
		{"mov", "quicktime", "video/quicktime", "h264", "2026-11-05T04:34:56.123Z", 6},
		{"legacy_mov", "quicktime", "video/quicktime", "h264", "2026-11-05T04:34:56.123Z", 6},
		{"multi", "matroska", "video/x-matroska", "ffv1", "2026-11-05T04:34:56.123Z", 0},
		{"matroska", "matroska", "video/x-matroska", "ffv1", "2026-11-05T04:34:56.123Z", 0},
		{"webm", "webm", "video/webm", "vp8", "2026-11-05T04:34:56.123Z", 0},
	} {
		t.Run(item.name, func(t *testing.T) {
			policy := DefaultInspectPolicy()
			policy.MaxFrames = 1 // Image animation bounds do not estimate video frames.
			result := inspectFixture(t, videoFixture(t, item.name), "misleading.gif", policy)
			if result.Status != "ok" || result.Kind != "video" || result.Format != item.format || result.MIME != item.mime || result.Codec == nil || *result.Codec != item.codec || result.Width == nil || *result.Width != 16 || result.Height == nil || *result.Height != 12 || result.DurationMS == nil || *result.DurationMS != 400 || result.CaptureAt == nil || result.CaptureAt.Format(time.RFC3339Nano) != item.capture {
				t.Fatal("actual bounded video metadata was lost or fabricated", result)
			}
			if item.orientation == 0 && result.Orientation != nil || item.orientation != 0 && (result.Orientation == nil || *result.Orientation != item.orientation) || result.Animated != nil || result.FrameCount != nil || result.LoopCount != nil {
				t.Fatal("video transforms or image-only animation evidence were guessed", result)
			}
			policy.MaxPixels = 191
			limited := inspectFixture(t, videoFixture(t, item.name), "video", policy)
			if limited.Status != "invalid" || limited.ErrorCode == nil || *limited.ErrorCode != "MEDIA_LIMIT" || limited.Codec != nil || limited.Width != nil || limited.CaptureAt != nil {
				t.Fatal("video pixel budget or trusted-field clearing bypassed", limited)
			}
		})
	}
}

func TestAttachedPictureOnlyContainersDoNotBecomeVideo(t *testing.T) {
	result := inspectFixture(t, videoFixture(t, "attached_only"), "cover.mp4", DefaultInspectPolicy())
	if result.Status != "unsupported" || result.ErrorCode == nil || *result.ErrorCode != "CODEC_UNAVAILABLE" || result.Width != nil || result.Codec != nil {
		t.Fatal("embedded cover was selected as the primary video", result)
	}
}

func TestVideoUnavailableAndChangedRuntimeNeverFallBack(t *testing.T) {
	policy := DefaultInspectPolicy()
	policy.codecRuntime = "unavailable"
	result := inspectFixture(t, videoFixture(t, "mp4"), "video.mp4", policy)
	if result.Status != "unsupported" || result.ErrorCode == nil || *result.ErrorCode != "CODEC_UNAVAILABLE" || result.Codec != nil || result.Width != nil {
		t.Fatal("missing capability invented metadata", result)
	}
	name := filepath.Join(t.TempDir(), "video")
	if err := os.WriteFile(name, videoFixture(t, "mp4"), 0o400); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	policy.codecRuntime = strings.Repeat("0", 64)
	if _, err := Inspect(t.Context(), input, "video", policy); !errors.Is(err, ErrContentChanged) {
		t.Fatal("changed codec installation was accepted under a frozen generation", err)
	}
}

const validVideoJSON = `{"programs":[],"streams":[{"index":0,"codec_name":"h264","codec_type":"video","width":16,"height":12,"duration":"0.400000","disposition":{"attached_pic":0},"tags":{}}],"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"0.400000","tags":{}}}`

func TestVideoProjectionRejectsAmbiguousPrivateOrUnboundedData(t *testing.T) {
	for _, data := range []string{
		strings.Replace(validVideoJSON, `"width":16`, `"width":16,"width":32`, 1),
		strings.Replace(validVideoJSON, `"width":16`, `"Width":16`, 1),
		strings.Replace(validVideoJSON, `"width":16`, `"width":null`, 1),
		strings.Replace(validVideoJSON, `"width":16`, `"width":0`, 1),
		strings.Replace(validVideoJSON, `"codec_name":"h264"`, `"codec_name":"/private/codec"`, 1),
		strings.Replace(validVideoJSON, `"codec_type":"video"`, `"codec_type":"audio"`, 1),
		strings.Replace(validVideoJSON, `"attached_pic":0`, `"attached_pic":1`, 1),
		strings.Replace(validVideoJSON, `"attached_pic":0`, `"attached_pic":null`, 1),
		strings.Replace(validVideoJSON, `"index":0`, `"index":64`, 1),
		strings.Replace(validVideoJSON, `"tags":{}`, `"tags":{"GPS":"private canary"}`, 1),
		strings.Replace(validVideoJSON, `"tags":{}`, `"tags":{"creation_time":null}`, 1),
		strings.Replace(validVideoJSON, `"programs":[]`, `"programs":[{}]`, 1),
		strings.Replace(validVideoJSON, `"programs":[]`, `"stream_groups":[{}]`, 1),
		strings.Replace(validVideoJSON, `mov,mp4,m4a,3gp,3g2,mj2`, `matroska,webm`, 1),
		validVideoJSON + `{}`, "{\"streams\":\"\xff\"}",
	} {
		if _, err := parseVideoProjection([]byte(data), "mp4", DefaultInspectPolicy()); !errors.Is(err, errVideoMetadata) {
			t.Fatal("untrusted projection accepted", data, err)
		}
	}
	for _, data := range []string{
		strings.Replace(validVideoJSON, `"codec_name":"h264"`, `"codec_name":"unknown"`, 1),
		`{"streams":[],"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2"}}`,
	} {
		if _, err := parseVideoProjection([]byte(data), "mp4", DefaultInspectPolicy()); !errors.Is(err, errVideoUnsupported) {
			t.Fatal("unknown codec/cover-only media invented video", err)
		}
	}
	policy := DefaultInspectPolicy()
	policy.MaxDuration = 399 * time.Millisecond
	if _, err := parseVideoProjection([]byte(validVideoJSON), "mp4", policy); !errors.Is(err, errMetadataLimit) {
		t.Fatal("duration budget ignored", err)
	}
	if _, err := parseVideoProjection(make([]byte, (1<<20)+1), "mp4", DefaultInspectPolicy()); !errors.Is(err, errMetadataLimit) {
		t.Fatal("output byte budget ignored", err)
	}
}

func TestVideoDurationUsesExactPositiveDeclaredSeconds(t *testing.T) {
	for text, expected := range map[string]int64{"0.0000001": 1, "0.001": 1, "0.001000001": 2, "0.400000": 400, "86400": 86400000} {
		value, err := videoDuration(&text, 24*time.Hour)
		if err != nil || value == nil || *value != expected {
			t.Fatal("video duration rounded incorrectly", text, value, err)
		}
	}
	for _, text := range []*string{nil, videoText(""), videoText("N/A"), videoText("0"), videoText("0.000000")} {
		if value, err := videoDuration(text, 24*time.Hour); err != nil || value != nil {
			t.Fatal("unknown duration was fabricated", text, value, err)
		}
	}
	for _, text := range []string{"-1", "+1", " 1", "1 ", "1/2", "1e3", "NaN", "Infinity", ".5", "1.", "1.2.3"} {
		if _, err := videoDuration(&text, 24*time.Hour); !errors.Is(err, errVideoMetadata) {
			t.Fatal("malformed declared seconds accepted", text, err)
		}
	}
	if _, err := videoDuration(videoText("86400.0000001"), 24*time.Hour); !errors.Is(err, errMetadataLimit) {
		t.Fatal("submillisecond duration overflow accepted", err)
	}
}

func TestVideoCapturePrefersExplicitOriginalAndNeverGuessesTimezones(t *testing.T) {
	original := "2026-11-05T12:34:56.123999999+08:00"
	value, err := videoCapture(videoTags{Created: videoText("2020-01-01T00:00:00Z")}, videoTags{Original: &original})
	if err != nil || value == nil || value.Format(time.RFC3339Nano) != "2026-11-05T04:34:56.123Z" {
		t.Fatal("original capture was replaced or rounded", value, err)
	}
	for _, unknown := range []string{"2026-11-05T12:34:56", "2026-11-05T12:34:56-00:00", "2026-11-05T12:34:56+24:00", "2026-11-05T12:34:56+08:60", "not a timestamp", "2026-11-05T4:34:56Z", "2026-11-05T04:34:56,123Z", "0000-01-01T00:00:00Z"} {
		value, err := videoCapture(videoTags{Created: &unknown}, videoTags{})
		if err != nil || value != nil {
			t.Fatal("unknown capture timezone or value was guessed", unknown, value, err)
		}
	}
	value, err = videoCapture(videoTags{Created: &original}, videoTags{Created: videoText("2020-01-01T00:00:00Z")})
	if err != nil || value != nil {
		t.Fatal("conflicting capture times were selected arbitrarily", value, err)
	}
	if _, err := videoCapture(videoTags{Created: &original, CreatedUpper: &original}, videoTags{}); !errors.Is(err, errVideoMetadata) {
		t.Fatal("duplicate-cased capture evidence accepted", err)
	}
}

func TestVideoDisplayMatrixMapsAllEightOrthogonalTransforms(t *testing.T) {
	for coefficients, expected := range map[[4]int64]int64{
		{65536, 0, 0, 65536}: 1, {-65536, 0, 0, 65536}: 2, {-65536, 0, 0, -65536}: 3, {65536, 0, 0, -65536}: 4,
		{0, 65536, 65536, 0}: 5, {0, 65536, -65536, 0}: 6, {0, -65536, -65536, 0}: 7, {0, -65536, 65536, 0}: 8,
	} {
		matrix := fmt.Sprintf("\n00000000: %d %d 0\n00000001: %d %d 0\n00000002: 0 0 1073741824\n", coefficients[0], coefficients[1], coefficients[2], coefficients[3])
		value, err := videoOrientation([]videoSideData{{Matrix: &matrix}}, videoTags{})
		if err != nil || value == nil || *value != expected {
			t.Fatal("matrix orientation was misreported", coefficients, value, err)
		}
		if _, err := videoOrientation([]videoSideData{{Matrix: &matrix}, {Matrix: &matrix}}, videoTags{}); !errors.Is(err, errVideoMetadata) {
			t.Fatal("ambiguous display matrices accepted", err)
		}
	}
	for text, expected := range map[string]int64{"0": 1, "90": 6, "180": 3, "270": 8, "-90": 8, "450": 6} {
		value, err := videoOrientation(nil, videoTags{Rotate: &text})
		if err != nil || value == nil || *value != expected {
			t.Fatal("legacy explicit quarter turn was lost", text, value, err)
		}
	}
	for _, text := range []string{"45", "90.1", "not rotation"} {
		if value, err := videoOrientation(nil, videoTags{Rotate: &text}); err != nil || value != nil {
			t.Fatal("arbitrary transform guessed", value, err)
		}
	}
	if _, err := videoOrientation(make([]videoSideData, 17), videoTags{}); !errors.Is(err, errMetadataLimit) {
		t.Fatal("side-data bounds ignored", err)
	}
}

func FuzzBoundedVideoProjection(f *testing.F) {
	f.Add([]byte(validVideoJSON))
	f.Add([]byte(`{"streams":[],"format":{"format_name":"matroska,webm"}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 65536 {
			return
		}
		value, err := parseVideoProjection(data, "mp4", DefaultInspectPolicy())
		if err == nil && (!ValidCodec(value.codec) || value.width < 1 || value.height < 1 || value.width > 65535 || value.height > 65535 || value.width > 120000000/value.height || value.capture != nil && value.capture.Nanosecond()%1000000 != 0) {
			t.Fatal("unbounded or invalid metadata accepted", value)
		}
	})
}
