// Package media reads authorized metadata/content and confines private worker
// artifacts. It never writes source files or exposes a filesystem mount;
// generation orchestration remains a separate worker concern.
package media

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/url"
	"path"
	"strings"
	"time"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
)

var (
	ErrVariantUnavailable = errors.New("the ready variant is unavailable or inconsistent")
	ErrSourceUnavailable  = errors.New("the configured source is unavailable or has changed")
	ErrContentChanged     = errors.New("media content changed while it was being read")
)

type SourceRoot struct {
	Path   string `json:"-"`
	Marker string `json:"-"`
}

// Signature binds worker configuration to the canonical path and mount marker.
// The digest is stored privately; neither it nor its inputs are public DTO fields.
func (root SourceRoot) Signature() string {
	data, _ := json.Marshal([2]string{root.Path, root.Marker})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

type Service struct {
	db        *sql.DB
	auth      *auth.Service
	cacheRoot string
	sources   map[string]SourceRoot
}

func New(db *sql.DB, authentication *auth.Service, cacheRoot string, sources map[string]SourceRoot) *Service {
	return &Service{db: db, auth: authentication, cacheRoot: cacheRoot, sources: maps.Clone(sources)}
}

func (s *Service) readTx(ctx context.Context, current auth.Session) (*sql.Tx, auth.User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, auth.User{}, err
	}
	viewer, err := s.auth.SessionInTx(ctx, tx, current)
	if err != nil {
		tx.Rollback()
		return nil, auth.User{}, err
	}
	return tx, viewer.User, nil
}

type Metadata struct {
	ID             string     `json:"id"`
	AlbumID        string     `json:"album_id"`
	Kind           string     `json:"kind"`
	Codec          *string    `json:"codec"`
	FileName       string     `json:"file_name"`
	Width          *int64     `json:"width"`
	Height         *int64     `json:"height"`
	DurationMS     *int64     `json:"duration_ms"`
	Animated       *bool      `json:"animated"`
	FrameCount     *int64     `json:"frame_count"`
	LoopCount      *int64     `json:"loop_count"`
	Orientation    *int64     `json:"orientation"`
	CaptureAt      *time.Time `json:"capture_at"`
	Revision       *string    `json:"revision"`
	OfflineCapable bool       `json:"offline_capable"`
}

const mediaSelect = `SELECT m.id,m.album_id,m.kind,m.file_name,m.width,m.height,m.duration_ms,r.id,m.orientation,m.capture_at,
 json_extract(m.metadata_json,'$.animated'),json_extract(m.metadata_json,'$.frame_count'),json_extract(m.metadata_json,'$.loop_count'),json_extract(m.metadata_json,'$.codec')
 FROM media m JOIN albums a ON a.id=m.album_id JOIN libraries l ON l.id=a.library_id
 LEFT JOIN media_revisions r ON r.id=m.current_revision_id AND r.media_id=m.id`

func scanMedia(row interface{ Scan(...any) error }) (Metadata, error) {
	var value Metadata
	var captured sql.NullInt64
	err := row.Scan(&value.ID, &value.AlbumID, &value.Kind, &value.FileName, &value.Width, &value.Height, &value.DurationMS, &value.Revision, &value.Orientation, &captured, &value.Animated, &value.FrameCount, &value.LoopCount, &value.Codec)
	if captured.Valid {
		instant := time.UnixMilli(captured.Int64).UTC()
		value.CaptureAt = &instant
	}
	value.FileName = baseName(value.FileName)
	if value.Codec != nil && !ValidCodec(*value.Codec) {
		value.Codec = nil
	}
	// Offline capability stays false until scoped download manifests are enabled.
	return value, err
}

func baseName(name string) string { return path.Base(strings.ReplaceAll(name, `\`, "/")) }

func (s *Service) Get(ctx context.Context, current auth.Session, id string) (Metadata, error) {
	tx, viewer, err := s.readTx(ctx, current)
	if err != nil {
		return Metadata{}, err
	}
	defer tx.Rollback()
	return scanMedia(tx.QueryRowContext(ctx, mediaSelect+` WHERE `+access.MediaVisible+` AND m.id=?`, viewer.ID, id))
}

func (s *Service) AlbumMedia(ctx context.Context, current auth.Session, albumID, after string, limit int) ([]Metadata, error) {
	if limit < 1 || limit > 101 {
		return nil, access.ErrGroupSet
	}
	tx, viewer, err := s.readTx(ctx, current)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT a.id FROM albums a JOIN libraries l ON l.id=a.library_id WHERE `+access.AlbumVisible+` AND a.id=?`, viewer.ID, albumID).Scan(&id); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, mediaSelect+` WHERE `+access.MediaVisible+` AND m.album_id=? AND m.id>? ORDER BY m.id LIMIT ?`, viewer.ID, albumID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Metadata, 0)
	for rows.Next() {
		item, err := scanMedia(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

type Variant struct {
	ID            string `json:"id"`
	RevisionID    string `json:"revision_id"`
	PresetKey     string `json:"preset_key"`
	PresetVersion int64  `json:"preset_version"`
	MIME          string `json:"mime"`
	SizeBytes     int64  `json:"size_bytes"`
	SHA256        string `json:"sha256"`
	Width         *int64 `json:"width"`
	Height        *int64 `json:"height"`
	DurationMS    *int64 `json:"duration_ms"`
	ContentURL    string `json:"content_url"`
}

func (s *Service) Variants(ctx context.Context, current auth.Session, mediaID, after string, limit int) ([]Variant, error) {
	if limit < 1 || limit > 101 {
		return nil, access.ErrGroupSet
	}
	tx, viewer, err := s.readTx(ctx, current)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT m.id FROM media m JOIN albums a ON a.id=m.album_id JOIN libraries l ON l.id=a.library_id WHERE `+access.MediaVisible+` AND m.id=?`, viewer.ID, mediaID).Scan(&id); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT v.id,v.revision_id,v.preset_key,v.preset_version,v.mime,v.size_bytes,v.sha256,v.width,v.height,v.duration_ms
	 FROM media_variants v JOIN media_revisions r ON r.id=v.revision_id JOIN media m ON m.current_revision_id=r.id AND m.id=r.media_id
	 WHERE m.id=? AND v.status='ready' AND v.id>? AND NOT EXISTS (
	 SELECT 1 FROM media_variants newer WHERE newer.revision_id=v.revision_id AND newer.preset_key=v.preset_key AND newer.status='ready' AND newer.preset_version>v.preset_version)
	 ORDER BY v.id LIMIT ?`, mediaID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Variant, 0)
	for rows.Next() {
		var item Variant
		if err := rows.Scan(&item.ID, &item.RevisionID, &item.PresetKey, &item.PresetVersion, &item.MIME, &item.SizeBytes, &item.SHA256, &item.Width, &item.Height, &item.DurationMS); err != nil {
			return nil, err
		}
		item.MIME = safeMIME(item.MIME)
		if item.MIME == "" || !validHash(item.SHA256) || item.SizeBytes < 1 || item.PresetVersion < 1 {
			return nil, ErrVariantUnavailable
		}
		item.ContentURL = "/api/v2/media/" + url.PathEscape(mediaID) + "/variants/" + url.PathEscape(item.ID) + "/content"
		items = append(items, item)
	}
	return items, rows.Err()
}
