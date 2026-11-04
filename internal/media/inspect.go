package media

import (
	"context"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

var ErrInspectPolicy = errors.New("invalid media inspection resource limits")

type InspectPolicy struct {
	MaxPixels    int64         `yaml:"max_pixels"`
	MaxDimension int           `yaml:"max_dimension"`
	HeaderBytes  int64         `yaml:"header_bytes"`
	Timeout      time.Duration `yaml:"timeout"`
}

func DefaultInspectPolicy() InspectPolicy {
	return InspectPolicy{MaxPixels: 120000000, MaxDimension: 65535, HeaderBytes: 8 << 20, Timeout: 15 * time.Second}
}
func (policy InspectPolicy) Validate() error {
	if policy.MaxPixels < 1 || policy.MaxPixels > 1000000000 || policy.MaxDimension < 1 || policy.MaxDimension > 1000000 || policy.HeaderBytes < 512 || policy.HeaderBytes > 32<<20 || policy.Timeout < time.Second || policy.Timeout > time.Minute {
		return ErrInspectPolicy
	}
	return nil
}

// Inspection is bounded metadata, not a decoded/READY derivative or a claim that
// every codec/frame is valid. Unsupported formats keep an explicit capability
// error until the controlled external codec adapter is implemented and verified.
// No EXIF/GPS/raw tags, paths, stderr, or worker capabilities enter this DTO.
type Inspection struct {
	Kind        string     `json:"kind"`
	MIME        string     `json:"mime"`
	Format      string     `json:"format"`
	Width       *int64     `json:"width"`
	Height      *int64     `json:"height"`
	DurationMS  *int64     `json:"duration_ms"`
	Orientation *int64     `json:"orientation"`
	CaptureAt   *time.Time `json:"capture_at"`
	Animated    *bool      `json:"animated"`
	Status      string     `json:"status"`
	ErrorCode   *string    `json:"error_code"`
}

func inspectionFailure(result Inspection, status, code string) Inspection {
	result.Status = status
	result.ErrorCode = &code
	return result
}

type inspectReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r inspectReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(buffer)
	if cancelled := r.ctx.Err(); cancelled != nil {
		return 0, cancelled
	}
	return n, err
}

// Inspect uses an already confined read-only regular descriptor. Its caller must
// bind this stat/hash to the staged source revision and recheck the root path
// afterwards. No image pixels are allocated: native formats use DecodeConfig.
func Inspect(ctx context.Context, file *os.File, name string, policy InspectPolicy) (Inspection, error) {
	if err := policy.Validate(); err != nil {
		return Inspection{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, policy.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return Inspection{}, err
	}
	before, err := file.Stat()
	if err != nil {
		return Inspection{}, err
	}
	if !before.Mode().IsRegular() {
		return Inspection{}, ErrSourceUnavailable
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Inspection{}, err
	}
	header := make([]byte, 512)
	n, err := io.ReadFull(inspectReader{ctx, file}, header)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return Inspection{}, err
	}
	kind, mime, codec := sniffMedia(header[:n])
	result := Inspection{Kind: kind, MIME: mime, Format: codec, Status: "not_media"}
	if kind == "" {
		if candidateKind(name) != "" {
			result.Kind = candidateKind(name)
			result = inspectionFailure(result, "invalid", "INVALID_MEDIA")
		}
	} else if codec != "jpeg" && codec != "png" && codec != "gif" {
		result = inspectionFailure(result, "unsupported", "CODEC_UNAVAILABLE")
	} else {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return Inspection{}, err
		}
		configuration, format, decodeErr := image.DecodeConfig(io.LimitReader(inspectReader{ctx, file}, policy.HeaderBytes))
		if err := ctx.Err(); err != nil {
			return Inspection{}, err
		}
		if decodeErr != nil || format != codec || configuration.Width < 1 || configuration.Height < 1 {
			result = inspectionFailure(result, "invalid", "INVALID_MEDIA")
		} else if configuration.Width > policy.MaxDimension || configuration.Height > policy.MaxDimension || int64(configuration.Width) > policy.MaxPixels/int64(configuration.Height) {
			result = inspectionFailure(result, "invalid", "MEDIA_LIMIT")
		} else {
			width, height := int64(configuration.Width), int64(configuration.Height)
			result.Width, result.Height, result.Status = &width, &height, "ok"
			if codec == "jpeg" {
				animated := false
				result.Animated = &animated
			}
		}
	}
	after, err := file.Stat()
	if err != nil {
		return Inspection{}, err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return Inspection{}, ErrContentChanged
	}
	if err := ctx.Err(); err != nil {
		return Inspection{}, err
	}
	return result, nil
}

