package media

import (
	"context"
	"crypto/sha256"
	"database/sql"
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

	"gallerygo/internal/access"
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
// each read/seek abort an in-flight response instead of silently mixing revisions.
// Linux kernel filesystem stalls are not made cancellable by a context; real NAS
// behavior is a separate deployment gate, not a simulated guarantee.
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
	if !info.Mode().IsRegular() || info.Size() != content.info.Size() || !info.ModTime().Equal(content.info.ModTime()) {
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
	tx, viewer, err := s.readTx(ctx, current)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var storage, hash, contentType, name, revision, preset string
	var size, version int64
	err = tx.QueryRowContext(ctx, `SELECT v.storage_path,v.sha256,v.mime,v.size_bytes,m.file_name,v.revision_id,v.preset_key,v.preset_version
	 FROM media_variants v JOIN media_revisions r ON r.id=v.revision_id JOIN media m ON m.current_revision_id=r.id AND m.id=r.media_id
	 JOIN albums a ON a.id=m.album_id JOIN libraries l ON l.id=a.library_id
	 WHERE `+access.MediaVisible+` AND m.id=? AND v.id=? AND v.status='ready'`, viewer.ID, mediaID, variantID).Scan(&storage, &hash, &contentType, &size, &name, &revision, &preset, &version)
	if err != nil {
		return nil, err
	}
	if err := tx.Rollback(); err != nil {
		return nil, err
	}
	// No filesystem I/O runs while holding SQLite's writer transaction.
	canonical, err := VariantPath(revision, preset, version, hash, contentType)
	if err != nil || storage != canonical || size < 1 || !filepath.IsAbs(s.cacheRoot) {
		return nil, ErrVariantUnavailable
	}
	root, err := os.OpenRoot(s.cacheRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrVariantUnavailable, err)
	}
	defer root.Close()
	file, info, err := OpenReadOnlyFile(root, storage)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrVariantUnavailable, err)
	}
	if info.Size() != size {
		file.Close()
		return nil, ErrVariantUnavailable
	}
	contentType = safeMIME(contentType)
	filename := strings.TrimSuffix(baseName(name), path.Ext(baseName(name))) + "." + mediaExtensions[contentType]
	return &Content{MIME: contentType, Filename: filename, ETag: `"sha256-` + hash + `"`, Size: size, file: file, info: info, ctx: ctx}, nil
}

func (s *Service) OpenOriginal(ctx context.Context, current auth.Session, mediaID string) (*Content, error) {
	tx, viewer, err := s.readTx(ctx, current)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// The actual account policy applies even to administrators. A supplied user
	// DTO, an old scope, or a ready preview is not an original-download grant.
	if !viewer.DownloadOriginal {
		return nil, sql.ErrNoRows
	}
	var sourceName, sourcePath, rootSignature, state, availability, relative, hash, entryHash, contentType, name string
	var size, mtime, entrySize, entryMtime, sourceConfig, entryConfig int64
	err = tx.QueryRowContext(ctx, `SELECT src.name,src.root_path,src.root_config_sha256,src.config_version,e.source_config_version,src.state,e.availability,e.relative_path,r.source_sha256,COALESCE(e.sha256,''),
	 r.source_size,r.source_mtime_ns,e.size_bytes,e.mtime_ns,COALESCE(m.mime,''),m.file_name
	 FROM media m JOIN albums a ON a.id=m.album_id JOIN libraries l ON l.id=a.library_id
	 JOIN media_revisions r ON r.id=m.current_revision_id AND r.media_id=m.id
	 JOIN source_entries e ON e.id=m.source_entry_id AND e.source_id=a.source_id
	 JOIN sources src ON src.id=e.source_id AND src.library_id=l.id
	 WHERE `+access.MediaVisible+` AND m.id=?`, viewer.ID, mediaID).Scan(&sourceName, &sourcePath, &rootSignature, &sourceConfig, &entryConfig, &state, &availability, &relative, &hash, &entryHash, &size, &mtime, &entrySize, &entryMtime, &contentType, &name)
	if err != nil {
		return nil, err
	}
	if err := tx.Rollback(); err != nil {
		return nil, err
	}
	configured, ok := s.sources[sourceName]
	if !ok || configured.Path != sourcePath || rootSignature != configured.Signature() || sourceConfig != entryConfig || !filepath.IsAbs(configured.Path) || !localPath(configured.Marker) || !localPath(relative) || state != "healthy" || availability != "present" || size < 1 || size != entrySize || mtime != entryMtime || !validHash(hash) || hash != entryHash {
		return nil, ErrSourceUnavailable
	}
	root, err := os.OpenRoot(configured.Path)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSourceUnavailable, err)
	}
	defer root.Close()
	marker, _, err := OpenReadOnlyFile(root, configured.Marker)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSourceUnavailable, err)
	}
	_, err = marker.Read(make([]byte, 1))
	marker.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: %w", ErrSourceUnavailable, err)
	}
	file, info, err := OpenReadOnlyFile(root, relative)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSourceUnavailable, err)
	}
	if info.Size() != size || info.ModTime().UnixNano() != mtime {
		file.Close()
		return nil, ErrSourceUnavailable
	}
	if contentType = safeMIME(contentType); contentType == "" {
		contentType = "application/octet-stream"
	}
	return &Content{MIME: contentType, Filename: baseName(name), ETag: `"sha256-` + hash + `"`, Size: size, Attachment: true, file: file, info: info, ctx: ctx}, nil
}
