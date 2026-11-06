package access

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

var (
	ErrAlbumMetadata = errors.New("provide a display_name or cover_media_id override")
	ErrAlbumCover    = errors.New("select an eligible current album-owned cover media ID")
)

// Empty patch strings explicitly clear an override; nil means omitted/unchanged.
// HTTP null is deliberately not accepted, matching the strict JSON default.
type AlbumMetadataPatch struct {
	DisplayName  *string `json:"display_name"`
	CoverMediaID *string `json:"cover_media_id"`
}

type AlbumMetadata struct {
	ID              string  `json:"id"`
	SourceName      string  `json:"source_name"`
	DisplayName     *string `json:"display_name"`
	CoverMediaID    *string `json:"cover_media_id"`
	CoverOverrideID *string `json:"cover_override_id"`
	Version         int64   `json:"version"`
}

type albumMetadataState struct {
	value    AlbumMetadata
	rawCover sql.NullString
}

const albumMetadataSelect = `SELECT a.id,a.source_name,a.display_name,
 (SELECT m.id FROM media m WHERE m.id=a.cover_media_id AND m.album_id=a.id AND ` + MediaVisible + `),
 a.cover_override_id,a.version,a.cover_media_id
 FROM albums a JOIN libraries l ON l.id=a.library_id`

func scanAlbumMetadata(row interface{ Scan(...any) error }) (albumMetadataState, error) {
	var state albumMetadataState
	err := row.Scan(&state.value.ID, &state.value.SourceName, &state.value.DisplayName, &state.value.CoverMediaID,
		&state.value.CoverOverrideID, &state.value.Version, &state.rawCover)
	return state, err
}

func (s *Service) AdminAlbumMetadata(ctx context.Context, current auth.Session, id string) (AlbumMetadata, error) {
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return AlbumMetadata{}, err
	}
	defer tx.Rollback()
	state, err := scanAlbumMetadata(tx.QueryRowContext(ctx, albumMetadataSelect+` WHERE `+AlbumVisible+` AND a.id=?`, actor.ID, actor.ID, id))
	return state.value, err
}

func sameOverride(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func (s *Service) UpdateAlbumMetadata(ctx context.Context, current auth.Session, id string, version int64, patch AlbumMetadataPatch, key string) (AlbumMetadata, error) {
	if patch.DisplayName == nil && patch.CoverMediaID == nil {
		return AlbumMetadata{}, ErrAlbumMetadata
	}
	if patch.CoverMediaID != nil && len(*patch.CoverMediaID) > 64 {
		return AlbumMetadata{}, ErrAlbumCover
	}
	if patch.DisplayName != nil && *patch.DisplayName != "" {
		name, err := normalizedName(*patch.DisplayName)
		if err != nil {
			return AlbumMetadata{}, err
		}
		patch.DisplayName = &name
	}
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return AlbumMetadata{}, err
	}
	defer tx.Rollback()
	now := s.now().UTC().Truncate(time.Millisecond)
	data, _ := json.Marshal(struct {
		ID      string             `json:"id"`
		Version int64              `json:"version"`
		Patch   AlbumMetadataPatch `json:"patch"`
	}{id, version, patch})
	command := store.Command{Actor: actor.ID, Key: key, Operation: "album.metadata.update", Metadata: string(data)}
	var result AlbumMetadata
	if found, err := command.Replay(ctx, tx, now, &result); found || err != nil {
		return result, err
	}
	state, err := scanAlbumMetadata(tx.QueryRowContext(ctx, albumMetadataSelect+` WHERE `+AlbumVisible+` AND a.id=?`, actor.ID, actor.ID, id))
	if err != nil {
		return AlbumMetadata{}, err
	}
	if state.value.Version != version {
		return AlbumMetadata{}, auth.ErrConflict
	}
	display, desired, cover := state.value.DisplayName, state.value.CoverOverrideID, state.rawCover
	if patch.DisplayName != nil {
		display = nil
		if *patch.DisplayName != "" {
			display = patch.DisplayName
		}
	}
	if patch.CoverMediaID != nil {
		desired = nil
		if *patch.CoverMediaID != "" {
			var selected string
			if err := tx.QueryRowContext(ctx, `SELECT m.id FROM media m JOIN albums a ON a.id=m.album_id JOIN libraries l ON l.id=a.library_id
 WHERE `+MediaVisible+` AND m.album_id=? AND m.id=? AND COALESCE(json_extract(m.metadata_json,'$.status'),'unknown')!='not_media'`, actor.ID, id, *patch.CoverMediaID).Scan(&selected); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return AlbumMetadata{}, ErrAlbumCover
				}
				return AlbumMetadata{}, err
			}
			desired = &selected
		}
		// Preserve the scanner's sticky automatic-cover order when clearing a
		// desired override. No media is moved, generated, read or deleted here.
		err := tx.QueryRowContext(ctx, `SELECT m.id FROM media m JOIN albums a ON a.id=m.album_id JOIN libraries l ON l.id=a.library_id
 WHERE `+MediaVisible+` AND m.album_id=? AND COALESCE(json_extract(m.metadata_json,'$.status'),'unknown')!='not_media'
 ORDER BY CASE WHEN m.id=? THEN 0 WHEN m.id=? THEN 1 ELSE 2 END,m.id LIMIT 1`, actor.ID, id, desired, state.rawCover).Scan(&cover)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return AlbumMetadata{}, err
		}
		if errors.Is(err, sql.ErrNoRows) {
			cover = sql.NullString{}
		}
	}
	changed := !sameOverride(display, state.value.DisplayName) || !sameOverride(desired, state.value.CoverOverrideID) || cover != state.rawCover
	if changed {
		if version == math.MaxInt64 {
			return AlbumMetadata{}, auth.ErrConflict
		}
		if _, err := tx.ExecContext(ctx, `UPDATE albums SET display_name=?,cover_override_id=?,cover_media_id=?,version=version+1,updated_at=max(updated_at,?) WHERE id=?`, display, desired, cover, now.UnixMilli(), id); err != nil {
			return AlbumMetadata{}, err
		}
	}
	updated, err := scanAlbumMetadata(tx.QueryRowContext(ctx, albumMetadataSelect+` WHERE `+AlbumVisible+` AND a.id=?`, actor.ID, actor.ID, id))
	if err != nil {
		return AlbumMetadata{}, err
	}
	result = updated.value
	if changed {
		if err := JournalAlbumTx(ctx, tx, id, now); err != nil {
			return AlbumMetadata{}, err
		}
		if err := store.Audit(ctx, tx, actor.ID, "album_metadata_updated", "album", id, map[string]any{"display_name": result.DisplayName, "cover_override_id": result.CoverOverrideID, "cover_media_id": result.CoverMediaID, "version": result.Version}, now); err != nil {
			return AlbumMetadata{}, err
		}
	}
	if err := command.Save(ctx, tx, result, now); err != nil {
		return AlbumMetadata{}, err
	}
	return result, tx.Commit()
}

func nullableMetadataString(value sql.NullString) *string {
	if value.Valid {
		return &value.String
	}
	return nil
}