func sniffMedia(header []byte) (kind, mime, codec string) {
	mime = http.DetectContentType(header)
	// Inspect the primary ISO BMFF brand before generic compatible-brand sniffing.
	if len(header) >= 16 && string(header[4:8]) == "ftyp" {
		brand := string(header[8:12])
		switch brand {
		case "avif", "avis":
			return "image", "image/avif", "avif"
		case "heic", "heix", "hevc", "hevx", "mif1", "msf1":
			return "image", "image/heif", "heif"
		case "qt  ":
			return "video", "video/quicktime", "quicktime"
		}
	}
	if len(header) >= 4 && string(header[:4]) == "\x1a\x45\xdf\xa3" {
		switch ebmlDocType(header) {
		case "webm":
			return "video", "video/webm", "webm"
		case "matroska":
			return "video", "video/x-matroska", "matroska"
		default:
			return "", "application/octet-stream", ""
		}
	}
	switch mime {
	case "image/jpeg":
		return "image", mime, "jpeg"
	case "image/png":
		return "image", mime, "png"
	case "image/gif":
		return "image", mime, "gif"
	case "image/webp":
		return "image", mime, "webp"
	case "video/mp4":
		return "video", mime, "mp4"
	}
	if len(header) >= 4 && (string(header[:4]) == "II\x2a\x00" || string(header[:4]) == "MM\x00\x2a") {
		return "image", "image/tiff", "tiff"
	}
	return "", "application/octet-stream", ""
}

// net/http identifies the EBML magic as WebM even for Matroska. Read only the
// bounded EBML header's DocType so an extension or generic magic cannot pick MIME.
func ebmlDocType(header []byte) string {
	if len(header) < 5 {
		return ""
	}
	size, width, ok := ebmlInteger(header[4:], false)
	if !ok || size > uint64(len(header)-4-width) {
		return ""
	}
	position, end := 4+width, 4+width+int(size)
	for position < end {
		id, count, ok := ebmlInteger(header[position:end], true)
		if !ok {
			return ""
		}
		position += count
		length, count, ok := ebmlInteger(header[position:end], false)
		if !ok {
			return ""
		}
		position += count
		if length > uint64(end-position) {
			return ""
		}
		if id == 0x4282 {
			return string(header[position : position+int(length)])
		}
		position += int(length)
	}
	return ""
}

func ebmlInteger(data []byte, identifier bool) (uint64, int, bool) {
	if len(data) == 0 || data[0] == 0 {
		return 0, 0, false
	}
	width, marker := 1, byte(0x80)
	for data[0]&marker == 0 {
		width++
		marker >>= 1
	}
	if width > len(data) || (identifier && width > 4) {
		return 0, 0, false
	}
	value := uint64(data[0])
	if !identifier {
		value = uint64(data[0] & (marker - 1))
	}
	for index := 1; index < width; index++ {
		value = value<<8 | uint64(data[index])
	}
	return value, width, true
}

func candidateKind(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".avif", ".heic", ".heif", ".tif", ".tiff", ".dng", ".cr2", ".nef", ".arw":
		return "image"
	case ".mp4", ".mov", ".mkv", ".webm":
		return "video"
	default:
		return ""
	}
}
