package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"gallerygo/internal/auth"
)

var presetPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

var mediaExtensions = map[string]string{
	"image/jpeg": "jpg", "image/png": "png", "image/webp": "webp", "image/gif": "gif", "image/avif": "avif",
	"video/mp4": "mp4", "video/webm": "webm",
}

func safeMIME(value string) string {
	kind, _, err := mime.ParseMediaType(value)
	if err != nil || mediaExtensions[kind] == "" {
		return ""
	}
	return kind
}

// Original-only containers keep their detected MIME behind the additional grant
// and forced attachment policy; this never expands the inline READY preset list.
func originalMIME(value string) string {
	kind, _, err := mime.ParseMediaType(value)
	if err == nil && (mediaExtensions[kind] != "" || kind == "video/quicktime" || kind == "video/x-matroska" || kind == "image/tiff" || kind == "image/heif") {
		return kind
	}
	return "application/octet-stream"
}

func validHash(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func localPath(name string) bool {
	return name != "." && fs.ValidPath(name) && !strings.Contains(name, `\`)
}

// VariantPath is the private immutable storage key shared by readers and the
// publisher. A database row cannot redirect a variant to another revision's key.
func VariantPath(revision, preset string, version int64, hash, contentType string) (string, error) {
	kind := safeMIME(contentType)
	if !localPath(revision) || strings.Contains(revision, "/") || !presetPattern.MatchString(preset) || version < 1 || !validHash(hash) || kind == "" {
		return "", ErrVariantUnavailable
	}
	return path.Join("variants", revision, preset, strconv.FormatInt(version, 10), hash+"."+mediaExtensions[kind]), nil
}

// Content is a context-aware seekable read-only descriptor. Signature checks on
// each read/seek abort on detected in-flight changes rather than appending errors
// to partial media. Filesystem signatures do not provide an atomic NAS snapshot;
// context cannot interrupt arbitrary kernel filesystem stalls.
type Content struct {
	MIME       string
	Filename   string
	ETag       string
	Size       int64
	Attachment bool
	file       *os.File
	info       fs.FileInfo
	ctx        context.Context
	err        error
}

func (content *Content) check() error {
	if err := content.ctx.Err(); err != nil {
		return err
	}
	info, err := content.file.Stat()
	if err != nil {
		return err
	}
	if !content.sameInfo(info) {
		return ErrContentChanged
	}
	return nil
}

func (content *Content) Read(buffer []byte) (int, error) {
	if err := content.check(); err != nil {
		content.err = err
		return 0, err
	}
	n, err := content.file.Read(buffer)
	if changed := content.check(); changed != nil {
		content.err = changed
		return 0, changed
	}
	if err != nil && !errors.Is(err, io.EOF) {
		content.err = err
	}
	return n, err
}

func (content *Content) Seek(offset int64, whence int) (int64, error) {
	if err := content.check(); err != nil {
		content.err = err
		return 0, err
	}
	position, err := content.file.Seek(offset, whence)
	if err != nil {
		content.err = err
	}
	return position, err
}

func (content *Content) Close() error { return content.file.Close() }
func (content *Content) Err() error   { return content.err }

// OpenReadOnlyFile confines a regular input to root and never opens for write.
// Scanner and source-health adapters reuse the same FIFO/symlink boundary.
func OpenReadOnlyFile(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	if !localPath(name) {
		return nil, nil, errors.New("invalid root-relative media path")
	}
	// The Linux server never opens for write. NONBLOCK prevents a malicious FIFO
	// from hanging open before its non-regular file mode can be rejected.
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		if err == nil {
			err = errors.New("media must be a regular file")
		}
		return nil, nil, err
	}
	return file, info, nil
}

func (s *Service) OpenVariant(ctx context.Context, current auth.Session, mediaID, variantID string) (*Content, error) {
	state, err := s.variantContentState(ctx, current, mediaID, variantID)
	if err != nil {
		return nil, err
	}
	// No filesystem I/O runs while holding SQLite's writer transaction.
	canonical, err := VariantPath(state.revision, state.preset, state.presetVersion, state.hash, state.contentType)
	if err != nil || state.storage != canonical || state.size < 1 || !filepath.IsAbs(s.cacheRoot) {
		return nil, ErrVariantUnavailable
	}
	if err := s.acquireContentVerification(ctx); err != nil {
		return nil, err
	}
	defer func() { <-s.contentVerifies }()
	if err := s.requireVariantContentState(ctx, current, mediaID, variantID, state); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(s.cacheRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrVariantUnavailable, err)
	}
	defer root.Close()
	rootInfo, err := root.Stat(".")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrVariantUnavailable, err)
	}
	file, info, err := OpenReadOnlyFile(root, state.storage)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrVariantUnavailable, err)
	}
	if info.Size() != state.size {
		file.Close()
		return nil, ErrVariantUnavailable
	}
	contentType := safeMIME(state.contentType)
	filename := strings.TrimSuffix(baseName(state.name), path.Ext(baseName(state.name))) + "." + mediaExtensions[contentType]
	content := &Content{MIME: contentType, Filename: filename, ETag: `"sha256-` + state.hash + `"`, Size: state.size, file: file, info: info, ctx: ctx}
	if err := content.verifyDigest(state.hash); err != nil {
		file.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %w", ErrVariantUnavailable, err)
	}
	if err := content.sameNamedFile(root, state.storage); err != nil {
		file.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %w", ErrVariantUnavailable, err)
	}
	rootNow, err := os.Stat(s.cacheRoot)
	if err != nil || !os.SameFile(rootInfo, rootNow) {
		file.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrVariantUnavailable
	}
	if err := s.requireVariantContentState(ctx, current, mediaID, variantID, state); err != nil {
		file.Close()
		return nil, err
	}
	if err := content.check(); err != nil {
		file.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %w", ErrVariantUnavailable, err)
	}
	return content, nil
}

func (s *Service) OpenOriginal(ctx context.Context, current auth.Session, mediaID string) (*Content, error) {
	state, err := s.originalContentState(ctx, current, mediaID)
	if err != nil {
		return nil, err
	}
	configured, ok := s.sources[state.sourceName]
	if !ok || configured.Path != state.sourcePath || state.rootSignature != configured.Signature() || state.sourceConfig != state.entryConfig || !filepath.IsAbs(configured.Path) || !localPath(configured.Marker) || !localPath(state.relative) || state.state != "healthy" || state.availability != "present" || state.mediaAvailability != "available" || state.size < 1 || state.size != state.entrySize || state.mtime != state.entryMtime || !validHash(state.hash) || state.hash != state.entryHash {
		return nil, ErrSourceUnavailable
	}
	if err := s.acquireContentVerification(ctx); err != nil {
		return nil, err
	}
	defer func() { <-s.contentVerifies }()
	if err := s.requireOriginalContentState(ctx, current, mediaID, state); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(configured.Path)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSourceUnavailable, err)
	}
	defer root.Close()
	rootInfo, err := root.Stat(".")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSourceUnavailable, err)
	}
	markerInfo, err := readContentMarker(ctx, root, configured.Marker)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %w", ErrSourceUnavailable, err)
	}
	file, info, err := OpenReadOnlyFile(root, state.relative)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSourceUnavailable, err)
	}
	if info.Size() != state.size || info.ModTime().UnixNano() != state.mtime {
		file.Close()
		return nil, ErrSourceUnavailable
	}
	content := &Content{MIME: originalMIME(state.contentType), Filename: baseName(state.name), ETag: `"sha256-` + state.hash + `"`, Size: state.size, Attachment: true, file: file, info: info, ctx: ctx}
	if err := content.verifyDigest(state.hash); err != nil {
		file.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %w", ErrSourceUnavailable, err)
	}
	if err := content.sameNamedFile(root, state.relative); err != nil {
		file.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %w", ErrSourceUnavailable, err)
	}
	rootNow, rootErr := os.Stat(configured.Path)
	markerNow, markerErr := readContentMarker(ctx, root, configured.Marker)
	if rootErr != nil || markerErr != nil || !os.SameFile(rootInfo, rootNow) || !sameContentInfo(markerInfo, markerNow) {
		file.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrSourceUnavailable
	}
	if err := s.requireOriginalContentState(ctx, current, mediaID, state); err != nil {
		file.Close()
		return nil, err
	}
	if err := content.check(); err != nil {
		file.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %w", ErrSourceUnavailable, err)
	}
	return content, nil
}
